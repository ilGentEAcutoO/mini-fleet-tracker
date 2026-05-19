package middleware

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// newCSRFApp returns a Fiber app with RequestID + NewCSRF in front of a
// trivial handler. Tests assert on the response of the handler vs the
// 403 envelope emitted by the middleware.
func newCSRFApp(t *testing.T) *fiber.App {
	t.Helper()
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(RequestID())
	app.Use(NewCSRF())
	app.Get("/", func(c *fiber.Ctx) error { return c.SendString("ok-get") })
	app.Head("/head", func(c *fiber.Ctx) error { return c.SendStatus(http.StatusOK) })
	app.Options("/", func(c *fiber.Ctx) error { return c.SendStatus(http.StatusOK) })
	app.Post("/", func(c *fiber.Ctx) error { return c.SendString("ok-post") })
	app.Put("/", func(c *fiber.Ctx) error { return c.SendString("ok-put") })
	app.Patch("/", func(c *fiber.Ctx) error { return c.SendString("ok-patch") })
	app.Delete("/", func(c *fiber.Ctx) error { return c.SendString("ok-delete") })
	return app
}

// readBodyString drains the response body to a string. Kept local so the
// test file does not depend on the helper in auth_test.go.
func readBodyString(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func TestCSRF_SafeMethods_BypassEnforcement(t *testing.T) {
	app := newCSRFApp(t)

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/"},
		{http.MethodHead, "/head"},
		{http.MethodOptions, "/"},
	}
	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, readBodyString(t, resp))
			}
		})
	}
}

func TestCSRF_POSTMissingHeader_403(t *testing.T) {
	app := newCSRFApp(t)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(nil))
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: "abc"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body := readBodyString(t, resp)
	if !strings.Contains(body, `"error":"csrf_missing"`) {
		t.Errorf("body should report csrf_missing: %s", body)
	}
}

func TestCSRF_POSTMissingCookie_403(t *testing.T) {
	app := newCSRFApp(t)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(nil))
	req.Header.Set(CSRFHeaderName, "abc")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body := readBodyString(t, resp)
	if !strings.Contains(body, `"error":"csrf_missing"`) {
		t.Errorf("body should report csrf_missing: %s", body)
	}
}

func TestCSRF_POSTMismatch_403(t *testing.T) {
	app := newCSRFApp(t)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(nil))
	req.Header.Set(CSRFHeaderName, "header-value-abc")
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: "cookie-value-xyz"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body := readBodyString(t, resp)
	if !strings.Contains(body, `"error":"csrf_mismatch"`) {
		t.Errorf("body should report csrf_mismatch: %s", body)
	}
}

func TestCSRF_POSTMatching_PassesThrough(t *testing.T) {
	app := newCSRFApp(t)
	const tok = "matching-token-1234567890abcdef"
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(nil))
	req.Header.Set(CSRFHeaderName, tok)
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tok})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, readBodyString(t, resp))
	}
	if got := readBodyString(t, resp); got != "ok-post" {
		t.Errorf("body = %q, want ok-post", got)
	}
}

func TestCSRF_MutatingMethods_RejectMissing(t *testing.T) {
	app := newCSRFApp(t)
	// PUT, PATCH, DELETE without headers must all be rejected.
	for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/", bytes.NewReader(nil))
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("%s: status = %d, want 403; body=%s", method, resp.StatusCode, readBodyString(t, resp))
			}
		})
	}
}

func TestCSRF_ConstantTimeCompare_LengthMismatchAlsoRejected(t *testing.T) {
	// The middleware uses subtle.ConstantTimeCompare which returns 0 for
	// length mismatches — verify that path is exercised.
	app := newCSRFApp(t)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(nil))
	req.Header.Set(CSRFHeaderName, "short")
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: "considerably-longer-than-the-header"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestCSRF_403BodyIncludesRequestID(t *testing.T) {
	app := newCSRFApp(t)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(nil))
	req.Header.Set(RequestIDHeader, "csrf-test-9001")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	body := readBodyString(t, resp)
	if !strings.Contains(body, `"request_id":"csrf-test-9001"`) {
		t.Errorf("expected request_id in body, got: %s", body)
	}
}
