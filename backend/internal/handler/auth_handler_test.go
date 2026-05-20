package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/middleware"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/pkg/hash"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/pkg/jwt"
)

// ---------------------------------------------------------------------------
// Hand-rolled in-memory fakes — minimal surface, kept inline so each test
// case can be read in one screen.
// ---------------------------------------------------------------------------

type memDriverRepo struct {
	mu      sync.Mutex
	byID    map[string]*domain.Driver
	byEmail map[string]*domain.Driver
}

func newMemDriverRepo() *memDriverRepo {
	return &memDriverRepo{byID: map[string]*domain.Driver{}, byEmail: map[string]*domain.Driver{}}
}

func (m *memDriverRepo) Create(_ context.Context, d *domain.Driver) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byEmail[d.Email]; ok {
		return fmt.Errorf("dup %s: %w", d.Email, domain.ErrAlreadyExists)
	}
	cp := *d
	m.byID[d.ID] = &cp
	m.byEmail[d.Email] = &cp
	return nil
}

func (m *memDriverRepo) GetByEmail(_ context.Context, email string) (*domain.Driver, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.byEmail[email]
	if !ok {
		return nil, fmt.Errorf("email %s: %w", email, domain.ErrNotFound)
	}
	cp := *d
	return &cp, nil
}

func (m *memDriverRepo) GetByID(_ context.Context, id string) (*domain.Driver, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.byID[id]
	if !ok {
		return nil, fmt.Errorf("id %s: %w", id, domain.ErrNotFound)
	}
	cp := *d
	return &cp, nil
}

type memBlocklist struct {
	mu   sync.Mutex
	data map[string]bool
}

func newMemBlocklist() *memBlocklist { return &memBlocklist{data: map[string]bool{}} }

func (m *memBlocklist) Revoke(jti string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[jti] = true
}

func (m *memBlocklist) IsRevoked(_ context.Context, c *jwt.Claims) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c == nil {
		return false, nil
	}
	return m.data[c.JTI], nil
}

// stubUsecase implements the AuthUsecase interface. The default behaviour
// hashes passwords with the real pkg/hash and signs tokens with a real
// pkg/jwt.Signer so handler-level assertions about cookies, status codes,
// and JSON shape are end-to-end.
type stubUsecase struct {
	repo    *memDriverRepo
	signer  *jwt.Signer
	bl      *memBlocklist
	idCount int
	mu      sync.Mutex
}

func newStubUsecase(t *testing.T, signer *jwt.Signer) *stubUsecase {
	t.Helper()
	return &stubUsecase{
		repo:   newMemDriverRepo(),
		signer: signer,
		bl:     newMemBlocklist(),
	}
}

func (u *stubUsecase) nextID() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.idCount++
	return fmt.Sprintf("drv_%03d", u.idCount)
}

func (u *stubUsecase) Register(ctx context.Context, email, password, name string, role domain.Role) (*domain.Driver, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	name = strings.TrimSpace(name)
	if email == "" || name == "" || password == "" {
		return nil, fmt.Errorf("missing field: %w", domain.ErrValidation)
	}
	if !role.Valid() {
		return nil, fmt.Errorf("invalid role %q: %w", role, domain.ErrValidation)
	}
	encoded, err := hash.HashPassword(password)
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	d := &domain.Driver{
		ID:           u.nextID(),
		Email:        email,
		PasswordHash: encoded,
		Name:         name,
		Role:         role,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := u.repo.Create(ctx, d); err != nil {
		return nil, err
	}
	return d, nil
}

func (u *stubUsecase) Login(ctx context.Context, email, password string) (string, *domain.Driver, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	d, err := u.repo.GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return "", nil, domain.ErrUnauthorized
		}
		return "", nil, err
	}
	if !hash.VerifyPassword(password, d.PasswordHash) {
		return "", nil, domain.ErrUnauthorized
	}
	token, _, err := u.signer.Issue(d.ID, string(d.Role))
	if err != nil {
		return "", nil, err
	}
	return token, d, nil
}

