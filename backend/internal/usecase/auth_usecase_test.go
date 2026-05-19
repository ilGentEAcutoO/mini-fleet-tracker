package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/pkg/hash"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/pkg/jwt"
)

// ---------------------------------------------------------------------------
// Hand-rolled mocks. Kept here (rather than in a fixtures file) so each
// test can see them inline. The mocks are intentionally minimal — they
// implement only the methods the AuthUsecase actually calls.
// ---------------------------------------------------------------------------

type memDriverRepo struct {
	mu      sync.Mutex
	byID    map[string]*domain.Driver
	byEmail map[string]*domain.Driver
}

func newMemDriverRepo() *memDriverRepo {
	return &memDriverRepo{
		byID:    map[string]*domain.Driver{},
		byEmail: map[string]*domain.Driver{},
	}
}

func (m *memDriverRepo) Create(_ context.Context, d *domain.Driver) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byEmail[d.Email]; ok {
		return fmt.Errorf("dup email %s: %w", d.Email, domain.ErrAlreadyExists)
	}
	if _, ok := m.byID[d.ID]; ok {
		return fmt.Errorf("dup id %s: %w", d.ID, domain.ErrAlreadyExists)
	}
	// Store a copy so later test code mutating the result does not poison
	// the map.
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
	data map[string][]byte
	ttl  map[string]time.Duration
}

func newMemBlocklist() *memBlocklist {
	return &memBlocklist{
		data: map[string][]byte{},
		ttl:  map[string]time.Duration{},
	}
}

func (m *memBlocklist) Put(_ context.Context, key string, value []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := append([]byte(nil), value...)
	m.data[key] = cp
	m.ttl[key] = ttl
	return nil
}

func (m *memBlocklist) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[key]
	if !ok {
		return nil, false, nil
	}
	cp := append([]byte(nil), v...)
	return cp, true, nil
}

// counterIDs hands out monotonic IDs so tests can assert against the
// generated driver ID without depending on UUID randomness.
type counterIDs struct {
	prefix string
	n      int
	mu     sync.Mutex
}

func newCounterIDs(prefix string) *counterIDs { return &counterIDs{prefix: prefix} }

func (c *counterIDs) NewID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return fmt.Sprintf("%s_%03d", c.prefix, c.n)
}

// ---------------------------------------------------------------------------
// Test helpers.
// ---------------------------------------------------------------------------

func newTestAuth(t *testing.T) (*AuthUsecase, *memDriverRepo, *memBlocklist, *jwt.Signer) {
	t.Helper()
	repo := newMemDriverRepo()
	bl := newMemBlocklist()
	signer, err := jwt.NewSigner("test-secret-please-replace", time.Hour)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	uc, err := NewAuthUsecase(repo, signer, bl, newCounterIDs("drv"))
	if err != nil {
		t.Fatalf("NewAuthUsecase: %v", err)
	}
	return uc, repo, bl, signer
}

// ---------------------------------------------------------------------------
// Construction.
// ---------------------------------------------------------------------------

