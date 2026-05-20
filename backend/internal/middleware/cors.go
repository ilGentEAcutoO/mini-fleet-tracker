package middleware

import (
	"errors"
	"fmt"
	"net/url"
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

// ErrInvalidAllowOrigin is returned by CORS when the operator supplies an
// origin that does not parse to a URL with BOTH a scheme and a host. Fiber's
// CORS middleware treats unknown patterns as deny-all (the safe direction),
// so a misconfigured value like "*.example.com" or "http://" would silently
// produce a CORS-rejected SPA instead of an obvious deploy-time error. This
// guard surfaces the misconfiguration at boot — see security review M3.
var ErrInvalidAllowOrigin = errors.New(
	"middleware.CORS: allowOrigin must be a valid scheme://host[:port] URL",
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
// Each origin is parsed by net/url at startup; anything missing a scheme or
// a host is rejected with ErrInvalidAllowOrigin (security review M3 — so a
// typoed CORS_ORIGIN surfaces at boot instead of as a silent deny-all at
// the first cross-origin request).
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

	// Fiber accepts a comma-separated list in AllowOrigins; we validate
	// each entry independently so a single malformed entry surfaces
	// clearly rather than corrupting the whole list silently.
	parts := strings.Split(origin, ",")
	normalised := make([]string, 0, len(parts))
	for _, raw := range parts {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			return nil, fmt.Errorf("%w: empty entry in comma-separated origins", ErrInvalidAllowOrigin)
		}
		if entry == "*" {
			return nil, ErrWildcardWithCredentials
		}
		u, err := url.Parse(entry)
		if err != nil {
			return nil, fmt.Errorf("%w: parse %q: %v", ErrInvalidAllowOrigin, entry, err)
		}
		if u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("%w: %q missing scheme or host", ErrInvalidAllowOrigin, entry)
		}
		// http or https only — any other scheme (file://, ftp://, etc.)
		// is invalid for a browser CORS allow-origin.
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("%w: %q scheme %q is not http or https", ErrInvalidAllowOrigin, entry, u.Scheme)
		}
		normalised = append(normalised, entry)
	}

	return cors.New(cors.Config{
		AllowOrigins:     strings.Join(normalised, ","),
		AllowCredentials: true,
		AllowMethods:     "GET,POST,PUT,PATCH,DELETE,OPTIONS",
		AllowHeaders:     "Content-Type,X-CSRF-Token,X-Request-Id",
		ExposeHeaders:    "X-Request-Id",
		MaxAge:           600,
	}), nil
}