func (u *stubUsecase) Me(ctx context.Context, userID string) (*domain.Driver, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, fmt.Errorf("empty user id: %w", domain.ErrValidation)
	}
	return u.repo.GetByID(ctx, userID)
}

func (u *stubUsecase) Logout(_ context.Context, claims *jwt.Claims) error {
	if claims == nil {
		return fmt.Errorf("nil claims: %w", domain.ErrValidation)
	}
	if strings.TrimSpace(claims.JTI) == "" {
		return fmt.Errorf("missing jti: %w", domain.ErrValidation)
	}
	u.bl.Revoke(claims.JTI)
	return nil
}

// ---------------------------------------------------------------------------
// Test harness — boots a Fiber app with the same middleware order
// production uses, so cookie + CSRF + auth interactions are exercised
// end-to-end.
// ---------------------------------------------------------------------------

type harness struct {
	app     *fiber.App
	stub    *stubUsecase
	signer  *jwt.Signer
	handler *AuthHandler
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	signer, err := jwt.NewSigner("handler-test-secret-please-replace", time.Hour)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	stub := newStubUsecase(t, signer)
	h, err := NewAuthHandler(stub, signer, DefaultCookieAttrs(true))
	if err != nil {
		t.Fatalf("NewAuthHandler: %v", err)
	}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(middleware.RequestID())
	app.Use(middleware.Logger())

	app.Post("/api/auth/register", h.Register)
	app.Post("/api/auth/login", h.Login)

	authed := app.Group("", middleware.NewAuth(signer, &usecaseBlocklistAdapter{u: stub}))
	authed.Get("/api/auth/me", h.Me)
	// Logout is auth + csrf protected.
	authed.Post("/api/auth/logout", middleware.NewCSRF(), h.Logout)

	return &harness{app: app, stub: stub, signer: signer, handler: h}
}

// usecaseBlocklistAdapter exposes stubUsecase as a BlocklistChecker so we
// can reuse the in-memory blocklist for the auth-middleware integration
// in this test file.
type usecaseBlocklistAdapter struct{ u *stubUsecase }

func (a *usecaseBlocklistAdapter) IsRevoked(ctx context.Context, c *jwt.Claims) (bool, error) {
	return a.u.bl.IsRevoked(ctx, c)
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

func jsonReq(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// loginAndCollectCookies registers a user and logs them in. Returns the
// auth + csrf cookie values for use in subsequent requests.
func loginAndCollectCookies(t *testing.T, h *harness, email, password string) (authCookie, csrfCookie string) {
	t.Helper()
	regBody := registerRequest{Email: email, Password: password, Name: "Test User", Role: "driver"}
	regReq := jsonReq(t, http.MethodPost, "/api/auth/register", regBody)
	regResp, err := h.app.Test(regReq)
	if err != nil {
		t.Fatalf("register Test: %v", err)
	}
	defer regResp.Body.Close()
	if regResp.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d, want 201; body=%s", regResp.StatusCode, readBody(t, regResp))
	}

	loginBody := loginRequest{Email: email, Password: password}
	loginReq := jsonReq(t, http.MethodPost, "/api/auth/login", loginBody)
	loginResp, err := h.app.Test(loginReq)
	if err != nil {
		t.Fatalf("login Test: %v", err)
	}
	defer loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200; body=%s", loginResp.StatusCode, readBody(t, loginResp))
	}

	for _, ck := range loginResp.Cookies() {
		switch ck.Name {
		case middleware.AuthCookieName:
			authCookie = ck.Value
		case middleware.CSRFCookieName:
			csrfCookie = ck.Value
		}
	}
	if authCookie == "" {
		t.Fatal("login response did not set auth_token cookie")
	}
	if csrfCookie == "" {
		t.Fatal("login response did not set csrf_token cookie")
	}
	return authCookie, csrfCookie
}

