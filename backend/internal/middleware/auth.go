package middleware

import (
	"context"
	"errors"

	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog/log"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/pkg/jwt"
)

// Auth middleware locals keys. Handlers and downstream middleware should read
// these via the exported accessor helpers below so a typo in the key name
// never silently breaks the auth contract.
const (
	// AuthCookieName is the HTTP cookie that carries the signed JWT. Set by
	// the Login handler with HttpOnly+Secure+SameSite=Lax; checked here.
	AuthCookieName = "auth_token"

	// AuthClaimsLocalsKey stores the full *jwt.Claims for the authenticated
	// caller. Other middleware (CSRF, per-user rate-limit) and handlers
	// (e.g. /api/auth/me) read it via AuthClaimsFromCtx.
	AuthClaimsLocalsKey = "auth.claims"

	// AuthUserIDLocalsKey stores the JWT subject (driver ID) as a
	// convenience so callers do not have to nil-check the claims pointer
	// for the most common lookup.
	AuthUserIDLocalsKey = "auth.user_id"

	// AuthRoleLocalsKey stores the JWT role claim. Used by role-based
	// guards in later tasks.
	AuthRoleLocalsKey = "auth.role"
)

// BlocklistChecker is the narrow contract the auth middleware needs from
// the KV-backed revocation store. usecase.AuthUsecase.IsRevoked satisfies
// this directly — the middleware does not import the usecase package so
// the dependency direction stays consumer-side.
type BlocklistChecker interface {
	IsRevoked(ctx context.Context, claims *jwt.Claims) (bool, error)
}

// TokenVerifier is the narrow contract the auth middleware needs from a
// signer. The concrete *jwt.Signer from pkg/jwt satisfies this — declaring
// it here keeps test wiring uncluttered.
type TokenVerifier interface {
	Verify(token string) (*jwt.Claims, error)
}

// authErrorBody is the JSON payload sent on every 401 from this middleware.
// Kept consistent with the handler's respondError helper so the SPA only
// needs to learn one shape.
type authErrorBody struct {
	Error     string `json:"error"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// NewAuth returns a Fiber middleware that:
//
//  1. reads the auth_token cookie; 401 if absent or empty.
//  2. verifies the JWT signature, issuer, and expiration via verifier.
//  3. checks the JTI against the KV blocklist via blocklist.
//  4. on success, stores the claims, user ID, and role in c.Locals under
//     the exported keys above and calls c.Next().
//
// Every failure path returns 401 with a JSON body shaped like authErrorBody
// and includes the request_id from RequestIDFromCtx so operators can
// correlate denied requests with the access log.
//
// The middleware runs the blocklist lookup BEFORE c.Next() so a revoked
// token can never reach a handler. The KV call is the slow path
// (~5-50ms cold), but auth-required endpoints already cost more than a
// KV round-trip; the trade-off is correctness over micro-latency.
func NewAuth(verifier TokenVerifier, blocklist BlocklistChecker) fiber.Handler {
	if verifier == nil {
		// A nil verifier is a programmer error — return a middleware that
		// always 500s rather than crashing on the first request. Production
		// wiring in main.go fails fast before we get here.
		return func(c *fiber.Ctx) error {
			return respondAuthError(c, fiber.StatusInternalServerError, "internal", "auth middleware misconfigured: nil verifier")
		}
	}
	if blocklist == nil {
		return func(c *fiber.Ctx) error {
			return respondAuthError(c, fiber.StatusInternalServerError, "internal", "auth middleware misconfigured: nil blocklist")
		}
	}

	return func(c *fiber.Ctx) error {
		token := c.Cookies(AuthCookieName)
		if token == "" {
			return respondAuthError(c, fiber.StatusUnauthorized, "unauthorized", "missing auth cookie")
		}

		claims, err := verifier.Verify(token)
		if err != nil {
			// Do not leak whether the failure was signature, exp, or shape —
			// every flavour returns the same 401 so a malicious caller
			// cannot probe for token structure via error messages.
			return respondAuthError(c, fiber.StatusUnauthorized, "unauthorized", "invalid token")
		}

		revoked, err := blocklist.IsRevoked(c.UserContext(), claims)
		if err != nil {
			// A blocklist lookup failure must NOT fail-open — we treat it
			// as a server error so the SPA surfaces a retry-friendly state
			// rather than the caller silently riding a revoked token.
			//
			// Log the detailed error server-side (operators correlate via
			// request_id) but return only a GENERIC envelope to the client.
			// The raw error can carry CF account/namespace identifiers
			// (cfclient.firstErrorMessage / readShort cap at 512 bytes but
			// don't strip identifiers); leaking those into the response
			// gives an unauthenticated caller cheap recon. Security review M4.
			log.Warn().
				Err(err).
				Str("request_id", RequestIDFromCtx(c)).
				Str("route", c.Path()).
				Str("ip", c.IP()).
				Str("jti", claims.JTI).
				Msg("auth middleware: blocklist lookup failed")
			return respondAuthError(c, fiber.StatusInternalServerError, "internal", "blocklist unavailable")
		}
		if revoked {
			return respondAuthError(c, fiber.StatusUnauthorized, "unauthorized", "token revoked")
		}

		c.Locals(AuthClaimsLocalsKey, claims)
		c.Locals(AuthUserIDLocalsKey, claims.Subject)
		c.Locals(AuthRoleLocalsKey, claims.Role)

		return c.Next()
	}
}

// AuthClaimsFromCtx returns the JWT claims set by NewAuth middleware, or
// nil if no auth middleware ran (e.g. an unauthenticated route). Never
// panics — handlers can call it unconditionally.
func AuthClaimsFromCtx(c *fiber.Ctx) *jwt.Claims {
	if v, ok := c.Locals(AuthClaimsLocalsKey).(*jwt.Claims); ok {
		return v
	}
	return nil
}

// AuthUserIDFromCtx returns the driver ID set by NewAuth middleware, or
// "" if no auth middleware ran.
func AuthUserIDFromCtx(c *fiber.Ctx) string {
	if v, ok := c.Locals(AuthUserIDLocalsKey).(string); ok {
		return v
	}
	return ""
}

// AuthRoleFromCtx returns the role string set by NewAuth middleware, or
// "" if no auth middleware ran.
func AuthRoleFromCtx(c *fiber.Ctx) string {
	if v, ok := c.Locals(AuthRoleLocalsKey).(string); ok {
		return v
	}
	return ""
}

// respondAuthError formats a JSON error response with the request_id
// pulled from RequestIDFromCtx. The middleware and the handler layer
// share this shape so the SPA only learns one envelope.
func respondAuthError(c *fiber.Ctx, status int, code, msg string) error {
	body := authErrorBody{
		Error:     code,
		Message:   msg,
		RequestID: RequestIDFromCtx(c),
	}
	return c.Status(status).JSON(body)
}

// errMisconfigured is returned only when NewAuth is constructed with nil
// dependencies. Exposed for the handful of unit tests that exercise the
// guard rail; production code should never see it.
var errMisconfigured = errors.New("auth middleware: nil dependency")
