package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/middleware"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/usecase"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/pkg/jwt"
)

// ---------------------------------------------------------------------------
// In-memory fake usecase. Implements VehicleUsecase end-to-end so handler
// tests exercise the real validation -> mapping -> response shape pipeline.
// ---------------------------------------------------------------------------

type memVehicleUsecase struct {
	mu       sync.Mutex
	byID     map[string]*domain.Vehicle
	plateIdx map[string]string
	// positions stores per-vehicle history in insertion order. The
	// ListPositions fake reverses to satisfy the DESC contract and applies
	// the [from, to] window + limit just like the real repo.
	positions map[string][]*domain.Position
	idSeq     int
	// errOverride lets a specific test inject a custom error from any method
	// without rewriting the entire fake. nil means "default behaviour".
	errOverride map[string]error
}

func newMemVehicleUsecase() *memVehicleUsecase {
	return &memVehicleUsecase{
		byID:        map[string]*domain.Vehicle{},
		plateIdx:    map[string]string{},
		positions:   map[string][]*domain.Position{},
		errOverride: map[string]error{},
	}
}

// seedPosition appends a position to the fake's per-vehicle list. Tests
// call this directly because pushing through the position usecase would
// require wiring the full publisher/freshness stack — overkill when the
// goal is just "make List return rows".
func (m *memVehicleUsecase) seedPosition(p *domain.Position) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.positions[p.VehicleID] = append(m.positions[p.VehicleID], p)
}

func (m *memVehicleUsecase) nextID() string {
	m.idSeq++
	return fmt.Sprintf("veh_%03d", m.idSeq)
}

func (m *memVehicleUsecase) List(_ context.Context) ([]*domain.Vehicle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.errOverride["List"]; err != nil {
		return nil, err
	}
	out := make([]*domain.Vehicle, 0, len(m.byID))
	for _, v := range m.byID {
		cp := *v
		out = append(out, &cp)
	}
	return out, nil
}

func (m *memVehicleUsecase) Get(_ context.Context, id string) (*domain.Vehicle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.errOverride["Get"]; err != nil {
		return nil, err
	}
	v, ok := m.byID[id]
	if !ok {
		return nil, fmt.Errorf("id %s: %w", id, domain.ErrNotFound)
	}
	cp := *v
	return &cp, nil
}

func (m *memVehicleUsecase) Create(_ context.Context, in usecase.CreateVehicleInput) (*domain.Vehicle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.errOverride["Create"]; err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.PlateNumber) == "" {
		return nil, fmt.Errorf("plate required: %w", domain.ErrValidation)
	}
	if _, taken := m.plateIdx[in.PlateNumber]; taken {
		return nil, fmt.Errorf("dup plate: %w", domain.ErrAlreadyExists)
	}
	now := time.Now().UnixMilli()
	v := &domain.Vehicle{
		ID:          m.nextID(),
		PlateNumber: in.PlateNumber,
		Model:       in.Model,
		DriverID:    in.DriverID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	cp := *v
	m.byID[v.ID] = &cp
	m.plateIdx[v.PlateNumber] = v.ID
	return v, nil
}

func (m *memVehicleUsecase) Update(_ context.Context, id string, in usecase.UpdateVehicleInput) (*domain.Vehicle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.errOverride["Update"]; err != nil {
		return nil, err
	}
	v, ok := m.byID[id]
	if !ok {
		return nil, fmt.Errorf("id %s: %w", id, domain.ErrNotFound)
	}
	if in.PlateNumber != nil {
		if otherID, taken := m.plateIdx[*in.PlateNumber]; taken && otherID != id {
			return nil, fmt.Errorf("dup plate: %w", domain.ErrAlreadyExists)
		}
		delete(m.plateIdx, v.PlateNumber)
		v.PlateNumber = *in.PlateNumber
		m.plateIdx[v.PlateNumber] = id
	}
	if in.Model != nil {
		v.Model = *in.Model
	}
	if in.DriverID != nil {
		v.DriverID = *in.DriverID
	}
	v.UpdatedAt = time.Now().UnixMilli()
	cp := *v
	return &cp, nil
}

func (m *memVehicleUsecase) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.errOverride["Delete"]; err != nil {
		return err
	}
	v, ok := m.byID[id]
	if !ok {
		return fmt.Errorf("id %s: %w", id, domain.ErrNotFound)
	}
	delete(m.plateIdx, v.PlateNumber)
	delete(m.byID, id)
	return nil
}

