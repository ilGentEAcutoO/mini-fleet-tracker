package middleware

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
)

// ErrWildcardWithCredentials is returned by CORS when the operator supplies
// a wildcard origin. Browsers reject AllowCredentials: true paired with
// AllowOrigin: "*" — failing fast at boot is friendlier than discovering
// the misconfiguration at runtime on the first cross-origin fetch.
var ErrWildcardWithCredentials = errors.New(
	"middleware.CORS: AllowOrigins=\"*\" cannot be combined with credentials-true CORS; supply an explicit scheme://host[:port]",
)

// ErrEmptyAllowOrigin is returned by CORS when an empty allow-origin is
// supplied. Misconfigured CORS_ORIGIN is a common deploy footgun; refusing
// to start avoids silently shipping a broken auth flow.
var ErrEmptyAllowOrigin = errors.New(
	"middleware.CORS: allowOrigin must be a non-empty scheme://host[:port]",
)

// CORS returns a Fiber CORS middleware tuned for the Mini Fleet Tracker
// browser app, which uses cookie-based auth and therefore needs
// AllowCredentials: true. The accepted origin is the single value passed
// in (typically config.Config.CORSOrigin), e.g.:
//
//	http://localhost:3000          // dev
//	https://fleet-tracker.jairukchan.com   // prod
//
// Wildcards are rejected because the browser CORS spec forbids them when
// credentials are sent. The configured ExposeHeaders includes
// X-Request-Id so the SPA can surface the correlation ID to operators.
//
// Returns an error rather than panicking so the caller's main() can log a
// structured failure and exit non-zero.
func CORS(allowOrigin string) (fiber.Handler, error) {
	origin := strings.TrimSpace(allowOrigin)
	if origin == "" {
		return nil, ErrEmptyAllowOrigin
	}
	if origin == "*" {
		return nil, ErrWildcardWithCredentials
	}

	return cors.New(cors.Config{
		AllowOrigins:     origin,
		AllowCredentials: true,
		AllowMethods:     "GET,POST,PUT,PATCH,DELETE,OPTIONS",
		AllowHeaders:     "Content-Type,X-CSRF-Token,X-Request-Id",
		ExposeHeaders:    "X-Request-Id",
		MaxAge:           600,
	}), nil
}