// ---------------------------------------------------------------------------
// Register tests.
// ---------------------------------------------------------------------------

func TestRegister_Success(t *testing.T) {
	h := newHarness(t)
	body := registerRequest{
		Email:    "ada@example.com",
		Password: "hunter2-very-secure",
		Name:     "Ada Lovelace",
		Role:     "manager",
	}
	resp, err := h.app.Test(jsonReq(t, http.MethodPost, "/api/auth/register", body))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", resp.StatusCode, readBody(t, resp))
	}

	got := readBody(t, resp)
	if strings.Contains(got, "password_hash") {
		t.Fatalf("response body must not include password_hash; got: %s", got)
	}
	if !strings.Contains(got, `"email":"ada@example.com"`) {
		t.Errorf("body missing email field: %s", got)
	}
	if !strings.Contains(got, `"role":"manager"`) {
		t.Errorf("body missing role field: %s", got)
	}
	if !strings.Contains(got, `"id":"drv_001"`) {
		t.Errorf("body missing id field: %s", got)
	}
}

func TestRegister_DuplicateEmail_409(t *testing.T) {
	h := newHarness(t)
	body := registerRequest{
		Email: "dup@example.com", Password: "hunter2-very-secure", Name: "Dup", Role: "driver",
	}
	_, err := h.app.Test(jsonReq(t, http.MethodPost, "/api/auth/register", body))
	if err != nil {
		t.Fatalf("first Test: %v", err)
	}
	resp, err := h.app.Test(jsonReq(t, http.MethodPost, "/api/auth/register", body))
	if err != nil {
		t.Fatalf("second Test: %v", err)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if got := readBody(t, resp); !strings.Contains(got, `"error":"already_exists"`) {
		t.Errorf("body should report already_exists: %s", got)
	}
}

func TestRegister_ValidationFail_400(t *testing.T) {
	h := newHarness(t)
	body := registerRequest{
		Email:    "not-an-email",
		Password: "short",
		Name:     "",
		Role:     "admin",
	}
	resp, err := h.app.Test(jsonReq(t, http.MethodPost, "/api/auth/register", body))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	got := readBody(t, resp)
	if !strings.Contains(got, `"error":"validation_failed"`) {
		t.Errorf("body should report validation_failed: %s", got)
	}
}

func TestRegister_InvalidJSON_400(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Login tests.
// ---------------------------------------------------------------------------

func TestLogin_Success_SetsBothCookies(t *testing.T) {
	h := newHarness(t)

	regBody := registerRequest{
		Email: "ada@example.com", Password: "hunter2-very-secure", Name: "Ada", Role: "driver",
	}
	_, _ = h.app.Test(jsonReq(t, http.MethodPost, "/api/auth/register", regBody))

	loginBody := loginRequest{Email: "ada@example.com", Password: "hunter2-very-secure"}
	resp, err := h.app.Test(jsonReq(t, http.MethodPost, "/api/auth/login", loginBody))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, readBody(t, resp))
	}

	var (
		seenAuth, seenCSRF bool
	)
	for _, ck := range resp.Cookies() {
		switch ck.Name {
		case middleware.AuthCookieName:
			seenAuth = true
			if ck.Value == "" {
				t.Errorf("auth cookie value is empty")
			}
			if !ck.HttpOnly {
				t.Errorf("auth cookie must be HttpOnly")
			}
			if ck.SameSite != http.SameSiteLaxMode {
				t.Errorf("auth cookie SameSite = %v, want Lax", ck.SameSite)
			}
		case middleware.CSRFCookieName:
			seenCSRF = true
			if ck.Value == "" {
				t.Errorf("csrf cookie value is empty")
			}
			if ck.HttpOnly {
				t.Errorf("csrf cookie must NOT be HttpOnly (JS reads it)")
			}
		}
	}
	if !seenAuth || !seenCSRF {
		t.Fatalf("expected both cookies set; auth=%v csrf=%v", seenAuth, seenCSRF)
	}

	body := readBody(t, resp)
	if strings.Contains(body, "password_hash") {
		t.Fatalf("login body must not include password_hash: %s", body)
	}
}

