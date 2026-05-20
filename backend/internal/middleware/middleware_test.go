package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	fiberrecover "github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/rs/zerolog"
)

// newTestApp returns a Fiber app suitable for httptest.
func newTestApp() *fiber.App {
	return fiber.New(fiber.Config{
		DisableStartupMessage: true,
	})
}

func TestRequestID_GeneratesWhenAbsent(t *testing.T) {
	app := newTestApp()
	app.Use(RequestID())
	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString(RequestIDFromCtx(c))
	})

	req := httptest.NewRequest("GET", "/", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	hdr := resp.Header.Get(RequestIDHeader)
	if hdr == "" {
		t.Fatalf("response missing %s header", RequestIDHeader)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != hdr {
		t.Errorf("body %q != header %q (Locals/header drift)", body, hdr)
	}
	// Generated value should look like a UUID — 36 chars with 4 dashes.
	if len(hdr) != 36 || strings.Count(hdr, "-") != 4 {
		t.Errorf("generated request_id does not look like a UUID: %q", hdr)
	}
}

func TestRequestID_PreservesInbound(t *testing.T) {
	app := newTestApp()
	app.Use(RequestID())
	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString(RequestIDFromCtx(c))
	})

	const inbound = "edge-cdn-trace-12345"
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(RequestIDHeader, inbound)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get(RequestIDHeader); got != inbound {
		t.Errorf("response header = %q, want %q", got, inbound)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != inbound {
		t.Errorf("locals propagation: body = %q, want %q", body, inbound)
	}
}

func TestRequestID_TrimsWhitespaceFallsBackToGenerated(t *testing.T) {
	app := newTestApp()
	app.Use(RequestID())
	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString(RequestIDFromCtx(c))
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(RequestIDHeader, "   ")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	hdr := resp.Header.Get(RequestIDHeader)
	if strings.TrimSpace(hdr) == "" {
		t.Fatalf("expected a generated UUID, got %q", hdr)
	}
	if len(hdr) != 36 {
		t.Errorf("expected generated UUID, got %q", hdr)
	}
}

func TestRequestIDFromCtx_EmptyWhenAbsent(t *testing.T) {
	app := newTestApp()
	app.Get("/", func(c *fiber.Ctx) error {
		if got := RequestIDFromCtx(c); got != "" {
			t.Errorf("RequestIDFromCtx() = %q, want \"\"", got)
		}
		return c.SendStatus(fiber.StatusOK)
	})
	req := httptest.NewRequest("GET", "/", nil)
	if _, err := app.Test(req); err != nil {
		t.Fatalf("app.Test: %v", err)
	}
}

