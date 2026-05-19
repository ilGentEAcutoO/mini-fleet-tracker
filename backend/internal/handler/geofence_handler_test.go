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
// In-memory fake usecase. Implements GeofenceUsecase so handler tests
// exercise the real validation -> mapping -> response shape pipeline
// without hauling in the production usecase struct + its deps.
// ---------------------------------------------------------------------------

type memGeofenceUsecase struct {
	mu       sync.Mutex
	byVeh    map[string]*domain.Geofence
	fenceSeq int
	// errOverride lets a specific test inject a custom error from any
	// method without rewriting the fake. nil means "default behaviour".
	errOverride map[string]error
	// vehiclesExist tracks which vehicle IDs the fake considers valid.
	// Set determines its "vehicle exists" behaviour from this map —
	// absent IDs produce ErrNotFound (mirrors the real usecase's
	// existence check).
	vehiclesExist map[string]bool
}

func newMemGeofenceUsecase() *memGeofenceUsecase {
	return &memGeofenceUsecase{
		byVeh:         map[string]*domain.Geofence{},
		errOverride:   map[string]error{},
		vehiclesExist: map[string]bool{},
	}
}

func (m *memGeofenceUsecase) nextID() string {
	m.fenceSeq++
	return fmt.Sprintf("fence_%03d", m.fenceSeq)
}

func (m *memGeofenceUsecase) GetByVehicle(_ context.Context, vehicleID string) (*domain.Geofence, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.errOverride["GetByVehicle"]; err != nil {
		return nil, err
	}
	if strings.TrimSpace(vehicleID) == "" {
		return nil, fmt.Errorf("vehicle_id required: %w", domain.ErrValidation)
	}
	g, ok := m.byVeh[vehicleID]
	if !ok {
		return nil, fmt.Errorf("fence for vehicle %s: %w", vehicleID, domain.ErrNotFound)
	}
	cp := *g
	return &cp, nil
}

func (m *memGeofenceUsecase) Set(_ context.Context, in usecase.SetGeofenceInput) (*domain.Geofence, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.errOverride["Set"]; err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.VehicleID) == "" {
		return nil, fmt.Errorf("vehicle_id required: %w", domain.ErrValidation)
	}
	if in.CenterLat < -90 || in.CenterLat > 90 {
		return nil, fmt.Errorf("lat: %w", domain.ErrValidation)
	}
	if in.CenterLng < -180 || in.CenterLng > 180 {
		return nil, fmt.Errorf("lng: %w", domain.ErrValidation)
	}
	if in.RadiusM < 50 || in.RadiusM > 50_000 {
		return nil, fmt.Errorf("radius: %w", domain.ErrValidation)
	}
	if !m.vehiclesExist[in.VehicleID] {
		return nil, fmt.Errorf("vehicle %s: %w", in.VehicleID, domain.ErrNotFound)
	}
	g := &domain.Geofence{
		ID:        m.nextID(),
		VehicleID: in.VehicleID,
		CenterLat: in.CenterLat,
		CenterLng: in.CenterLng,
		RadiusM:   in.RadiusM,
		CreatedAt: time.Now().UnixMilli(),
	}
	cp := *g
	m.byVeh[in.VehicleID] = &cp
	return g, nil
}

// ---------------------------------------------------------------------------
// Harness — mirrors the vehicle handler harness: real auth middleware,
// real CSRF, JWT signer, only the usecase is faked.
// ---------------------------------------------------------------------------

type geofenceHarness struct {
	app     *fiber.App
	signer  *jwt.Signer
	mock    *memGeofenceUsecase
	handler *GeofenceHandler
}

