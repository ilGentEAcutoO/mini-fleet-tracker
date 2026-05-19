// Package main runs a driver simulator that logs in as a driver and posts
// random-walk GPS positions to /api/positions on an interval. Used to
// drive the live demo dashboard without a real device.
//
// Example:
//
//	make sim ARGS="--email driver@demo.local --password secret123 \
//	  --vehicle-id 11111111-1111-1111-1111-111111111111 --interval 2s"
//
// The binary:
//  1. Logs in via POST /api/auth/login, capturing auth_token + csrf_token
//     cookies in a per-process jar.
//  2. Loops on the configured interval, POSTing /api/positions with a
//     Bangkok-seeded random walk (lat 13.7563, lng 100.5018, ~55m step).
//  3. Echoes the csrf_token cookie value back as X-CSRF-Token on every
//     mutating request, satisfying the double-submit CSRF check.
//  4. Exits cleanly on Ctrl-C / SIGTERM via signal.NotifyContext.
//
// Output is human-readable zerolog ConsoleWriter, suitable for live
// demo recording. The password is masked in the startup log line.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// config holds the parsed CLI flags. All fields are validated in
// parseFlags before main() touches the network.
type config struct {
	Email     string
	Password  string
	VehicleID string
	BaseURL   string // e.g. http://localhost:8080
	Interval  time.Duration
	SpeedKmh  float64 // optional simulated speed reported in each post
}

// parseFlags reads the CLI flags into a config and validates them.
// Returns an error rather than calling log.Fatal so tests can exercise
// the validation paths without exiting the process.
func parseFlags(args []string) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("sim", flag.ContinueOnError)
	fs.StringVar(&cfg.Email, "email", "", "driver email (required)")
	fs.StringVar(&cfg.Password, "password", "", "driver password (required)")
	fs.StringVar(&cfg.VehicleID, "vehicle-id", "", "vehicle UUID to post positions for (required)")
	fs.StringVar(&cfg.BaseURL, "base-url", "http://localhost:8080", "API base URL")
	fs.DurationVar(&cfg.Interval, "interval", 2*time.Second, "interval between position posts")
	fs.Float64Var(&cfg.SpeedKmh, "speed", 35.0, "simulated speed in km/h (reported, not enforced)")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if cfg.Email == "" || cfg.Password == "" || cfg.VehicleID == "" {
		return cfg, errors.New("--email, --password, and --vehicle-id are required")
	}
	if cfg.Interval < time.Second {
		// The server rate-limits position writes at 60/min/driver, so
		// any interval below 1s would self-DOS within seconds.
		return cfg, errors.New("--interval must be >= 1s (server rate-limits at 60/min/driver)")
	}
	if err := validateBaseURL(cfg.BaseURL); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// validateBaseURL rejects empty or otherwise unusable URLs early so the
// first request doesn't bail with an inscrutable transport error.
func validateBaseURL(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("--base-url must not be empty")
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("--base-url parse: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("--base-url scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("--base-url must include a host, got %q", s)
	}
	return nil
}

// login posts credentials and returns an http.Client whose cookie jar
// holds auth_token + csrf_token. The csrf_token value is also returned
// so callers can echo it as X-CSRF-Token on subsequent mutating calls
// (the server's double-submit pattern requires the header to match the
// cookie). Subsequent calls reuse the same client/jar to ride the
// auth_token cookie.
func login(ctx context.Context, baseURL, email, password string) (*http.Client, string, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		// cookiejar.New only returns nil-error today, but guard anyway
		// so a future stdlib change can't silently break us.
		return nil, "", fmt.Errorf("cookie jar: %w", err)
	}
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}

	body, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		return nil, "", fmt.Errorf("marshal login body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/auth/login", bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("build login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("login transport: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, "", fmt.Errorf("login failed: %d %s", res.StatusCode, strings.TrimSpace(string(b)))
	}

	// Extract csrf_token from the jar so we can echo it on the
	// X-CSRF-Token header for mutating requests. The auth_token cookie
	// stays in the jar and the client sends it automatically.
	u, err := url.Parse(baseURL)
	if err != nil {
		// Unreachable: validateBaseURL parsed the same string at
		// startup; keep the guard for defensive correctness.
		return nil, "", fmt.Errorf("parse baseURL for jar lookup: %w", err)
	}
	var csrf string
	for _, c := range jar.Cookies(u) {
		if c.Name == "csrf_token" {
			csrf = c.Value
			break
		}
	}
	if csrf == "" {
		return nil, "", errors.New("login succeeded but csrf_token cookie missing")
	}
	return client, csrf, nil
}