func TestNewAuthUsecase_RejectsNilDependencies(t *testing.T) {
	repo := newMemDriverRepo()
	bl := newMemBlocklist()
	signer, _ := jwt.NewSigner("test", time.Hour)
	ids := newCounterIDs("drv")

	cases := []struct {
		name string
		drv  DriverRepo
		sig  TokenSigner
		bk   Blocklist
		id   IDGenerator
	}{
		{"nil drivers", nil, signer, bl, ids},
		{"nil signer", repo, nil, bl, ids},
		{"nil blocklist", repo, signer, nil, ids},
		{"nil ids", repo, signer, bl, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewAuthUsecase(tc.drv, tc.sig, tc.bk, tc.id); err == nil {
				t.Fatalf("NewAuthUsecase should reject %s", tc.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Register.
// ---------------------------------------------------------------------------

func TestRegister_Success(t *testing.T) {
	uc, repo, _, _ := newTestAuth(t)
	d, err := uc.Register(context.Background(), "Ada@Example.COM", "secret123!", " Ada Lovelace ", domain.RoleManager)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Email should be lower-cased; name should be trimmed.
	if d.Email != "ada@example.com" {
		t.Fatalf("email not lower-cased: %q", d.Email)
	}
	if d.Name != "Ada Lovelace" {
		t.Fatalf("name not trimmed: %q", d.Name)
	}
	if d.Role != domain.RoleManager {
		t.Fatalf("role: want manager, got %q", d.Role)
	}
	if d.ID == "" {
		t.Fatal("ID must be populated")
	}
	if d.PasswordHash == "" {
		t.Fatal("PasswordHash must be populated")
	}
	if !hash.VerifyPassword("secret123!", d.PasswordHash) {
		t.Fatal("returned hash must verify against the source password")
	}
	if _, ok := repo.byID[d.ID]; !ok {
		t.Fatal("driver not persisted in repo")
	}
}

func TestRegister_DuplicateEmail(t *testing.T) {
	uc, _, _, _ := newTestAuth(t)
	if _, err := uc.Register(context.Background(), "ada@example.com", "pw", "Ada", domain.RoleDriver); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	_, err := uc.Register(context.Background(), "ada@example.com", "pw", "Ada", domain.RoleDriver)
	if err == nil {
		t.Fatal("second Register with same email must fail")
	}
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists, got: %v", err)
	}
}

func TestRegister_InvalidRole(t *testing.T) {
	uc, _, _, _ := newTestAuth(t)
	_, err := uc.Register(context.Background(), "rogue@example.com", "pw", "Rogue", "admin")
	if err == nil {
		t.Fatal("Register with invalid role must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

func TestRegister_RequiredFields(t *testing.T) {
	uc, _, _, _ := newTestAuth(t)
	cases := []struct {
		name                 string
		email, password, who string
	}{
		{"empty email", "", "pw", "n"},
		{"whitespace email", "   ", "pw", "n"},
		{"empty password", "a@b", "", "n"},
		{"empty name", "a@b", "pw", "  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := uc.Register(context.Background(), tc.email, tc.password, tc.who, domain.RoleDriver)
			if err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("%s: expected ErrValidation, got: %v", tc.name, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Login.
// ---------------------------------------------------------------------------

func TestLogin_Success(t *testing.T) {
	uc, _, _, signer := newTestAuth(t)

	d, err := uc.Register(context.Background(), "ada@example.com", "secret123!", "Ada", domain.RoleManager)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	token, got, err := uc.Login(context.Background(), "ada@example.com", "secret123!")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}
	if got.ID != d.ID {
		t.Fatalf("returned driver ID mismatch: want %q, got %q", d.ID, got.ID)
	}

	// Decode the token via the same signer — claims must reflect the
	// driver's identity and role, JTI must be populated, exp must be in
	// the future.
	claims, err := signer.Verify(token)
	if err != nil {
		t.Fatalf("Verify token: %v", err)
	}
	if claims.Subject != d.ID {
		t.Fatalf("token sub: want %q, got %q", d.ID, claims.Subject)
	}
	if claims.Role != string(domain.RoleManager) {
		t.Fatalf("token role: want manager, got %q", claims.Role)
	}
	if claims.JTI == "" {
		t.Fatal("token jti must be populated")
	}
	if claims.ExpiresAt == nil || claims.ExpiresAt.Before(time.Now()) {
		t.Fatal("token exp must be in the future")
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	uc, _, _, _ := newTestAuth(t)
	if _, err := uc.Register(context.Background(), "ada@example.com", "right-pw", "Ada", domain.RoleDriver); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, _, err := uc.Login(context.Background(), "ada@example.com", "wrong-pw")
	if err == nil {
		t.Fatal("Login with wrong password must fail")
	}
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got: %v", err)
	}
}

func TestLogin_UnknownEmail(t *testing.T) {
	uc, _, _, _ := newTestAuth(t)
	_, _, err := uc.Login(context.Background(), "ghost@example.com", "anything")
	if err == nil {
		t.Fatal("Login with unknown email must fail")
	}
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized (same as wrong-password), got: %v", err)
	}
}

func TestLogin_EmptyInputs(t *testing.T) {
	uc, _, _, _ := newTestAuth(t)
	for _, tc := range []struct{ email, pw string }{
		{"", "pw"},
		{"a@b", ""},
		{"   ", "pw"},
	} {
		_, _, err := uc.Login(context.Background(), tc.email, tc.pw)
		if err == nil {
			t.Fatalf("Login(%q, %q) should fail", tc.email, tc.pw)
		}
		if !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatalf("Login(%q, %q): expected ErrUnauthorized, got: %v", tc.email, tc.pw, err)
		}
	}
}

func TestLogin_EmailLowercased(t *testing.T) {
	uc, _, _, _ := newTestAuth(t)
	if _, err := uc.Register(context.Background(), "ada@example.com", "pw", "Ada", domain.RoleDriver); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Caller submits the email in mixed case — Login must lower-case
	// before lookup.
	tok, _, err := uc.Login(context.Background(), "Ada@Example.COM", "pw")
	if err != nil {
		t.Fatalf("Login with mixed-case email: %v", err)
	}
	if tok == "" {
		t.Fatal("expected token")
	}
}

// ---------------------------------------------------------------------------
// Me.
// ---------------------------------------------------------------------------

func TestMe_Success(t *testing.T) {
	uc, _, _, _ := newTestAuth(t)
	d, err := uc.Register(context.Background(), "ada@example.com", "pw", "Ada", domain.RoleDriver)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, err := uc.Me(context.Background(), d.ID)
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if got.ID != d.ID {
		t.Fatalf("Me returned wrong driver: got %q, want %q", got.ID, d.ID)
	}
}

func TestMe_NotFound(t *testing.T) {
	uc, _, _, _ := newTestAuth(t)
	_, err := uc.Me(context.Background(), "drv_does_not_exist")
	if err == nil {
		t.Fatal("Me on missing id must fail")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestMe_EmptyID(t *testing.T) {
	uc, _, _, _ := newTestAuth(t)
	_, err := uc.Me(context.Background(), "   ")
	if err == nil {
		t.Fatal("Me with empty id must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Logout.
// ---------------------------------------------------------------------------

func TestLogout_Success(t *testing.T) {
	uc, _, bl, _ := newTestAuth(t)
	if _, err := uc.Register(context.Background(), "ada@example.com", "pw", "Ada", domain.RoleDriver); err != nil {
		t.Fatalf("Register: %v", err)
	}
	token, _, err := uc.Login(context.Background(), "ada@example.com", "pw")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// Verify the token to extract the claims, then logout.
	signer, _ := jwt.NewSigner("test-secret-please-replace", time.Hour)
	claims, err := signer.Verify(token)
	if err != nil {
		t.Fatalf("Verify token: %v", err)
	}
	if err := uc.Logout(context.Background(), claims); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	// The blocklist should now contain bl:{jti} with a positive TTL.
	key := "bl:" + claims.JTI
	bl.mu.Lock()
	defer bl.mu.Unlock()
	if _, ok := bl.data[key]; !ok {
		t.Fatalf("expected blocklist entry at %q", key)
	}
	ttl := bl.ttl[key]
	if ttl <= 0 || ttl > time.Hour {
		t.Fatalf("blocklist TTL out of expected range: got %v, want (0, 1h]", ttl)
	}
}

func TestLogout_NilClaims(t *testing.T) {
	uc, _, _, _ := newTestAuth(t)
	err := uc.Logout(context.Background(), nil)
	if err == nil {
		t.Fatal("Logout(nil) must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

func TestLogout_MissingJTI(t *testing.T) {
	uc, _, _, _ := newTestAuth(t)
	err := uc.Logout(context.Background(), &jwt.Claims{})
	if err == nil {
		t.Fatal("Logout with empty JTI must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

func TestLogout_Idempotent(t *testing.T) {
	uc, _, bl, signer := newTestAuth(t)
	if _, err := uc.Register(context.Background(), "ada@example.com", "pw", "Ada", domain.RoleDriver); err != nil {
		t.Fatalf("Register: %v", err)
	}
	token, _, err := uc.Login(context.Background(), "ada@example.com", "pw")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	claims, err := signer.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if err := uc.Logout(context.Background(), claims); err != nil {
		t.Fatalf("first Logout: %v", err)
	}
	// Second call must succeed too (overwrite is fine).
	if err := uc.Logout(context.Background(), claims); err != nil {
		t.Fatalf("second Logout: %v", err)
	}
	// Still exactly one entry.
	bl.mu.Lock()
	defer bl.mu.Unlock()
	if len(bl.data) != 1 {
		t.Fatalf("expected 1 blocklist entry after idempotent logout, got %d", len(bl.data))
	}
}

// ---------------------------------------------------------------------------
// IsRevoked.
// ---------------------------------------------------------------------------

func TestIsRevoked(t *testing.T) {
	uc, _, _, signer := newTestAuth(t)
	if _, err := uc.Register(context.Background(), "ada@example.com", "pw", "Ada", domain.RoleDriver); err != nil {
		t.Fatalf("Register: %v", err)
	}
	token, _, err := uc.Login(context.Background(), "ada@example.com", "pw")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	claims, err := signer.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	// Before logout: not revoked.
	revoked, err := uc.IsRevoked(context.Background(), claims)
	if err != nil {
		t.Fatalf("IsRevoked: %v", err)
	}
	if revoked {
		t.Fatal("token should not be revoked before logout")
	}

	if logoutErr := uc.Logout(context.Background(), claims); logoutErr != nil {
		t.Fatalf("Logout: %v", logoutErr)
	}

	// After logout: revoked.
	revoked, err = uc.IsRevoked(context.Background(), claims)
	if err != nil {
		t.Fatalf("IsRevoked: %v", err)
	}
	if !revoked {
		t.Fatal("token should be revoked after logout")
	}

	// Nil claims: not revoked, no error.
	nilRevoked, nilErr := uc.IsRevoked(context.Background(), nil)
	if nilErr != nil || nilRevoked {
		t.Fatalf("IsRevoked(nil): want (false, nil), got (%v, %v)", nilRevoked, nilErr)
	}
}

// ---------------------------------------------------------------------------
// init() — guard against silent dummy-hash misconfiguration.
// ---------------------------------------------------------------------------

func TestDummyHashInit_NoError(t *testing.T) {
	if err := dummyHashInitError(); err != nil {
		t.Fatalf("dummyHash init failed: %v", err)
	}
	if !strings.HasPrefix(dummyHashValue, "$argon2id$") {
		t.Fatalf("dummyHashValue must be a PHC string, got %q", dummyHashValue)
	}
}
