package middleware

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
)

// fakeClock returns a now func anchored at the given timestamp so the
// middleware compares against a deterministic moment regardless of when
// the test runs.
func fakeClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// stubNext writes 200 OK + a known body so we can assert that c.Next()
// actually ran (i.e. the request was NOT short-circuited).
func stubNext(c *fiber.Ctx) error {
	return c.Status(fiber.StatusOK).SendString("downstream-ok")
}

// TestDemoExpiry covers the time-windowed branches plus the body shape
// guarantee. Each subtest is independent and constructs its own app so
// failures don't cascade.
func TestDemoExpiry(t *testing.T) {
	// All cases pin against a fixed expiration so we can reason about
	// "before" / "at" / "after" without wall-clock drift.
	expires := time.Date(2026, time.May, 31, 16, 59, 59, 0, time.UTC) // == 2026-05-31T23:59:59+07:00

	cases := []struct {
		name       string
		now        time.Time
		path       string
		wantStatus int
		// wantBody is non-empty when we expect the 410 envelope.
		wantBody bool
	}{
		{
			name:       "before expiration → downstream runs",
			now:        expires.Add(-time.Hour),
			path:       "/api/auth/me",
			wantStatus: fiber.StatusOK,
			wantBody:   false,
		},
		{
			name:       "exactly at expiration → downstream runs (After returns false at equality)",
			now:        expires,
			path:       "/api/auth/me",
			wantStatus: fiber.StatusOK,
			wantBody:   false,
		},
		{
			name:       "one second past expiration → 410 Gone",
			now:        expires.Add(time.Second),
			path:       "/api/auth/me",
			wantStatus: fiber.StatusGone,
			wantBody:   true,
		},
		{
			name:       "one day past expiration → 410 Gone",
			now:        expires.Add(24 * time.Hour),
			path:       "/api/vehicles",
			wantStatus: fiber.StatusGone,
			wantBody:   true,
		},
		{
			name:       "/healthz past expiration → bypass, downstream runs",
			now:        expires.Add(24 * time.Hour),
			path:       "/healthz",
			wantStatus: fiber.StatusOK,
			wantBody:   false,
		},
		{
			name:       "/healthz before expiration → bypass, downstream runs",
			now:        expires.Add(-time.Hour),
			path:       "/healthz",
			wantStatus: fiber.StatusOK,
			wantBody:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := newTestApp()
			app.Use(NewDemoExpiry(ExpiryConfig{
				ExpiresAt: expires,
				Now:       fakeClock(tc.now),
			}))
			// Same downstream handler for every path under test — the
			// middleware decides whether it runs.
			app.Get("/api/auth/me", stubNext)
			app.Get("/api/vehicles", stubNext)
			app.Get("/healthz", stubNext)

			req := httptest.NewRequest("GET", tc.path, nil)
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}

			body, readErr := io.ReadAll(resp.Body)
			if readErr != nil {
				t.Fatalf("read body: %v", readErr)
			}

			if !tc.wantBody {
				// Downstream stub writes a known string — verifies that the
				// middleware called c.Next() rather than returning early.
				if string(body) != "downstream-ok" {
					t.Errorf("expected downstream body, got %q", string(body))
				}
				return
			}

			var got map[string]any
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("unmarshal 410 body: %v (raw: %s)", err, body)
			}
			if got["error"] != "demo_expired" {
				t.Errorf("error field = %v, want \"demo_expired\"", got["error"])
			}
			// expired_at must be a string that parses as RFC3339.
			expRaw, ok := got["expired_at"].(string)
			if !ok || expRaw == "" {
				t.Errorf("expired_at missing or wrong type: %v", got["expired_at"])
			} else if _, err := time.Parse(time.RFC3339, expRaw); err != nil {
				t.Errorf("expired_at not RFC3339: %q (%v)", expRaw, err)
			}
			repoURL, ok := got["repo_url"].(string)
			if !ok || repoURL == "" {
				t.Errorf("repo_url missing or empty: %v", got["repo_url"])
			}
			msg, ok := got["message"].(string)
			if !ok || msg == "" {
				t.Errorf("message missing or empty: %v", got["message"])
			}
		})
	}
}

// TestDemoExpiry_DefaultRepoURL covers the empty-string → canonical-URL
// fallback so the constructor's nil-defaults stay regression-guarded.
func TestDemoExpiry_DefaultRepoURL(t *testing.T) {
	expires := time.Date(2026, time.May, 31, 16, 59, 59, 0, time.UTC)
	app := newTestApp()
	app.Use(NewDemoExpiry(ExpiryConfig{
		ExpiresAt: expires,
		// RepoURL deliberately empty — should fall back to the const.
		Now: fakeClock(expires.Add(time.Hour)),
	}))
	app.Get("/api/anything", stubNext)

	req := httptest.NewRequest("GET", "/api/anything", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusGone {
		t.Fatalf("status = %d, want 410", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	repoURL, _ := got["repo_url"].(string)
	const want = "https://github.com/ilGentEAcutoO/mini-fleet-tracker"
	if repoURL != want {
		t.Errorf("repo_url = %q, want %q", repoURL, want)
	}
}

// TestDemoExpiry_NilNowFallsBackToTimeNow exercises the cfg.Now == nil
// branch (production wiring leaves it unset). With a far-future
// expiration the wall clock can't have moved past it, so c.Next() must
// run.
func TestDemoExpiry_NilNowFallsBackToTimeNow(t *testing.T) {
	future := time.Now().Add(100 * 365 * 24 * time.Hour) // ~100 years out
	app := newTestApp()
	app.Use(NewDemoExpiry(ExpiryConfig{
		ExpiresAt: future,
		// Now intentionally nil — should default to time.Now.
	}))
	app.Get("/api/x", stubNext)

	req := httptest.NewRequest("GET", "/api/x", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		t.Errorf("status = %d, want 200 (now() < far-future expiry)", resp.StatusCode)
	}
}