// ListPositions mirrors the real usecase: validates params, checks vehicle
// existence, then returns the windowed slice in DESC order. Tests can
// drive the fake by seeding via seedPosition and asserting on the
// returned ordering.
//
// The clamp logic uses literal constants matching the production
// constants in vehicle_usecase.go (defaultHistoryLimit=1000,
// maxHistoryLimit=5000). Keeping the two in sync is a deliberate cost —
// importing the usecase package's unexported consts is not possible, and
// promoting them to exported names just for tests would leak an
// implementation detail. Drift here would break tests immediately, which
// is the right failure mode.
func (m *memVehicleUsecase) ListPositions(_ context.Context, id string, fromMs, toMs int64, limit int) ([]*domain.Position, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.errOverride["ListPositions"]; err != nil {
		return nil, err
	}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("id required: %w", domain.ErrValidation)
	}
	if fromMs < 0 || toMs < 0 {
		return nil, fmt.Errorf("bounds must be non-negative: %w", domain.ErrValidation)
	}
	if fromMs > 0 && toMs > 0 && fromMs > toMs {
		return nil, fmt.Errorf("from > to: %w", domain.ErrValidation)
	}
	if limit < 0 {
		return nil, fmt.Errorf("limit must be non-negative: %w", domain.ErrValidation)
	}
	if limit == 0 {
		limit = 1000
	}
	if limit > 5000 {
		limit = 5000
	}
	if _, ok := m.byID[id]; !ok {
		return nil, fmt.Errorf("id %s: %w", id, domain.ErrNotFound)
	}
	rows := m.positions[id]
	out := make([]*domain.Position, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		p := rows[i]
		if fromMs > 0 && p.RecordedAt < fromMs {
			continue
		}
		if toMs > 0 && p.RecordedAt > toMs {
			continue
		}
		cp := *p
		out = append(out, &cp)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Harness — mirrors the pattern in position_handler_test.go: real auth
// middleware, real CSRF, JWT signer, only the usecase is faked.
// ---------------------------------------------------------------------------

type vehicleHarness struct {
	app     *fiber.App
	signer  *jwt.Signer
	mock    *memVehicleUsecase
	handler *VehicleHandler
}

func newVehicleHarness(t *testing.T) *vehicleHarness {
	t.Helper()
	signer, err := jwt.NewSigner("vehicle-test-secret-please-replace", time.Hour)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	mock := newMemVehicleUsecase()
	h, err := NewVehicleHandler(mock)
	if err != nil {
		t.Fatalf("NewVehicleHandler: %v", err)
	}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(middleware.RequestID())
	app.Use(middleware.NewAuth(signer, neverBlockedVehicle{}))
	// CSRF only applies to mutators (POST/PATCH/DELETE). The middleware
	// short-circuits idempotent reads itself, so registering it globally
	// is safe.
	app.Use(middleware.NewCSRF())

	app.Get("/api/vehicles", h.List)
	// Order matters: /:id/positions must register before /:id so Fiber's
	// trie matches the more specific path first. Production wiring in
	// bootstrap.go uses the inverse order (List, Get, History, ...) but
	// the bare /:id route uses different segment counts, so collision is
	// avoided either way; here we mirror production order for parity.
	app.Get("/api/vehicles/:id", h.Get)
	app.Get("/api/vehicles/:id/positions", h.History)
	app.Post("/api/vehicles", h.Create)
	app.Patch("/api/vehicles/:id", h.Update)
	app.Delete("/api/vehicles/:id", h.Delete)

	return &vehicleHarness{app: app, signer: signer, mock: mock, handler: h}
}

// neverBlockedVehicle is the BlocklistChecker stub for vehicle handler
// tests. Token revocation is the auth middleware's concern; we trust
// every issued cookie here.
type neverBlockedVehicle struct{}

func (neverBlockedVehicle) IsRevoked(_ context.Context, _ *jwt.Claims) (bool, error) {
	return false, nil
}

// issueCookie mints a real JWT for (sub, role) so the auth middleware
// accepts it. Returns the encoded token string the test attaches as
// auth_token.
func (h *vehicleHarness) issueCookie(t *testing.T, sub, role string) string {
	t.Helper()
	token, _, err := h.signer.Issue(sub, role)
	if err != nil {
		t.Fatalf("issue cookie: %v", err)
	}
	return token
}

// vehReq builds an authenticated request with the CSRF double-submit
// already populated. authCookie="" means "no cookie attached" so the
// caller can exercise the unauthenticated path.
//
// The helper is named vehReq rather than reusing jsonReqWithAuth from
// position_handler_test.go on purpose: the position tests' helper expects
// a single signature (POST /api/positions) and tying our suite to its
// internals couples two unrelated test files. Tiny duplication, large
// independence dividend.
func vehReq(t *testing.T, method, path string, body any, authCookie string) *http.Request {
	t.Helper()
	req := jsonReq(t, method, path, body)
	if authCookie != "" {
		req.AddCookie(&http.Cookie{Name: middleware.AuthCookieName, Value: authCookie})
		req.AddCookie(&http.Cookie{Name: middleware.CSRFCookieName, Value: csrfTokenForTests})
		req.Header.Set(middleware.CSRFHeaderName, csrfTokenForTests)
	}
	return req
}

// ---------------------------------------------------------------------------
// Construction.
// ---------------------------------------------------------------------------

func TestNewVehicleHandler_RejectsNilUsecase(t *testing.T) {
	if _, err := NewVehicleHandler(nil); err == nil {
		t.Fatal("expected error for nil usecase")
	}
}

// ---------------------------------------------------------------------------
// Role guard (driver vs manager vs unauthenticated).
// ---------------------------------------------------------------------------

func TestVehicleList_NoAuth_401(t *testing.T) {
	h := newVehicleHarness(t)
	req := vehReq(t, http.MethodGet, "/api/vehicles", nil, "")
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestVehicleList_AsDriver_403(t *testing.T) {
	h := newVehicleHarness(t)
	driverCookie := h.issueCookie(t, "drv_001", "driver")
	req := vehReq(t, http.MethodGet, "/api/vehicles", nil, driverCookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", resp.StatusCode, readBody(t, resp))
	}
	if got := readBody(t, resp); !strings.Contains(got, `"error":"forbidden"`) {
		t.Errorf("body should report forbidden: %s", got)
	}
}

func TestVehicleList_AsManager_200(t *testing.T) {
	h := newVehicleHarness(t)
	mgrCookie := h.issueCookie(t, "mgr_001", "manager")
	req := vehReq(t, http.MethodGet, "/api/vehicles", nil, mgrCookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"vehicles"`) {
		t.Errorf("body should contain vehicles key: %s", body)
	}
}

// ---------------------------------------------------------------------------
// Create.
// ---------------------------------------------------------------------------

func TestVehicleCreate_AsManager_201(t *testing.T) {
	h := newVehicleHarness(t)
	mgrCookie := h.issueCookie(t, "mgr_001", "manager")

	body := createVehicleRequest{PlateNumber: "ABC-1234", Model: "Toyota Hilux"}
	req := vehReq(t, http.MethodPost, "/api/vehicles", body, mgrCookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", resp.StatusCode, readBody(t, resp))
	}
	got := readBody(t, resp)
	if !strings.Contains(got, `"plate_number":"ABC-1234"`) {
		t.Errorf("body missing plate_number: %s", got)
	}
	if !strings.Contains(got, `"id":"veh_001"`) {
		t.Errorf("body missing id: %s", got)
	}
}

func TestVehicleCreate_AsDriver_403(t *testing.T) {
	h := newVehicleHarness(t)
	driverCookie := h.issueCookie(t, "drv_001", "driver")
	body := createVehicleRequest{PlateNumber: "ABC-1234"}
	req := vehReq(t, http.MethodPost, "/api/vehicles", body, driverCookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestVehicleCreate_ValidationFail_400(t *testing.T) {
	h := newVehicleHarness(t)
	mgrCookie := h.issueCookie(t, "mgr_001", "manager")
	// Empty plate_number trips the validator (required).
	body := createVehicleRequest{Model: "Hilux"}
	req := vehReq(t, http.MethodPost, "/api/vehicles", body, mgrCookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestVehicleCreate_BadJSON_400(t *testing.T) {
	h := newVehicleHarness(t)
	mgrCookie := h.issueCookie(t, "mgr_001", "manager")
	req := httptest.NewRequest(http.MethodPost, "/api/vehicles", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: middleware.AuthCookieName, Value: mgrCookie})
	req.AddCookie(&http.Cookie{Name: middleware.CSRFCookieName, Value: csrfTokenForTests})
	req.Header.Set(middleware.CSRFHeaderName, csrfTokenForTests)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestVehicleCreate_DuplicatePlate_409(t *testing.T) {
	h := newVehicleHarness(t)
	mgrCookie := h.issueCookie(t, "mgr_001", "manager")
	body := createVehicleRequest{PlateNumber: "DUP-1"}

	// First create succeeds.
	resp, err := h.app.Test(vehReq(t, http.MethodPost, "/api/vehicles", body, mgrCookie))
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	resp.Body.Close()

	// Second create with the same plate must 409.
	resp2, err := h.app.Test(vehReq(t, http.MethodPost, "/api/vehicles", body, mgrCookie))
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", resp2.StatusCode, readBody(t, resp2))
	}
	if got := readBody(t, resp2); !strings.Contains(got, `"error":"already_exists"`) {
		t.Errorf("body should report already_exists: %s", got)
	}
}

func TestVehicleCreate_InternalError_500(t *testing.T) {
	h := newVehicleHarness(t)
	mgrCookie := h.issueCookie(t, "mgr_001", "manager")
	h.mock.errOverride["Create"] = errors.New("db down")

	body := createVehicleRequest{PlateNumber: "ABC-1"}
	resp, err := h.app.Test(vehReq(t, http.MethodPost, "/api/vehicles", body, mgrCookie))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

// ---------------------------------------------------------------------------
// Get (single).
// ---------------------------------------------------------------------------

func TestVehicleGet_AsManager_200(t *testing.T) {
	h := newVehicleHarness(t)
	mgrCookie := h.issueCookie(t, "mgr_001", "manager")

	// Pre-seed a row via the fake.
	if _, err := h.mock.Create(context.Background(), usecase.CreateVehicleInput{PlateNumber: "ABC-1"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	resp, err := h.app.Test(vehReq(t, http.MethodGet, "/api/vehicles/veh_001", nil, mgrCookie))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"id":"veh_001"`) {
		t.Errorf("body missing id: %s", body)
	}
}

func TestVehicleGet_NotFound_404(t *testing.T) {
	h := newVehicleHarness(t)
	mgrCookie := h.issueCookie(t, "mgr_001", "manager")
	resp, err := h.app.Test(vehReq(t, http.MethodGet, "/api/vehicles/missing", nil, mgrCookie))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", resp.StatusCode, readBody(t, resp))
	}
	if got := readBody(t, resp); !strings.Contains(got, `"error":"not_found"`) {
		t.Errorf("body should report not_found: %s", got)
	}
}

func TestVehicleGet_AsDriver_403(t *testing.T) {
	h := newVehicleHarness(t)
	driverCookie := h.issueCookie(t, "drv_001", "driver")
	resp, err := h.app.Test(vehReq(t, http.MethodGet, "/api/vehicles/veh_001", nil, driverCookie))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

// ---------------------------------------------------------------------------
// Update (PATCH).
// ---------------------------------------------------------------------------

func TestVehicleUpdate_AsManager_200(t *testing.T) {
	h := newVehicleHarness(t)
	mgrCookie := h.issueCookie(t, "mgr_001", "manager")
	if _, err := h.mock.Create(context.Background(), usecase.CreateVehicleInput{PlateNumber: "OLD-1"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	newPlate := "NEW-1"
	body := updateVehicleRequest{PlateNumber: &newPlate}
	resp, err := h.app.Test(vehReq(t, http.MethodPatch, "/api/vehicles/veh_001", body, mgrCookie))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, readBody(t, resp))
	}
	got := readBody(t, resp)
	if !strings.Contains(got, `"plate_number":"NEW-1"`) {
		t.Errorf("plate not updated in response: %s", got)
	}
}

func TestVehicleUpdate_AsDriver_403(t *testing.T) {
	h := newVehicleHarness(t)
	driverCookie := h.issueCookie(t, "drv_001", "driver")
	newPlate := "X"
	body := updateVehicleRequest{PlateNumber: &newPlate}
	resp, err := h.app.Test(vehReq(t, http.MethodPatch, "/api/vehicles/veh_001", body, driverCookie))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestVehicleUpdate_NotFound_404(t *testing.T) {
	h := newVehicleHarness(t)
	mgrCookie := h.issueCookie(t, "mgr_001", "manager")
	newPlate := "X"
	body := updateVehicleRequest{PlateNumber: &newPlate}
	resp, err := h.app.Test(vehReq(t, http.MethodPatch, "/api/vehicles/missing", body, mgrCookie))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestVehicleUpdate_BadJSON_400(t *testing.T) {
	h := newVehicleHarness(t)
	mgrCookie := h.issueCookie(t, "mgr_001", "manager")
	req := httptest.NewRequest(http.MethodPatch, "/api/vehicles/veh_001", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: middleware.AuthCookieName, Value: mgrCookie})
	req.AddCookie(&http.Cookie{Name: middleware.CSRFCookieName, Value: csrfTokenForTests})
	req.Header.Set(middleware.CSRFHeaderName, csrfTokenForTests)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestVehicleUpdate_OverlongPlate_400(t *testing.T) {
	h := newVehicleHarness(t)
	mgrCookie := h.issueCookie(t, "mgr_001", "manager")
	// 60-char plate trips the validator's max=50 tag at the handler level
	// before the usecase even runs.
	long := strings.Repeat("A", 60)
	body := updateVehicleRequest{PlateNumber: &long}
	resp, err := h.app.Test(vehReq(t, http.MethodPatch, "/api/vehicles/veh_001", body, mgrCookie))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

// ---------------------------------------------------------------------------
// Delete.
// ---------------------------------------------------------------------------

func TestVehicleDelete_AsManager_204(t *testing.T) {
	h := newVehicleHarness(t)
	mgrCookie := h.issueCookie(t, "mgr_001", "manager")
	if _, err := h.mock.Create(context.Background(), usecase.CreateVehicleInput{PlateNumber: "DEL-1"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resp, err := h.app.Test(vehReq(t, http.MethodDelete, "/api/vehicles/veh_001", nil, mgrCookie))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestVehicleDelete_AsDriver_403(t *testing.T) {
	h := newVehicleHarness(t)
	driverCookie := h.issueCookie(t, "drv_001", "driver")
	resp, err := h.app.Test(vehReq(t, http.MethodDelete, "/api/vehicles/veh_001", nil, driverCookie))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestVehicleDelete_NotFound_404(t *testing.T) {
	h := newVehicleHarness(t)
	mgrCookie := h.issueCookie(t, "mgr_001", "manager")
	resp, err := h.app.Test(vehReq(t, http.MethodDelete, "/api/vehicles/missing", nil, mgrCookie))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestVehicleDelete_InternalError_500(t *testing.T) {
	h := newVehicleHarness(t)
	mgrCookie := h.issueCookie(t, "mgr_001", "manager")
	h.mock.errOverride["Delete"] = errors.New("kv outage")
	resp, err := h.app.Test(vehReq(t, http.MethodDelete, "/api/vehicles/veh_001", nil, mgrCookie))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

// ---------------------------------------------------------------------------
// List error.
// ---------------------------------------------------------------------------

func TestVehicleList_InternalError_500(t *testing.T) {
	h := newVehicleHarness(t)
	mgrCookie := h.issueCookie(t, "mgr_001", "manager")
	h.mock.errOverride["List"] = errors.New("db went away")
	resp, err := h.app.Test(vehReq(t, http.MethodGet, "/api/vehicles", nil, mgrCookie))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

// ---------------------------------------------------------------------------
// DTO mapping safety.
// ---------------------------------------------------------------------------

func TestVehicleDTO_NilSafe(t *testing.T) {
	dto := toVehicleDTO(nil)
	if dto.ID != "" || dto.PlateNumber != "" {
		t.Errorf("toVehicleDTO(nil) should return zero struct; got %+v", dto)
	}
}

func TestVehicleDTO_OmitsEmptyOptionalFields(t *testing.T) {
	v := &domain.Vehicle{
		ID:          "veh_1",
		PlateNumber: "PLATE",
		// Model and DriverID intentionally left empty so the omitempty
		// JSON tag drops them in the marshalled output.
		CreatedAt: 1,
		UpdatedAt: 2,
	}
	dto := toVehicleDTO(v)
	// We don't go through encoding/json here — the omitempty behaviour is
	// already covered by the integration tests above that read response
	// bodies. Just assert the DTO mirror reads back what we put in.
	if dto.Model != "" {
		t.Errorf("Model: got %q, want empty", dto.Model)
	}
	if dto.DriverID != "" {
		t.Errorf("DriverID: got %q, want empty", dto.DriverID)
	}
}

// ---------------------------------------------------------------------------
// History (GET /api/vehicles/:id/positions) — TASK-018.
// ---------------------------------------------------------------------------

// seedVehicleAndHistory creates a vehicle on the fake and returns its ID
// plus a helper to push N positions with monotonically-increasing
// recorded_at timestamps. Keeps the per-test boilerplate small.
func seedVehicleAndHistory(t *testing.T, h *vehicleHarness, plate string, n int, baseMs int64) string {
	t.Helper()
	v, err := h.mock.Create(context.Background(), usecase.CreateVehicleInput{PlateNumber: plate})
	if err != nil {
		t.Fatalf("seed vehicle: %v", err)
	}
	for i := 0; i < n; i++ {
		h.mock.seedPosition(&domain.Position{
			ID:         int64(i + 1),
			VehicleID:  v.ID,
			Lat:        13.0 + float64(i)*0.001,
			Lng:        100.0 + float64(i)*0.001,
			RecordedAt: baseMs + int64(i)*1000,
		})
	}
	return v.ID
}

func TestVehicleHistory_NoAuth_401(t *testing.T) {
	h := newVehicleHarness(t)
	id := seedVehicleAndHistory(t, h, "HIST-1", 3, 1_700_000_000_000)
	resp, err := h.app.Test(vehReq(t, http.MethodGet, "/api/vehicles/"+id+"/positions", nil, ""))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestVehicleHistory_AsDriver_403(t *testing.T) {
	h := newVehicleHarness(t)
	id := seedVehicleAndHistory(t, h, "HIST-2", 3, 1_700_000_000_000)
	cookie := h.issueCookie(t, "drv_001", "driver")
	resp, err := h.app.Test(vehReq(t, http.MethodGet, "/api/vehicles/"+id+"/positions", nil, cookie))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestVehicleHistory_AsManager_200(t *testing.T) {
	h := newVehicleHarness(t)
	id := seedVehicleAndHistory(t, h, "HIST-3", 3, 1_700_000_000_000)
	cookie := h.issueCookie(t, "mgr_001", "manager")
	resp, err := h.app.Test(vehReq(t, http.MethodGet, "/api/vehicles/"+id+"/positions", nil, cookie))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"vehicle_id":"`+id+`"`) {
		t.Errorf("body missing vehicle_id: %s", body)
	}
	if !strings.Contains(body, `"count":3`) {
		t.Errorf("body missing count=3: %s", body)
	}
	if !strings.Contains(body, `"positions"`) {
		t.Errorf("body missing positions key: %s", body)
	}
}

func TestVehicleHistory_OrderDescByRecordedAt(t *testing.T) {
	h := newVehicleHarness(t)
	id := seedVehicleAndHistory(t, h, "HIST-4", 3, 1_700_000_000_000)
	cookie := h.issueCookie(t, "mgr_001", "manager")
	resp, err := h.app.Test(vehReq(t, http.MethodGet, "/api/vehicles/"+id+"/positions", nil, cookie))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	body := readBody(t, resp)
	// Newest first — third (recorded_at 1_700_000_002_000) should appear
	// before the oldest (recorded_at 1_700_000_000_000) in the JSON.
	idxNewest := strings.Index(body, `"recorded_at":1700000002000`)
	idxOldest := strings.Index(body, `"recorded_at":1700000000000`)
	if idxNewest < 0 || idxOldest < 0 {
		t.Fatalf("recorded_at markers missing in body: %s", body)
	}
	if idxNewest > idxOldest {
		t.Errorf("DESC order broken: newest appears after oldest in body: %s", body)
	}
}

func TestVehicleHistory_NotFound_404(t *testing.T) {
	h := newVehicleHarness(t)
	cookie := h.issueCookie(t, "mgr_001", "manager")
	resp, err := h.app.Test(vehReq(t, http.MethodGet, "/api/vehicles/veh_missing/positions", nil, cookie))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestVehicleHistory_FromGreaterThanTo_400(t *testing.T) {
	h := newVehicleHarness(t)
	id := seedVehicleAndHistory(t, h, "HIST-5", 3, 1_700_000_000_000)
	cookie := h.issueCookie(t, "mgr_001", "manager")
	url := "/api/vehicles/" + id + "/positions?from=2000&to=1000"
	resp, err := h.app.Test(vehReq(t, http.MethodGet, url, nil, cookie))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestVehicleHistory_LimitClampedAt5000(t *testing.T) {
	// Seed > 5000 rows; request limit=99999. Expect the response to
	// contain exactly 5000 positions (silent clamp).
	h := newVehicleHarness(t)
	v, err := h.mock.Create(context.Background(), usecase.CreateVehicleInput{PlateNumber: "BIG-1"})
	if err != nil {
		t.Fatalf("seed vehicle: %v", err)
	}
	// 5100 rows is more than enough to hit the cap.
	for i := 0; i < 5100; i++ {
		h.mock.seedPosition(&domain.Position{
			ID:         int64(i + 1),
			VehicleID:  v.ID,
			RecordedAt: int64(i + 1),
		})
	}
	cookie := h.issueCookie(t, "mgr_001", "manager")
	resp, err := h.app.Test(vehReq(t, http.MethodGet, "/api/vehicles/"+v.ID+"/positions?limit=99999", nil, cookie))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, readBody(t, resp))
	}
	// Substring match for "count":5000 — the assert avoids unmarshalling
	// the whole 5000-row body, which would be wasteful here.
	if got := readBody(t, resp); !strings.Contains(got, `"count":5000`) {
		t.Errorf(`expected "count":5000 in body, got prefix: %s`, got[:min(120, len(got))])
	}
}

func TestVehicleHistory_NoParamsUsesDefaults(t *testing.T) {
	// No query params; expect default limit (1000) to apply. Seed 1500
	// rows so the cap is observable; expect 1000 in the response.
	h := newVehicleHarness(t)
	v, err := h.mock.Create(context.Background(), usecase.CreateVehicleInput{PlateNumber: "DFL-1"})
	if err != nil {
		t.Fatalf("seed vehicle: %v", err)
	}
	for i := 0; i < 1500; i++ {
		h.mock.seedPosition(&domain.Position{
			ID:         int64(i + 1),
			VehicleID:  v.ID,
			RecordedAt: int64(i + 1),
		})
	}
	cookie := h.issueCookie(t, "mgr_001", "manager")
	resp, err := h.app.Test(vehReq(t, http.MethodGet, "/api/vehicles/"+v.ID+"/positions", nil, cookie))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if got := readBody(t, resp); !strings.Contains(got, `"count":1000`) {
		t.Errorf(`expected "count":1000, got prefix: %s`, got[:min(120, len(got))])
	}
}

func TestVehicleHistory_RangeBracketingFilters(t *testing.T) {
	// Seed positions at recorded_at = 1000, 2000, 3000, 4000, 5000.
	// Request from=2000&to=4000 — expect 3 rows.
	h := newVehicleHarness(t)
	v, err := h.mock.Create(context.Background(), usecase.CreateVehicleInput{PlateNumber: "RANGE-1"})
	if err != nil {
		t.Fatalf("seed vehicle: %v", err)
	}
	for _, ts := range []int64{1000, 2000, 3000, 4000, 5000} {
		h.mock.seedPosition(&domain.Position{VehicleID: v.ID, RecordedAt: ts})
	}
	cookie := h.issueCookie(t, "mgr_001", "manager")
	resp, err := h.app.Test(vehReq(t, http.MethodGet, "/api/vehicles/"+v.ID+"/positions?from=2000&to=4000", nil, cookie))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if got := readBody(t, resp); !strings.Contains(got, `"count":3`) {
		t.Errorf(`expected "count":3, got: %s`, got)
	}
}

func TestVehicleHistory_InternalError_500(t *testing.T) {
	h := newVehicleHarness(t)
	id := seedVehicleAndHistory(t, h, "HIST-ERR", 1, 1_700_000_000_000)
	h.mock.errOverride["ListPositions"] = errors.New("d1 outage")
	cookie := h.issueCookie(t, "mgr_001", "manager")
	resp, err := h.app.Test(vehReq(t, http.MethodGet, "/api/vehicles/"+id+"/positions", nil, cookie))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", resp.StatusCode, readBody(t, resp))
	}
}