// TestLogin_CSRFCookie_IsStrictWhileAuthStaysLax pins TASK-059 (security
// review L1). The CSRF cookie must be SameSite=Strict so a top-level
// cross-site navigation to the API can't carry the double-submit token
// along. The auth_token cookie stays SameSite=Lax so legitimate same-site
// link clicks keep working — the SPA is single-origin so there is no
// federated login flow that needs Lax on CSRF.
//
// Note the deliberate asymmetry: weakening auth_token to Strict has been
// shown to break the "share a deep link" UX in some browsers, and the
// security review did NOT flag auth_token — only the CSRF cookie.
func TestLogin_CSRFCookie_IsStrictWhileAuthStaysLax(t *testing.T) {
	h := newHarness(t)

	regBody := registerRequest{
		Email: "ada@example.com", Password: "hunter2-very-secure", Name: "Ada", Role: "driver",
	}
	_, _ = h.app.Test(jsonReq(t, http.MethodPost, "/api/auth/register", regBody))

	loginBody := loginRequest{Email: "ada@example.com", Password: "hunter2-very-secure"}
	resp, err := h.app.Test(jsonReq(t, http.MethodPost, "/api/auth/login", loginBody))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var (
		authSameSite, csrfSameSite http.SameSite
		seenAuth, seenCSRF         bool
	)
	for _, ck := range resp.Cookies() {
		switch ck.Name {
		case middleware.AuthCookieName:
			seenAuth = true
			authSameSite = ck.SameSite
		case middleware.CSRFCookieName:
			seenCSRF = true
			csrfSameSite = ck.SameSite
		}
	}
	if !seenAuth || !seenCSRF {
		t.Fatalf("expected both cookies; auth=%v csrf=%v", seenAuth, seenCSRF)
	}
	if authSameSite != http.SameSiteLaxMode {
		t.Errorf("auth_token SameSite = %v, want Lax (unchanged)", authSameSite)
	}
	if csrfSameSite != http.SameSiteStrictMode {
		t.Errorf("csrf_token SameSite = %v, want Strict (TASK-059)", csrfSameSite)
	}
}

func TestLogin_WrongPassword_401(t *testing.T) {
	h := newHarness(t)
	regBody := registerRequest{
		Email: "ada@example.com", Password: "hunter2-very-secure", Name: "Ada", Role: "driver",
	}
	_, _ = h.app.Test(jsonReq(t, http.MethodPost, "/api/auth/register", regBody))

	loginBody := loginRequest{Email: "ada@example.com", Password: "wrong-password"}
	resp, err := h.app.Test(jsonReq(t, http.MethodPost, "/api/auth/login", loginBody))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if got := readBody(t, resp); !strings.Contains(got, `"error":"unauthorized"`) {
		t.Errorf("body should report unauthorized: %s", got)
	}
}

func TestLogin_UnknownEmail_401_SameAsWrongPassword(t *testing.T) {
	h := newHarness(t)
	// No registration first — login should fail with the exact same
	// envelope as wrong password to preserve enumeration-resistance.
	loginBody := loginRequest{Email: "ghost@example.com", Password: "anything-goes-here"}
	resp, err := h.app.Test(jsonReq(t, http.MethodPost, "/api/auth/login", loginBody))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if got := readBody(t, resp); !strings.Contains(got, `"error":"unauthorized"`) {
		t.Errorf("body should report unauthorized: %s", got)
	}
}