func newGeofenceHarness(t *testing.T) *geofenceHarness {
	t.Helper()
	signer, err := jwt.NewSigner("geofence-test-secret-please-replace", time.Hour)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	mock := newMemGeofenceUsecase()
	h, err := NewGeofenceHandler(mock)
	if err != nil {
		t.Fatalf("NewGeofenceHandler: %v", err)
	}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(middleware.RequestID())
	app.Use(middleware.NewAuth(signer, neverBlockedGeofence{}))
	app.Use(middleware.NewCSRF())

	app.Get("/api/vehicles/:id/geofence", h.Get)
	app.Put("/api/vehicles/:id/geofence", h.Put)

	return &geofenceHarness{app: app, signer: signer, mock: mock, handler: h}
}

// neverBlockedGeofence is the BlocklistChecker stub for this handler's
// tests. We trust every issued cookie — token revocation is the auth
// middleware's concern.
type neverBlockedGeofence struct{}

func (neverBlockedGeofence) IsRevoked(_ context.Context, _ *jwt.Claims) (bool, error) {
	return false, nil
}

func (h *geofenceHarness) issueCookie(t *testing.T, sub, role string) string {
	t.Helper()
	token, _, err := h.signer.Issue(sub, role)
	if err != nil {
		t.Fatalf("issue cookie: %v", err)
	}
	return token
}