func TestLogger_AttachesRequestScopedLogger(t *testing.T) {
	// Capture global logger output through a buffer; restore on cleanup.
	var buf strings.Builder
	prev := zerolog.DefaultContextLogger
	t.Cleanup(func() { zerolog.DefaultContextLogger = prev })

	// Swap the global logger so FromCtx will see a known sink. zerolog's
	// log.Logger is a package-level var — we can't easily reset it, so
	// instead we just rely on the per-request logger being non-nil and
	// emitting the request_id field.
	app := newTestApp()
	app.Use(RequestID())
	app.Use(Logger())
	app.Get("/", func(c *fiber.Ctx) error {
		l := FromCtx(c)
		// Redirect this specific logger's writer to our buffer for the
		// duration of the handler — proves the per-request logger is
		// distinct from the global one.
		scoped := l.Output(&buf)
		scoped.Info().Msg("hello")
		return c.SendStatus(fiber.StatusOK)
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(RequestIDHeader, "fixed-id-9001")
	if _, err := app.Test(req); err != nil {
		t.Fatalf("app.Test: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, `"request_id":"fixed-id-9001"`) {
		t.Errorf("logger output missing request_id field; got: %s", out)
	}
	if !strings.Contains(out, `"message":"hello"`) {
		t.Errorf("logger output missing message; got: %s", out)
	}
}

func TestFromCtx_FallsBackToGlobalWhenMiddlewareAbsent(t *testing.T) {
	app := newTestApp()
	app.Get("/", func(c *fiber.Ctx) error {
		l := FromCtx(c)
		if l == nil {
			t.Errorf("FromCtx returned nil")
		}
		return c.SendStatus(fiber.StatusOK)
	})
	req := httptest.NewRequest("GET", "/", nil)
	if _, err := app.Test(req); err != nil {
		t.Fatalf("app.Test: %v", err)
	}
}

func TestLogger_PreservesParentContextValues(t *testing.T) {
	type ctxKey string
	const k ctxKey = "trace"

	app := newTestApp()
	// Inject a parent context value BEFORE the logger middleware to verify
	// it's preserved through WithContext + SetUserContext.
	app.Use(func(c *fiber.Ctx) error {
		c.SetUserContext(context.WithValue(c.UserContext(), k, "parent-value"))
		return c.Next()
	})
	app.Use(RequestID())
	app.Use(Logger())
	app.Get("/", func(c *fiber.Ctx) error {
		if v, _ := c.UserContext().Value(k).(string); v != "parent-value" {
			t.Errorf("parent ctx value lost; got %q", v)
		}
		return c.SendStatus(fiber.StatusOK)
	})
	req := httptest.NewRequest("GET", "/", nil)
	if _, err := app.Test(req); err != nil {
		t.Fatalf("app.Test: %v", err)
	}
}

func TestCORS_RejectsEmpty(t *testing.T) {
	if _, err := CORS(""); !errors.Is(err, ErrEmptyAllowOrigin) {
		t.Errorf("CORS(\"\") err = %v, want ErrEmptyAllowOrigin", err)
	}
	if _, err := CORS("   "); !errors.Is(err, ErrEmptyAllowOrigin) {
		t.Errorf("CORS(\"   \") err = %v, want ErrEmptyAllowOrigin", err)
	}
}

func TestCORS_RejectsWildcard(t *testing.T) {
	if _, err := CORS("*"); !errors.Is(err, ErrWildcardWithCredentials) {
		t.Errorf("CORS(\"*\") err = %v, want ErrWildcardWithCredentials", err)
	}
}

// TestCORS_RejectsInvalidOrigins exercises the startup-time URL parse guard
// added by TASK-055 (security review M3). Any origin that does not have BOTH
// a scheme and a host should be rejected with ErrInvalidAllowOrigin so a
// misconfigured CORS_ORIGIN surfaces at boot rather than at the first
// cross-origin fetch (Fiber's CORS treats unknown patterns as deny-all,
// which would silently break the SPA).
func TestCORS_RejectsInvalidOrigins(t *testing.T) {
	cases := []struct {
		name   string
		origin string
	}{
		{"scheme_only", "http://"},
		{"no_scheme_just_host", "example.com"},
		{"wildcard_host_no_scheme", "*.example.com"},
		{"path_only", "/api"},
		{"junk", "not a url"},
		{"control_chars", "http://\x00\x01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, err := CORS(tc.origin)
			if err == nil {
				t.Fatalf("CORS(%q) returned nil error; expected ErrInvalidAllowOrigin (handler non-nil: %v)", tc.origin, h != nil)
			}
			if !errors.Is(err, ErrInvalidAllowOrigin) {
				t.Fatalf("CORS(%q) err = %v, want errors.Is(ErrInvalidAllowOrigin)", tc.origin, err)
			}
		})
	}
}

// TestCORS_AcceptsValidOrigins is the positive partner. Both http://localhost
// and https://fleet-tracker.jairukchan.com — the project's two real origins
// — must continue to construct cleanly.
func TestCORS_AcceptsValidOrigins(t *testing.T) {
	cases := []string{
		"http://localhost:3000",
		"http://localhost",
		"https://fleet-tracker.jairukchan.com",
		"https://example.com:8443",
	}
	for _, origin := range cases {
		t.Run(origin, func(t *testing.T) {
			h, err := CORS(origin)
			if err != nil {
				t.Fatalf("CORS(%q) err = %v, want nil", origin, err)
			}
			if h == nil {
				t.Fatalf("CORS(%q) handler nil; expected non-nil", origin)
			}
		})
	}
}

func TestCORS_PreflightHeaders(t *testing.T) {
	h, err := CORS("http://localhost:3000")
	if err != nil {
		t.Fatalf("CORS: %v", err)
	}
	app := newTestApp()
	app.Use(h)
	app.Get("/", func(c *fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	req := httptest.NewRequest("OPTIONS", "/", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "X-CSRF-Token")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Errorf("Allow-Origin = %q, want http://localhost:3000", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Allow-Credentials = %q, want true", got)
	}
	if got := resp.Header.Get("Access-Control-Max-Age"); got != "600" {
		t.Errorf("Max-Age = %q, want 600", got)
	}
	allowMethods := resp.Header.Get("Access-Control-Allow-Methods")
	for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		if !strings.Contains(allowMethods, m) {
			t.Errorf("Allow-Methods %q missing %s", allowMethods, m)
		}
	}
}

func TestCORS_ActualRequestExposesRequestID(t *testing.T) {
	h, err := CORS("http://localhost:3000")
	if err != nil {
		t.Fatalf("CORS: %v", err)
	}
	app := newTestApp()
	app.Use(h)
	app.Get("/", func(c *fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	expose := resp.Header.Get("Access-Control-Expose-Headers")
	if !strings.Contains(expose, "X-Request-Id") {
		t.Errorf("Expose-Headers = %q, want to contain X-Request-Id", expose)
	}
}

// ---------------------------------------------------------------------------
// TASK-061 — JSONErrorHandler wraps recovered panics + handler errors in
// the standard {error, message, request_id} envelope.
// ---------------------------------------------------------------------------

// newPanicApp returns a Fiber app wired with the same chain bootstrap.go
// uses: JSONErrorHandler at the fiber.Config level, then RequestID then
// recover.New at the middleware level. The panic site is a /boom handler.
func newPanicApp() *fiber.App {
	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ErrorHandler:          JSONErrorHandler,
	})
	app.Use(RequestID())
	app.Use(fiberrecover.New())
	app.Get("/boom", func(c *fiber.Ctx) error {
		panic("kaboom: simulated panic inside handler")
	})
	app.Get("/fiber-error", func(c *fiber.Ctx) error {
		return fiber.NewError(http.StatusBadGateway, "upstream broken")
	})
	app.Get("/plain-error", func(c *fiber.Ctx) error {
		return errors.New("non-fiber error from handler")
	})
	return app
}

// TestJSONErrorHandler_PanicYieldsStandardEnvelope is the headline TASK-061
// guard rail: a handler panic must surface as a JSON envelope with status
// 500, not Fiber's default text/plain stack-trace dump. Without this, the
// SPA's error-toast pipeline fails to parse and the user sees nothing
// useful while operators lose the request_id correlation.
func TestJSONErrorHandler_PanicYieldsStandardEnvelope(t *testing.T) {
	app := newPanicApp()
	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	req.Header.Set(RequestIDHeader, "panic-test-rid-1")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	if ct := resp.Header.Get(fiber.HeaderContentType); !strings.HasPrefix(ct, fiber.MIMEApplicationJSON) {
		t.Errorf("Content-Type = %q, want application/json prefix", ct)
	}

	body, _ := io.ReadAll(resp.Body)
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body is not JSON: %v\nbody=%s", err, body)
	}
	if env["error"] != "internal" {
		t.Errorf("error field = %v, want internal", env["error"])
	}
	if env["message"] != "internal server error" {
		t.Errorf("message field = %v, want generic placeholder", env["message"])
	}
	if env["request_id"] != "panic-test-rid-1" {
		t.Errorf("request_id field = %v, want passthrough panic-test-rid-1", env["request_id"])
	}
	// Stack traces must NOT leak to clients — operators see them server-side
	// via recover's default StackTraceHandler.
	if strings.Contains(string(body), "kaboom") || strings.Contains(string(body), "goroutine") {
		t.Errorf("body must not leak panic payload or stack trace: %s", body)
	}
}

// TestJSONErrorHandler_FiberErrorRetainsStatus proves fiber.NewError(code,
// msg) keeps the explicit status — needed because the recover middleware
// turns recovered panics into anonymous errors, but real handlers that
// return *fiber.Error want their own code respected (404, 502, etc).
func TestJSONErrorHandler_FiberErrorRetainsStatus(t *testing.T) {
	app := newPanicApp()
	req := httptest.NewRequest(http.MethodGet, "/fiber-error", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body is not JSON: %v\nbody=%s", err, body)
	}
	// fiber.Error.Message *can* be surfaced because the handler chose it —
	// unlike a panic (uncontrolled origin) the message is a deliberate
	// choice from the developer.
	if env["message"] != "upstream broken" {
		t.Errorf("message = %v, want upstream broken", env["message"])
	}
	if env["error"] != "internal" {
		t.Errorf("error code = %v, want internal", env["error"])
	}
}

// TestJSONErrorHandler_NonFiberErrorBecomes500 — a plain `errors.New` from
// a handler is treated as a 500 because we don't know what status the
// developer intended. The message stays generic to avoid leaking error
// strings that may carry internal details (DB row IDs, etc).
func TestJSONErrorHandler_NonFiberErrorBecomes500(t *testing.T) {
	app := newPanicApp()
	req := httptest.NewRequest(http.MethodGet, "/plain-error", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "non-fiber error from handler") {
		t.Errorf("body must not leak the raw error message: %s", body)
	}
}
