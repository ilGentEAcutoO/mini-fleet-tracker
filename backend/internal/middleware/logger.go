package middleware

import (
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Logger returns a Fiber middleware that derives a per-request zerolog
// logger from the global one, tags it with the request_id resolved by
// RequestID, and stores it on c.UserContext() via zerolog's WithContext
// helper. Handlers and downstream middleware should retrieve the logger
// with FromCtx so every log line in a request shares the same request_id.
//
// Ordering note: this middleware reads c.Locals(RequestIDLocalsKey) and
// therefore MUST be registered after middleware.RequestID. Registering it
// first is not a hard error — the request_id field would simply be empty —
// but downstream filtering and tracing breaks, so we keep this strict in
// the wiring layer.
func Logger() fiber.Handler {
	return func(c *fiber.Ctx) error {
		reqID := RequestIDFromCtx(c)

		// Derive a logger from the global one. zerolog loggers are value
		// types with copy-on-write context, so this is allocation-light
		// and safe to do per request.
		l := log.Logger.With().
			Str("request_id", reqID).
			Logger()

		ctx := l.WithContext(c.UserContext())
		c.SetUserContext(ctx)

		return c.Next()
	}
}

// FromCtx returns the request-scoped logger attached by Logger. If Logger
// has not run for this request (typical in unit tests that exercise a
// handler in isolation), the global zerolog logger is returned so callers
// never have to nil-check.
func FromCtx(c *fiber.Ctx) *zerolog.Logger {
	return zerolog.Ctx(c.UserContext())
}
