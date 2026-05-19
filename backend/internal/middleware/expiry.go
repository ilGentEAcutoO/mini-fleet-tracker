// Package middleware — Demo expiration middleware.
//
// Renders a 410 Gone response with the demo_expired error envelope when
// the build's compile-time DemoExpiresAt has passed. The /healthz route
// is exempt (operators must still be able to read the per-dep status
// signal even after expiration). All other routes are short-circuited
// before any handler runs — saving handler + DB / KV work.
package middleware

import (
	"time"

	"github.com/gofiber/fiber/v2"
)

// ExpiryConfig is the constructor input for NewDemoExpiry.
type ExpiryConfig struct {
	// ExpiresAt is the moment after which the API returns 410. Pass the
	// parsed const from cmd/api/main.go — this middleware doesn't parse
	// strings itself so callers cannot smuggle a typo past the boot
	// validation that already runs.
	ExpiresAt time.Time

	// RepoURL is included in the 410 body so curl-only consumers see
	// where the source still lives. Defaults to the canonical GitHub URL
	// when empty.
	RepoURL string

	// Now lets tests inject a clock. Defaults to time.Now.
	Now func() time.Time
}

// NewDemoExpiry returns a middleware that short-circuits to 410 Gone
// after the configured expiration. The /healthz route is exempted via
// c.Path() prefix match — registering after this middleware would
// otherwise blackhole liveness checks.
//
// Boundary semantics: at the exact expiration instant we treat the
// window as still open (time.After returns false at equality). The
// minimum failure granularity is "one nanosecond past expiry", which is
// fine for a date-based cutoff where the relevant transition is the
// day rollover, not the nanosecond.
func NewDemoExpiry(cfg ExpiryConfig) fiber.Handler {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	repoURL := cfg.RepoURL
	if repoURL == "" {
		repoURL = "https://github.com/ilGentEAcutoO/mini-fleet-tracker"
	}
	expiresAt := cfg.ExpiresAt
	expiresAtRFC := expiresAt.Format(time.RFC3339)

	return func(c *fiber.Ctx) error {
		// Bypass for /healthz so monitors keep reading per-dep status.
		if c.Path() == "/healthz" {
			return c.Next()
		}
		if !now().After(expiresAt) {
			return c.Next()
		}
		return c.Status(fiber.StatusGone).JSON(fiber.Map{
			"error":      "demo_expired",
			"message":    "This demo expired on " + expiresAtRFC + ". See the repo for the source.",
			"repo_url":   repoURL,
			"expired_at": expiresAtRFC,
		})
	}
}