func TestLogin_ValidationFail_400(t *testing.T) {
	h := newHarness(t)
	loginBody := loginRequest{Email: "bad", Password: "x"}
	resp, err := h.app.Test(jsonReq(t, http.MethodPost, "/api/auth/login", loginBody))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Logout tests.
// ---------------------------------------------------------------------------

func TestLogout_NoAuth_401(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestLogout_MissingCSRF_403(t *testing.T) {
	h := newHarness(t)
	auth, _ := loginAndCollectCookies(t, h, "ada@example.com", "hunter2-very-secure")

	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: middleware.AuthCookieName, Value: auth})
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestLogout_Success_ClearsCookies(t *testing.T) {
	h := newHarness(t)
	auth, csrf := loginAndCollectCookies(t, h, "ada@example.com", "hunter2-very-secure")

	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: middleware.AuthCookieName, Value: auth})
	req.AddCookie(&http.Cookie{Name: middleware.CSRFCookieName, Value: csrf})
	req.Header.Set(middleware.CSRFHeaderName, csrf)

	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, readBody(t, resp))
	}

	clearedAuth, clearedCSRF := false, false
	for _, ck := range resp.Cookies() {
		// Browsers treat MaxAge<=0 OR Expires in the past as eviction.
		// Both flavours are acceptable; we accept either.
		evicted := ck.MaxAge < 0 || (!ck.Expires.IsZero() && ck.Expires.Before(time.Now()))
		switch ck.Name {
		case middleware.AuthCookieName:
			clearedAuth = evicted && ck.Value == ""
		case middleware.CSRFCookieName:
			clearedCSRF = evicted && ck.Value == ""
		}
	}
	if !clearedAuth || !clearedCSRF {
		t.Errorf("cookies should be cleared; auth=%v csrf=%v", clearedAuth, clearedCSRF)
	}
}

// ---------------------------------------------------------------------------
// Me tests.
// ---------------------------------------------------------------------------

func TestMe_NoCookie_401(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestMe_ValidCookie_200(t *testing.T) {
	h := newHarness(t)
	auth, _ := loginAndCollectCookies(t, h, "ada@example.com", "hunter2-very-secure")

	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: middleware.AuthCookieName, Value: auth})
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, readBody(t, resp))
	}

	body := readBody(t, resp)
	if !strings.Contains(body, `"email":"ada@example.com"`) {
		t.Errorf("body missing email: %s", body)
	}
	if strings.Contains(body, "password_hash") {
		t.Errorf("body must not include password_hash: %s", body)
	}
}

func TestMe_RevokedToken_401(t *testing.T) {
	h := newHarness(t)
	auth, csrf := loginAndCollectCookies(t, h, "ada@example.com", "hunter2-very-secure")

	// Use logout to revoke the token.
	logoutReq := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	logoutReq.AddCookie(&http.Cookie{Name: middleware.AuthCookieName, Value: auth})
	logoutReq.AddCookie(&http.Cookie{Name: middleware.CSRFCookieName, Value: csrf})
	logoutReq.Header.Set(middleware.CSRFHeaderName, csrf)
	logoutResp, _ := h.app.Test(logoutReq)
	logoutResp.Body.Close()

	// Try /me with the now-revoked cookie value.
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: middleware.AuthCookieName, Value: auth})
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (revoked); body=%s", resp.StatusCode, readBody(t, resp))
	}
}

// ---------------------------------------------------------------------------
// Construction tests.
// ---------------------------------------------------------------------------

func TestNewAuthHandler_RejectsNilDeps(t *testing.T) {
	signer, _ := jwt.NewSigner("test", time.Hour)
	if _, err := NewAuthHandler(nil, signer, CookieAttrs{}); err == nil {
		t.Error("expected error for nil usecase")
	}
	stub := newStubUsecase(t, signer)
	if _, err := NewAuthHandler(stub, nil, CookieAttrs{}); err == nil {
		t.Error("expected error for nil signer")
	}
}

