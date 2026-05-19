package middleware

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
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
