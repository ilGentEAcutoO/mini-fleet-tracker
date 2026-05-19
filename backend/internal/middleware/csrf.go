package middleware

import (
	"crypto/subtle"

	"github.com/gofiber/fiber/v2"
)

// CSRF cookie + header conventions. The double-submit pattern stores the
// same random value in a JS-readable cookie (csrf_token) and expects the
// SPA to echo it back in the X-CSRF-Token header. An attacker cross-origin
// page can read neither value because of SameSite=Lax on the cookie and the
// browser-enforced same-origin restriction on header reads — the match
// therefore proves the request originated from the SPA, not from a forged
// cross-site form.
const (
	// CSRFCookieName is the JS-readable cookie containing the per-session
	// CSRF token. Login sets it; the SPA reads it and copies to the header
	// on every mutating fetch.
	CSRFCookieName = "csrf_token"

	// CSRFHeaderName is the request header the SPA echoes the cookie value
	// in. Constant-time compared with the cookie below.
	CSRFHeaderName = "X-CSRF-Token"
)

// csrfErrorBody is the JSON envelope returned on every 403 from this
// middleware. Same shape as the auth middleware so the SPA learns one
// envelope only.
type csrfErrorBody struct {
	Error     string `json:"error"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// NewCSRF returns a Fiber middleware that enforces the double-submit cookie
// pattern on every mutating request (POST, PUT, PATCH, DELETE). Safe
// methods (GET, HEAD, OPTIONS) pass through unconditionally because they
// must not have side effects per RFC 9110 anyway, and CORS preflight
// (OPTIONS) is critical to allow through unmolested.
//
// On a mutating request the middleware:
//  1. reads the X-CSRF-Token header; 403 if empty.
//  2. reads the csrf_token cookie; 403 if empty.
//  3. constant-time compares the two; 403 on mismatch.
//  4. otherwise calls c.Next().
//
// The middleware does NOT generate the token — that's the Login handler's
// job. Tokens that need rotation are achieved by issuing a new csrf_token
// cookie at the next login.
//
// Order: must run AFTER NewAuth so a missing auth cookie surfaces as 401
// rather than 403. The wiring layer (cmd/api/main.go) enforces this.
func NewCSRF() fiber.Handler {
	return func(c *fiber.Ctx) error {
		method := c.Method()
		// Fast path for safe methods. fasthttp's c.Method returns the
		// canonical upper-case form so we can compare directly.
		switch method {
		case fiber.MethodGet, fiber.MethodHead, fiber.MethodOptions:
			return c.Next()
		}

		header := c.Get(CSRFHeaderName)
		if header == "" {
			return respondCSRFError(c, "csrf_missing", "missing "+CSRFHeaderName+" header")
		}
		cookie := c.Cookies(CSRFCookieName)
		if cookie == "" {
			return respondCSRFError(c, "csrf_missing", "missing "+CSRFCookieName+" cookie")
		}

		// constant-time comparison: ConstantTimeCompare returns 1 iff the
		// two slices have equal length AND equal contents. A length mismatch
		// returns 0 without short-circuiting on content, so timing reveals
		// only the length difference — and the cookie length is fixed
		// in the Login handler so even that is a constant.
		if subtle.ConstantTimeCompare([]byte(header), []byte(cookie)) != 1 {
			return respondCSRFError(c, "csrf_mismatch", "csrf token mismatch")
		}

		return c.Next()
	}
}

// respondCSRFError formats the standard 403 envelope, including the
// request_id so operators can correlate rejected requests with logs.
func respondCSRFError(c *fiber.Ctx, code, msg string) error {
	return c.Status(fiber.StatusForbidden).JSON(csrfErrorBody{
		Error:     code,
		Message:   msg,
		RequestID: RequestIDFromCtx(c),
	})
}