func TestDefaultCookieAttrs_DevVsProd(t *testing.T) {
	dev := DefaultCookieAttrs(true)
	if dev.Secure {
		t.Error("dev cookies must NOT be Secure (plain HTTP)")
	}
	prod := DefaultCookieAttrs(false)
	if !prod.Secure {
		t.Error("prod cookies MUST be Secure")
	}
	if prod.SameSite != "Lax" {
		t.Errorf("prod SameSite = %q, want Lax", prod.SameSite)
	}
}

// ---------------------------------------------------------------------------
// DTO mapping safety net.
// ---------------------------------------------------------------------------

func TestDriverDTO_OmitsPasswordHash(t *testing.T) {
	d := &domain.Driver{
		ID: "drv_1", Email: "x@y", Name: "X", Role: domain.RoleDriver,
		PasswordHash: "should-not-leak", CreatedAt: 1, UpdatedAt: 2,
	}
	dto := toDTO(d)
	b, _ := json.Marshal(dto)
	if strings.Contains(string(b), "password_hash") {
		t.Fatalf("DTO leaked password_hash: %s", string(b))
	}
	if strings.Contains(string(b), "should-not-leak") {
		t.Fatalf("DTO leaked hash value: %s", string(b))
	}
}

func TestDriverDTO_NilSafe(t *testing.T) {
	dto := toDTO(nil)
	if dto.ID != "" || dto.Email != "" {
		t.Errorf("toDTO(nil) should return zero struct; got %+v", dto)
	}
}

// ---------------------------------------------------------------------------
// Error-path coverage with a fake usecase that lets us inject any error.
// ---------------------------------------------------------------------------

// programmableUsecase returns prefabricated errors / drivers so test cases
// can exercise specific code paths (internal errors, ErrValidation
// surfaced from Logout, etc.).
type programmableUsecase struct {
	registerFn func(ctx context.Context, email, password, name string, role domain.Role) (*domain.Driver, error)
	loginFn    func(ctx context.Context, email, password string) (string, *domain.Driver, error)
	meFn       func(ctx context.Context, userID string) (*domain.Driver, error)
	logoutFn   func(ctx context.Context, claims *jwt.Claims) error
}

func (u *programmableUsecase) Register(ctx context.Context, email, password, name string, role domain.Role) (*domain.Driver, error) {
	return u.registerFn(ctx, email, password, name, role)
}
func (u *programmableUsecase) Login(ctx context.Context, email, password string) (string, *domain.Driver, error) {
	return u.loginFn(ctx, email, password)
}
func (u *programmableUsecase) Me(ctx context.Context, userID string) (*domain.Driver, error) {
	return u.meFn(ctx, userID)
}
func (u *programmableUsecase) Logout(ctx context.Context, claims *jwt.Claims) error {
	return u.logoutFn(ctx, claims)
}

// programmableHarness builds a Fiber app with a programmableUsecase and
// real auth middleware so the wiring + handler are exercised together.
// The auth middleware always treats the token as valid via a stub
// verifier so the handler-level error-mapping logic is the only thing
// under test.
type alwaysValidVerifier struct{ subject, role string }

func (v *alwaysValidVerifier) Verify(_ string) (*jwt.Claims, error) {
	return &jwt.Claims{Subject: v.subject, Role: v.role, JTI: "test-jti-1"}, nil
}

func (v *alwaysValidVerifier) Remaining(_ *jwt.Claims) time.Duration {
	return time.Hour
}

type neverBlocked struct{}

func (n neverBlocked) IsRevoked(_ context.Context, _ *jwt.Claims) (bool, error) {
	return false, nil
}

func newProgrammableHarness(t *testing.T, uc AuthUsecase) *fiber.App {
	t.Helper()
	verifier := &alwaysValidVerifier{subject: "drv_X", role: "driver"}
	h, err := NewAuthHandler(uc, verifier, DefaultCookieAttrs(true))
	if err != nil {
		t.Fatalf("NewAuthHandler: %v", err)
	}
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(middleware.RequestID())
	app.Post("/api/auth/register", h.Register)
	app.Post("/api/auth/login", h.Login)
	authed := app.Group("", middleware.NewAuth(verifier, neverBlocked{}))
	authed.Get("/api/auth/me", h.Me)
	authed.Post("/api/auth/logout", middleware.NewCSRF(), h.Logout)
	return app
}