// geoReq builds an authenticated request with the CSRF double-submit
// populated. authCookie="" means "no cookie attached".
//
// Reuses jsonReq from auth_handler_test.go (same package) and the
// canonical csrfTokenForTests constant from position_handler_test.go.
// Named geoReq so it stays grep-distinct from the per-handler helpers.
func geoReq(t *testing.T, method, path string, body any, authCookie string) *http.Request {
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

func TestNewGeofenceHandler_RejectsNilUsecase(t *testing.T) {
	if _, err := NewGeofenceHandler(nil); err == nil {
		t.Fatal("expected error for nil usecase")
	}
}

// ---------------------------------------------------------------------------
// Role gate.
// ---------------------------------------------------------------------------

func TestGeofenceGet_NoAuth_401(t *testing.T) {
	h := newGeofenceHarness(t)
	req := geoReq(t, http.MethodGet, "/api/vehicles/veh_1/geofence", nil, "")
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestGeofenceGet_AsDriver_403(t *testing.T) {
	h := newGeofenceHarness(t)
	driverCookie := h.issueCookie(t, "drv_001", "driver")
	req := geoReq(t, http.MethodGet, "/api/vehicles/veh_1/geofence", nil, driverCookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestGeofencePut_AsDriver_403(t *testing.T) {
	h := newGeofenceHarness(t)
	driverCookie := h.issueCookie(t, "drv_001", "driver")
	body := putGeofenceRequest{CenterLat: 13.7, CenterLng: 100.5, RadiusM: 500}
	req := geoReq(t, http.MethodPut, "/api/vehicles/veh_1/geofence", body, driverCookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

// ---------------------------------------------------------------------------
// Get.
// ---------------------------------------------------------------------------

func TestGeofenceGet_NotFound_404(t *testing.T) {
	h := newGeofenceHarness(t)
	mgrCookie := h.issueCookie(t, "mgr_001", "manager")
	req := geoReq(t, http.MethodGet, "/api/vehicles/veh_unknown/geofence", nil, mgrCookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestGeofenceGet_Found_200(t *testing.T) {
	h := newGeofenceHarness(t)
	mgrCookie := h.issueCookie(t, "mgr_001", "manager")
	h.mock.byVeh["veh_1"] = &domain.Geofence{
		ID: "fence_xyz", VehicleID: "veh_1",
		CenterLat: 13.7, CenterLng: 100.5, RadiusM: 500, CreatedAt: 1,
	}

	req := geoReq(t, http.MethodGet, "/api/vehicles/veh_1/geofence", nil, mgrCookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"id":"fence_xyz"`) {
		t.Errorf("body missing fence id: %s", body)
	}
	if !strings.Contains(body, `"radius_m":500`) {
		t.Errorf("body missing radius_m: %s", body)
	}
}

func TestGeofenceGet_InternalError_500(t *testing.T) {
	h := newGeofenceHarness(t)
	mgrCookie := h.issueCookie(t, "mgr_001", "manager")
	h.mock.errOverride["GetByVehicle"] = errors.New("D1 unreachable")

	req := geoReq(t, http.MethodGet, "/api/vehicles/veh_1/geofence", nil, mgrCookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

// ---------------------------------------------------------------------------
// Put.
// ---------------------------------------------------------------------------

func TestGeofencePut_AsManager_200(t *testing.T) {
	h := newGeofenceHarness(t)
	mgrCookie := h.issueCookie(t, "mgr_001", "manager")
	h.mock.vehiclesExist["veh_1"] = true

	body := putGeofenceRequest{CenterLat: 13.7563, CenterLng: 100.5018, RadiusM: 500}
	req := geoReq(t, http.MethodPut, "/api/vehicles/veh_1/geofence", body, mgrCookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, readBody(t, resp))
	}
	got := readBody(t, resp)
	if !strings.Contains(got, `"vehicle_id":"veh_1"`) {
		t.Errorf("body missing vehicle_id: %s", got)
	}
	if !strings.Contains(got, `"radius_m":500`) {
		t.Errorf("body missing radius_m: %s", got)
	}
}

func TestGeofencePut_MissingVehicle_404(t *testing.T) {
	h := newGeofenceHarness(t)
	mgrCookie := h.issueCookie(t, "mgr_001", "manager")
	// No vehiclesExist entry → ErrNotFound from the fake usecase.

	body := putGeofenceRequest{CenterLat: 13.7, CenterLng: 100.5, RadiusM: 500}
	req := geoReq(t, http.MethodPut, "/api/vehicles/veh_missing/geofence", body, mgrCookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestGeofencePut_InvalidBody_400(t *testing.T) {
	h := newGeofenceHarness(t)
	mgrCookie := h.issueCookie(t, "mgr_001", "manager")
	h.mock.vehiclesExist["veh_1"] = true

	cases := []struct {
		name string
		body putGeofenceRequest
	}{
		{"lat out of range", putGeofenceRequest{CenterLat: 99, CenterLng: 100, RadiusM: 500}},
		{"lng out of range", putGeofenceRequest{CenterLat: 13, CenterLng: 200, RadiusM: 500}},
		{"radius too small", putGeofenceRequest{CenterLat: 13, CenterLng: 100, RadiusM: 10}},
		{"radius too large", putGeofenceRequest{CenterLat: 13, CenterLng: 100, RadiusM: 100_000}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := geoReq(t, http.MethodPut, "/api/vehicles/veh_1/geofence", tc.body, mgrCookie)
			resp, err := h.app.Test(req)
			if err != nil {
				t.Fatalf("Test: %v", err)
			}
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, readBody(t, resp))
			}
		})
	}
}

func TestGeofencePut_MalformedJSON_400(t *testing.T) {
	// A request body that doesn't parse as JSON must be rejected with
	// 400 — the BodyParser error path.
	h := newGeofenceHarness(t)
	mgrCookie := h.issueCookie(t, "mgr_001", "manager")
	h.mock.vehiclesExist["veh_1"] = true

	req := httptest.NewRequest(http.MethodPut, "/api/vehicles/veh_1/geofence",
		strings.NewReader("{not valid json"))
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

func TestGeofencePut_NoCSRF_403(t *testing.T) {
	// A mutating request without the CSRF double-submit must be
	// rejected by the CSRF middleware before the handler runs.
	h := newGeofenceHarness(t)
	mgrCookie := h.issueCookie(t, "mgr_001", "manager")

	body := putGeofenceRequest{CenterLat: 13, CenterLng: 100, RadiusM: 500}
	// Build a request with the auth cookie but no CSRF cookie / header.
	req := jsonReq(t, http.MethodPut, "/api/vehicles/veh_1/geofence", body)
	req.AddCookie(&http.Cookie{Name: middleware.AuthCookieName, Value: mgrCookie})

	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", resp.StatusCode, readBody(t, resp))
	}
}
