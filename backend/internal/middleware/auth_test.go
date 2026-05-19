package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/pkg/jwt"
)

// ---------------------------------------------------------------------------
// Test doubles. Hand-rolled, minimal — no mocking library pulled into the
// dependency tree.
// ---------------------------------------------------------------------------

// stubBlocklist is an in-memory BlocklistChecker. The error field lets tests
// inject a deliberate failure to exercise the fail-closed path.
type stubBlocklist struct {
	mu      sync.Mutex
	revoked map[string]bool
	err     error
}

func newStubBlocklist() *stubBlocklist {
	return &stubBlocklist{revoked: map[string]bool{}}
}

func (b *stubBlocklist) Revoke(jti string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.revoked[jti] = true
}

func (b *stubBlocklist) IsRevoked(_ context.Context, c *jwt.Claims) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return false, b.err
	}
	if c == nil {
		return false, nil
	}
	return b.revoked[c.JTI], nil
}

// ---------------------------------------------------------------------------
// Test helpers.
// ---------------------------------------------------------------------------

func newAuthApp(t *testing.T, verifier TokenVerifier, bl BlocklistChecker) *fiber.App {
	t.Helper()
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(RequestID())
	app.Use(NewAuth(verifier, bl))
	app.Get("/", func(c *fiber.Ctx) error {
		// Echo the claims back so we can assert they made it into Locals.
		claims := AuthClaimsFromCtx(c)
		uid := AuthUserIDFromCtx(c)
		role := AuthRoleFromCtx(c)
		return c.JSON(fiber.Map{
			"jti":     claims.JTI,
			"user_id": uid,
			"role":    role,
		})
	})
	return app
}

// issueTestToken mints a signed token via the real Signer so the
// middleware sees the same shape it will in production.
func issueTestToken(t *testing.T, signer *jwt.Signer, sub, role string) (string, *jwt.Claims) {
	t.Helper()
	tok, claims, err := signer.Issue(sub, role)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return tok, claims
}

func newTestSigner(t *testing.T, ttl time.Duration) *jwt.Signer {
	t.Helper()
	s, err := jwt.NewSigner("auth-mw-test-secret", ttl)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

// readBody returns the response body as a string so error assertions can
// pattern-match without juggling buffers per-test.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------------

func TestAuth_NoCookie_401(t *testing.T) {
	signer := newTestSigner(t, time.Hour)
	bl := newStubBlocklist()
	app := newAuthApp(t, signer, bl)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"error":"unauthorized"`) {
		t.Fatalf("body missing error=unauthorized: %s", body)
	}
}

func TestAuth_BadSignature_401(t *testing.T) {
	// Token signed with one secret, verified with another → 401.
	other, err := jwt.NewSigner("a-different-secret", time.Hour)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	badTok, _ := issueTestToken(t, other, "drv_1", "driver")

	signer := newTestSigner(t, time.Hour)
	bl := newStubBlocklist()
	app := newAuthApp(t, signer, bl)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: AuthCookieName, Value: badTok})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuth_ExpiredToken_401(t *testing.T) {
	// Issue a token with a very short TTL, then wait long enough for
	// the wall clock to advance past it. We can't easily mock the
	// signer's now from outside, so use a positive TTL and rely on
	// time.Sleep being well above clock granularity — 5ms TTL + 50ms
	// pause is robust on every CI we use.
	short, err := jwt.NewSigner("test", 5*time.Millisecond)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	tok, _ := issueTestToken(t, short, "drv_1", "driver")
	time.Sleep(50 * time.Millisecond)

	bl := newStubBlocklist()
	app := newAuthApp(t, short, bl)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: AuthCookieName, Value: tok})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuth_RevokedToken_401(t *testing.T) {
	signer := newTestSigner(t, time.Hour)
	tok, claims := issueTestToken(t, signer, "drv_1", "driver")

	bl := newStubBlocklist()
	bl.Revoke(claims.JTI)

	app := newAuthApp(t, signer, bl)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: AuthCookieName, Value: tok})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "revoked") {
		t.Fatalf("revoked path should mention 'revoked': %s", body)
	}
}

func TestAuth_ValidToken_PopulatesLocals_200(t *testing.T) {
	signer := newTestSigner(t, time.Hour)
	tok, claims := issueTestToken(t, signer, "drv_42", "manager")

	bl := newStubBlocklist()
	app := newAuthApp(t, signer, bl)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: AuthCookieName, Value: tok})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, readBody(t, resp))
	}

	var body struct {
		JTI    string `json:"jti"`
		UserID string `json:"user_id"`
		Role   string `json:"role"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	defer resp.Body.Close()

	if body.JTI != claims.JTI {
		t.Errorf("JTI: got %q, want %q", body.JTI, claims.JTI)
	}
	if body.UserID != "drv_42" {
		t.Errorf("user_id: got %q, want drv_42", body.UserID)
	}
	if body.Role != "manager" {
		t.Errorf("role: got %q, want manager", body.Role)
	}
}

func TestAuth_BlocklistError_500(t *testing.T) {
	signer := newTestSigner(t, time.Hour)
	tok, _ := issueTestToken(t, signer, "drv_1", "driver")

	bl := newStubBlocklist()
	bl.err = errors.New("kv outage")

	app := newAuthApp(t, signer, bl)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: AuthCookieName, Value: tok})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

func TestAuth_NilVerifier_500(t *testing.T) {
	// NewAuth returns a 500-emitting middleware (rather than panicking)
	// when constructed with nil deps; this guards the wiring against
	// a silent contract drift.
	bl := newStubBlocklist()
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(RequestID())
	app.Use(NewAuth(nil, bl))
	app.Get("/", func(c *fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

func TestAuth_NilBlocklist_500(t *testing.T) {
	signer := newTestSigner(t, time.Hour)
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(RequestID())
	app.Use(NewAuth(signer, nil))
	app.Get("/", func(c *fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

func TestAuth_LocalsAccessors_EmptyWhenMiddlewareAbsent(t *testing.T) {
	// AuthClaimsFromCtx / AuthUserIDFromCtx / AuthRoleFromCtx must be
	// nil-safe so handlers can call them unconditionally without
	// crashing if the auth middleware is bypassed (e.g. on a public route).
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/", func(c *fiber.Ctx) error {
		if AuthClaimsFromCtx(c) != nil {
			t.Errorf("expected nil claims, got non-nil")
		}
		if AuthUserIDFromCtx(c) != "" {
			t.Errorf("expected empty user_id, got non-empty")
		}
		if AuthRoleFromCtx(c) != "" {
			t.Errorf("expected empty role, got non-empty")
		}
		return c.SendStatus(fiber.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, err := app.Test(req); err != nil {
		t.Fatalf("app.Test: %v", err)
	}
}

func TestAuth_errMisconfigured_Sentinel(t *testing.T) {
	// Guard against accidental removal of the sentinel — the constant is
	// exposed so any future code that needs to detect a misconfigured
	// auth middleware can errors.Is against it.
	if errMisconfigured == nil {
		t.Fatal("errMisconfigured must be a non-nil sentinel")
	}
	if !strings.Contains(errMisconfigured.Error(), "nil dependency") {
		t.Errorf("errMisconfigured text drifted: %q", errMisconfigured.Error())
	}
}