// postPosition POSTs a single position. Returns an error on any
// non-201 response with up to 256 bytes of the server's body for
// diagnostics. The caller is responsible for retry/backoff policy —
// the simulator simply logs and continues on failure to keep the demo
// flowing.
func postPosition(ctx context.Context, client *http.Client, baseURL, csrf, vehicleID string, lat, lng, speed float64) error {
	body, err := json.Marshal(map[string]any{
		"vehicle_id":  vehicleID,
		"lat":         lat,
		"lng":         lng,
		"speed_kmh":   speed,
		"recorded_at": time.Now().UnixMilli(),
	})
	if err != nil {
		return fmt.Errorf("marshal position body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/positions", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build position request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)

	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post position transport: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 256))
		return fmt.Errorf("post position: %d %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// stepDeg is the maximum per-tick walk in degrees of lat/lng. At
// Bangkok's latitude (~13.76 N) one degree of lat is ~111 km and one
// degree of lng is ~108 km, so 0.0005 ≈ 55 metres per axis — small
// enough to look like a real vehicle creeping through traffic at 1-2 Hz
// update rates.
const stepDeg = 0.0005

// randomWalk produces the next lat/lng pair by adding a uniform random
// delta in [-stepDeg, +stepDeg] to each axis. The walk is unbounded so
// over long runs the marker will drift; for demo recording lengths
// (minutes, not hours) the drift is invisible against the Bangkok base
// map. Kept pure for testing — no time.Now or global RNG state besides
// math/rand/v2's auto-seeded default.
func randomWalk(lat, lng float64) (float64, float64) {
	dlat := (rand.Float64() - 0.5) * 2 * stepDeg
	dlng := (rand.Float64() - 0.5) * 2 * stepDeg
	return lat + dlat, lng + dlng
}

// maskPassword reduces a password to its first + last char with the
// middle replaced by asterisks. Short passwords (<4 chars) become a
// full asterisk run so the length is still hidden somewhat.
func maskPassword(s string) string {
	if len(s) < 4 {
		return strings.Repeat("*", len(s))
	}
	return s[:1] + strings.Repeat("*", len(s)-2) + s[len(s)-1:]
}

// configureLogger wires zerolog's ConsoleWriter to stderr with RFC3339
// timestamps — the simulator is a human-facing tool used during live
// demos, so pretty output matters more than machine parseability.
func configureLogger() {
	log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}).With().Timestamp().Logger()
}

// run is the body of main, factored out so an integration test could
// drive the full lifecycle against a test server. Returns the first
// fatal error encountered (login failure, context cancel error) so
// main can choose its exit code.
func run(ctx context.Context, cfg config) error {
	log.Info().
		Str("email", cfg.Email).
		Str("password", maskPassword(cfg.Password)).
		Str("vehicle_id", cfg.VehicleID).
		Str("base_url", cfg.BaseURL).
		Dur("interval", cfg.Interval).
		Float64("speed_kmh", cfg.SpeedKmh).
		Msg("starting simulator")

	client, csrf, err := login(ctx, cfg.BaseURL, cfg.Email, cfg.Password)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	log.Info().Msg("login succeeded; entering position loop")

	// Bangkok seed — central area, visible on the default map zoom.
	lat, lng := 13.7563, 100.5018

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	// Post one position immediately so the dashboard sees us before
	// the first ticker firing.
	postWithLog(ctx, client, cfg, csrf, lat, lng)

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("shutdown signal received; exiting")
			return nil
		case <-ticker.C:
			lat, lng = randomWalk(lat, lng)
			postWithLog(ctx, client, cfg, csrf, lat, lng)
		}
	}
}

// postWithLog wraps postPosition + structured logging. Failures are
// logged at error level and skipped — the demo loop should survive a
// transient 5xx or rate-limit blip without crashing.
func postWithLog(ctx context.Context, client *http.Client, cfg config, csrf string, lat, lng float64) {
	if err := postPosition(ctx, client, cfg.BaseURL, csrf, cfg.VehicleID, lat, lng, cfg.SpeedKmh); err != nil {
		// Context cancellation is the normal shutdown path; downgrade
		// it from error to debug so Ctrl-C produces a clean log.
		if errors.Is(err, context.Canceled) {
			log.Debug().Err(err).Msg("position post cancelled by shutdown")
			return
		}
		log.Error().Err(err).Float64("lat", lat).Float64("lng", lng).Msg("position post failed")
		return
	}
	log.Info().Float64("lat", lat).Float64("lng", lng).Msg("posted")
}

func main() {
	configureLogger()

	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		log.Fatal().Err(err).Msg("flag parse failed")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg); err != nil {
		log.Fatal().Err(err).Msg("simulator exited with error")
	}
}
