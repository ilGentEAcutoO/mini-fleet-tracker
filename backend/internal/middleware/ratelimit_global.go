package middleware

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
)

// Global cost-protection rate limits. These are the project-wide umbrella
// that applies to every request on the API, regardless of route. They
// exist to bound runaway cost on Cloudflare D1, KV, and Container CPU
// when the public demo is exposed at fleet-tracker.jairukchan.com — a
// single rogue IP cannot drain the free-tier ceilings in an afternoon.
//
// Two layered windows per IP:
//   - 600 requests per minute (per-minute counter, TTL ~ 2 minutes)
//   - 10,000 requests per day (per-day counter, TTL ~ 25 hours)
//
// Both counters are incremented on every request; either breach yields
// a 429 with Retry-After: 60. The exact Retry-After is intentionally
// the same for both flavours — for the daily breach the SPA should
// surface "too many requests today" via the JSON body's message field
// rather than via the header, which the browser uses to gate auto-retry.
//
// WebSocket upgrade caps (3/min) and healthz caps (60/min) are OUT of
// scope for this global middleware — they are applied as per-route
// limits via NewPerIP with route-specific buckets. Layering them outside
// the global umbrella keeps the global state-machine simple (just two
// fixed-window counters) and lets per-route caps tighten the global cap
// without loosening it.
const (
	// GlobalPerMinuteLimit is the per-IP request ceiling within a single
	// rolling UTC minute. The value is the cost-protection design's
	// 600/min from the project plan.
	GlobalPerMinuteLimit = 600

	// GlobalPerDayLimit is the per-IP request ceiling within a single
	// UTC day. The value is the cost-protection design's 10,000/day.
	GlobalPerDayLimit = 10_000

	// globalRetryAfterSeconds is the Retry-After value sent on any global
	// breach. Pegged at the per-minute window length (60s) because that's
	// the longest wait that does not violate the spec — Cloudflare KV's
	// 60s minimum TTL also matches.
	globalRetryAfterSeconds = 60

	// globalMinuteKeyPrefix and globalDayKeyPrefix scope the keys so the
	// global limiter cannot collide with per-route limiters that share
	// the same RateLimiterStorage namespace.
	globalMinuteKeyPrefix = "rl-global-min"
	globalDayKeyPrefix    = "rl-global-day"

	// globalMinuteTTL gives a small grace window beyond the counter's
	// nominal lifetime so a request landing right at the rollover sees
	// either the old or the new counter, never neither.
	globalMinuteTTL = 2 * time.Minute

	// globalDayTTL is the day counter's grace window. KV minimum is 60s,
	// so any value >= 24h is safe; we use 25h to be obvious.
	globalDayTTL = 25 * time.Hour
)

// GlobalConfig is the constructor input for NewGlobal.
type GlobalConfig struct {
	Storage RateLimiterStorage
	// Now is optional; tests inject a fake clock for deterministic
	// window rollover. Production code leaves it nil so time.Now is used.
	Now func() time.Time
}

// NewGlobal returns the cost-protection middleware that applies the
// project-wide per-IP rate limits to every request. Errors only when
// cfg is invalid (nil storage); production wiring fails fast at boot.
//
// On a breach, returns 429 with Retry-After: 60 and a JSON body. On
// success, calls c.Next().
//
// Storage failures fail OPEN: the alternative — denying every request
// on a transient KV outage — would make the API less available than
// the underlying KV namespace. Operators see the error logged by the
// storage adapter.
func NewGlobal(cfg GlobalConfig) (fiber.Handler, error) {
	if cfg.Storage == nil {
		return nil, errors.New("rate-limit global: storage is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	return func(c *fiber.Ctx) error {
		ip := strings.TrimSpace(c.IP())
		if ip == "" {
			ip = "unknown"
		}
		nowT := now().UTC()

		// Per-minute window key: bucket the timestamp into minute slots
		// so the same IP keeps incrementing the same key for ~60s, then
		// rolls over to a fresh key whose initial value is implicitly 0.
		minuteSlot := nowT.Format("2006-01-02T15:04")
		minuteKey := globalMinuteKeyPrefix + ":" + ip + ":" + minuteSlot

		// Per-day window key: same idea but in date slots.
		daySlot := nowT.Format("2006-01-02")
		dayKey := globalDayKeyPrefix + ":" + ip + ":" + daySlot

		// Read current counters. Storage errors fail open (see comment
		// above); a missing key reads as count=0 which is the correct
		// initial state for a brand-new window.
		minuteCount := readCounter(c.UserContext(), cfg.Storage, minuteKey)
		dayCount := readCounter(c.UserContext(), cfg.Storage, dayKey)

		// Check BEFORE incrementing so the cap is inclusive: the
		// (GlobalPerMinuteLimit+1)th request in a minute is the first
		// to be denied.
		if minuteCount >= GlobalPerMinuteLimit {
			return respondTooMany(c, globalRetryAfterSeconds, "global per-minute rate limit exceeded")
		}
		if dayCount >= GlobalPerDayLimit {
			return respondTooMany(c, globalRetryAfterSeconds, "global per-day rate limit exceeded")
		}

		// Increment both counters. We do not chain the two Write calls
		// in a transaction — KV does not support that and the worst case
		// (one counter advances while the other does not) skews counts by
		// at most one per affected request.
		_ = writeCounter(c.UserContext(), cfg.Storage, minuteKey, minuteCount+1, globalMinuteTTL)
		_ = writeCounter(c.UserContext(), cfg.Storage, dayKey, dayCount+1, globalDayTTL)

		return c.Next()
	}, nil
}

// readCounter pulls a uint64 counter from storage. Absent keys read as 0;
// corrupt bodies also read as 0 (the next write will overwrite). Errors
// fail open by returning 0 — the caller's increment proceeds as if this
// were a brand-new window.
func readCounter(ctx context.Context, storage RateLimiterStorage, key string) int64 {
	raw, _, err := storage.Read(ctx, key)
	if err != nil || len(raw) == 0 {
		return 0
	}
	// Counters are stored as a base-10 ASCII integer so the KV value
	// preview shows a useful "37" rather than a JSON blob.
	n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// writeCounter persists a counter value back to storage. TTL is set so
// the entry self-cleans after the window closes.
func writeCounter(ctx context.Context, storage RateLimiterStorage, key string, value int64, ttl time.Duration) error {
	body := []byte(strconv.FormatInt(value, 10))
	return storage.Write(ctx, key, body, ttl)
}