func TestRegister_InternalError_500(t *testing.T) {
	uc := &programmableUsecase{
		registerFn: func(context.Context, string, string, string, domain.Role) (*domain.Driver, error) {
			return nil, errors.New("database explosion")
		},
		loginFn:  func(context.Context, string, string) (string, *domain.Driver, error) { return "", nil, nil },
		meFn:     func(context.Context, string) (*domain.Driver, error) { return nil, nil },
		logoutFn: func(context.Context, *jwt.Claims) error { return nil },
	}
	app := newProgrammableHarness(t, uc)
	body := registerRequest{
		Email: "ok@example.com", Password: "long-enough-pw", Name: "Ok", Role: "driver",
	}
	resp, err := app.Test(jsonReq(t, http.MethodPost, "/api/auth/register", body))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if got := readBody(t, resp); !strings.Contains(got, `"error":"internal"`) {
		t.Errorf("body should report internal: %s", got)
	}
}

func TestRegister_UsecaseValidationError_400(t *testing.T) {
	uc := &programmableUsecase{
		registerFn: func(context.Context, string, string, string, domain.Role) (*domain.Driver, error) {
			return nil, fmt.Errorf("synthetic: %w", domain.ErrValidation)
		},
		loginFn:  func(context.Context, string, string) (string, *domain.Driver, error) { return "", nil, nil },
		meFn:     func(context.Context, string) (*domain.Driver, error) { return nil, nil },
		logoutFn: func(context.Context, *jwt.Claims) error { return nil },
	}
	app := newProgrammableHarness(t, uc)
	body := registerRequest{
		Email: "ok@example.com", Password: "long-enough-pw", Name: "Ok", Role: "driver",
	}
	resp, err := app.Test(jsonReq(t, http.MethodPost, "/api/auth/register", body))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestLogin_InternalError_500(t *testing.T) {
	uc := &programmableUsecase{
		registerFn: func(context.Context, string, string, string, domain.Role) (*domain.Driver, error) {
			return nil, nil
		},
		loginFn: func(context.Context, string, string) (string, *domain.Driver, error) {
			return "", nil, errors.New("infra failure")
		},
		meFn:     func(context.Context, string) (*domain.Driver, error) { return nil, nil },
		logoutFn: func(context.Context, *jwt.Claims) error { return nil },
	}
	app := newProgrammableHarness(t, uc)
	body := loginRequest{Email: "ok@example.com", Password: "long-enough-pw"}
	resp, err := app.Test(jsonReq(t, http.MethodPost, "/api/auth/login", body))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

func TestLogin_UsecaseValidationError_400(t *testing.T) {
	uc := &programmableUsecase{
		registerFn: func(context.Context, string, string, string, domain.Role) (*domain.Driver, error) {
			return nil, nil
		},
		loginFn: func(context.Context, string, string) (string, *domain.Driver, error) {
			return "", nil, fmt.Errorf("synthetic: %w", domain.ErrValidation)
		},
		meFn:     func(context.Context, string) (*domain.Driver, error) { return nil, nil },
		logoutFn: func(context.Context, *jwt.Claims) error { return nil },
	}
	app := newProgrammableHarness(t, uc)
	body := loginRequest{Email: "ok@example.com", Password: "long-enough-pw"}
	resp, err := app.Test(jsonReq(t, http.MethodPost, "/api/auth/login", body))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestLogout_InternalError_500(t *testing.T) {
	uc := &programmableUsecase{
		registerFn: func(context.Context, string, string, string, domain.Role) (*domain.Driver, error) {
			return nil, nil
		},
		loginFn:  func(context.Context, string, string) (string, *domain.Driver, error) { return "", nil, nil },
		meFn:     func(context.Context, string) (*domain.Driver, error) { return nil, nil },
		logoutFn: func(context.Context, *jwt.Claims) error { return errors.New("kv outage") },
	}
	app := newProgrammableHarness(t, uc)

	const csrf = "match-csrf-token-1234567890abcd"
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: middleware.AuthCookieName, Value: "any-fake-token-ok"})
	req.AddCookie(&http.Cookie{Name: middleware.CSRFCookieName, Value: csrf})
	req.Header.Set(middleware.CSRFHeaderName, csrf)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

func TestLogout_UsecaseValidationError_400(t *testing.T) {
	uc := &programmableUsecase{
		registerFn: func(context.Context, string, string, string, domain.Role) (*domain.Driver, error) {
			return nil, nil
		},
		loginFn: func(context.Context, string, string) (string, *domain.Driver, error) { return "", nil, nil },
		meFn:    func(context.Context, string) (*domain.Driver, error) { return nil, nil },
		logoutFn: func(context.Context, *jwt.Claims) error {
			return fmt.Errorf("synthetic: %w", domain.ErrValidation)
		},
	}
	app := newProgrammableHarness(t, uc)

	const csrf = "match-csrf-token-1234567890abcd"
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: middleware.AuthCookieName, Value: "any-fake-token-ok"})
	req.AddCookie(&http.Cookie{Name: middleware.CSRFCookieName, Value: csrf})
	req.Header.Set(middleware.CSRFHeaderName, csrf)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestMe_NotFound_404(t *testing.T) {
	uc := &programmableUsecase{
		registerFn: func(context.Context, string, string, string, domain.Role) (*domain.Driver, error) {
			return nil, nil
		},
		loginFn: func(context.Context, string, string) (string, *domain.Driver, error) { return "", nil, nil },
		meFn: func(context.Context, string) (*domain.Driver, error) {
			return nil, fmt.Errorf("synthetic: %w", domain.ErrNotFound)
		},
		logoutFn: func(context.Context, *jwt.Claims) error { return nil },
	}
	app := newProgrammableHarness(t, uc)
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: middleware.AuthCookieName, Value: "any-fake-token-ok"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestMe_ValidationError_400(t *testing.T) {
	uc := &programmableUsecase{
		registerFn: func(context.Context, string, string, string, domain.Role) (*domain.Driver, error) {
			return nil, nil
		},
		loginFn: func(context.Context, string, string) (string, *domain.Driver, error) { return "", nil, nil },
		meFn: func(context.Context, string) (*domain.Driver, error) {
			return nil, fmt.Errorf("synthetic: %w", domain.ErrValidation)
		},
		logoutFn: func(context.Context, *jwt.Claims) error { return nil },
	}
	app := newProgrammableHarness(t, uc)
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: middleware.AuthCookieName, Value: "any-fake-token-ok"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestMe_InternalError_500(t *testing.T) {
	uc := &programmableUsecase{
		registerFn: func(context.Context, string, string, string, domain.Role) (*domain.Driver, error) {
			return nil, nil
		},
		loginFn: func(context.Context, string, string) (string, *domain.Driver, error) { return "", nil, nil },
		meFn: func(context.Context, string) (*domain.Driver, error) {
			return nil, errors.New("db down")
		},
		logoutFn: func(context.Context, *jwt.Claims) error { return nil },
	}
	app := newProgrammableHarness(t, uc)
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: middleware.AuthCookieName, Value: "any-fake-token-ok"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

func TestLogin_BadJSON_400(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader("not-json"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestValidationMessage_FallbackForNonValidatorError(t *testing.T) {
	// A non-validator.ValidationErrors should fall back to err.Error().
	got := validationMessage(errors.New("plain error"))
	if got != "plain error" {
		t.Errorf("validationMessage fallback = %q, want %q", got, "plain error")
	}
}
