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
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/middleware"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/usecase"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/pkg/jwt"
)

// ---------------------------------------------------------------------------
// Programmable usecase mock — lets each test case inject the exact
// usecase response without re-running the real implementation.
// ---------------------------------------------------------------------------

type programmablePositionUsecase struct {
	writeFn func(ctx context.Context, driverID string, in usecase.WritePositionInput) (*domain.Position, error)
}

func (u *programmablePositionUsecase) Write(
	ctx context.Context,
	driverID string,
	in usecase.WritePositionInput,
) (*domain.Position, error) {
	return u.writeFn(ctx, driverID, in)
}

// ---------------------------------------------------------------------------
// Test harness — boots a Fiber app with real auth middleware so the
// cookie + role-gate interactions are exercised end-to-end. Mirrors the
// pattern in auth_handler_test.go (programmableHarness) — same
// middleware order, same JWT signer, just one different handler.
// ---------------------------------------------------------------------------

type positionHarness struct {
	app     *fiber.App
	signer  *jwt.Signer
	handler *PositionHandler
}

func newPositionHarness(
	t *testing.T,
	uc PositionUsecase,
) *positionHarness {
	t.Helper()
	signer, err := jwt.NewSigner("position-test-secret-please-replace", time.Hour)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	h, err := NewPositionHandler(uc)
	if err != nil {
		t.Fatalf("NewPositionHandler: %v", err)
	}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(middleware.RequestID())
	// Auth middleware decodes the cookie, populates locals; CSRF guards
	// mutating methods. Mirror the production wiring.
	app.Use(middleware.NewAuth(signer, neverBlockedPosition{}))
	app.Use(middleware.NewCSRF())
	app.Post("/api/positions", h.Write)

	return &positionHarness{app: app, signer: signer, handler: h}
}

// neverBlockedPosition is the BlocklistChecker stub for handler tests.
// We don't care about revocation in this layer — that's covered by the
// auth middleware tests.
type neverBlockedPosition struct{}

func (neverBlockedPosition) IsRevoked(_ context.Context, _ *jwt.Claims) (bool, error) {
	return false, nil
}

// issueCookie mints a real JWT for (sub, role) and returns the cookie
// value the test can attach to subsequent requests.
func (h *positionHarness) issueCookie(t *testing.T, sub, role string) string {
	t.Helper()
	token, _, err := h.signer.Issue(sub, role)
	if err != nil {
		t.Fatalf("issue cookie: %v", err)
	}
	return token
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

const csrfTokenForTests = "match-csrf-token-1234567890abcd1234567890abcd1234"

func jsonReqWithAuth(t *testing.T, method, path string, body any, authCookie string) *http.Request {
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
	if authCookie != "" {
		req.AddCookie(&http.Cookie{Name: middleware.AuthCookieName, Value: authCookie})
		// CSRF double-submit: same value in cookie and header.
		req.AddCookie(&http.Cookie{Name: middleware.CSRFCookieName, Value: csrfTokenForTests})
		req.Header.Set(middleware.CSRFHeaderName, csrfTokenForTests)
	}
	return req
}

func readBodyP(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// nowMs is the unix-ms timestamp used by every test that needs a fresh
// recorded_at. Tests that need to exercise freshness use their own
// computed values.
func nowMs() int64 { return time.Now().UnixMilli() }

// ---------------------------------------------------------------------------
// Construction.
// ---------------------------------------------------------------------------

func TestNewPositionHandler_RejectsNilUsecase(t *testing.T) {
	if _, err := NewPositionHandler(nil); err == nil {
		t.Fatal("expected error for nil usecase")
	}
}

// ---------------------------------------------------------------------------
// Authentication / authorisation.
// ---------------------------------------------------------------------------

func TestPosition_NoAuthCookie_401(t *testing.T) {
	uc := &programmablePositionUsecase{
		writeFn: func(context.Context, string, usecase.WritePositionInput) (*domain.Position, error) {
			t.Fatal("usecase should not be called when caller is unauthenticated")
			return nil, nil
		},
	}
	h := newPositionHarness(t, uc)

	body := writePositionRequest{
		VehicleID: "veh_1", Lat: 13.7563, Lng: 100.5018, RecordedAt: nowMs(),
	}
	req := jsonReqWithAuth(t, http.MethodPost, "/api/positions", body, "")
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", resp.StatusCode, readBodyP(t, resp))
	}
}

func TestPosition_AsManager_403(t *testing.T) {
	uc := &programmablePositionUsecase{
		writeFn: func(context.Context, string, usecase.WritePositionInput) (*domain.Position, error) {
			t.Fatal("usecase must not be called for a non-driver caller")
			return nil, nil
		},
	}
	h := newPositionHarness(t, uc)
	managerCookie := h.issueCookie(t, "drv_manager", "manager")

	body := writePositionRequest{
		VehicleID: "veh_1", Lat: 13.7563, Lng: 100.5018, RecordedAt: nowMs(),
	}
	req := jsonReqWithAuth(t, http.MethodPost, "/api/positions", body, managerCookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", resp.StatusCode, readBodyP(t, resp))
	}
	if got := readBodyP(t, resp); !strings.Contains(got, `"error":"forbidden"`) {
		t.Errorf("body should report forbidden: %s", got)
	}
}

func TestPosition_DriverNotOwner_403(t *testing.T) {
	uc := &programmablePositionUsecase{
		writeFn: func(_ context.Context, _ string, _ usecase.WritePositionInput) (*domain.Position, error) {
			return nil, fmt.Errorf("driver does not own vehicle: %w", domain.ErrForbidden)
		},
	}
	h := newPositionHarness(t, uc)
	driverCookie := h.issueCookie(t, "drv_intruder", "driver")

	body := writePositionRequest{
		VehicleID: "veh_belongs_to_someone_else", Lat: 13.7563, Lng: 100.5018, RecordedAt: nowMs(),
	}
	req := jsonReqWithAuth(t, http.MethodPost, "/api/positions", body, driverCookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", resp.StatusCode, readBodyP(t, resp))
	}
}

// ---------------------------------------------------------------------------
// Validation.
// ---------------------------------------------------------------------------

func TestPosition_BadLat_400(t *testing.T) {
	uc := &programmablePositionUsecase{
		writeFn: func(context.Context, string, usecase.WritePositionInput) (*domain.Position, error) {
			t.Fatal("usecase should not be called when validator fails at the handler")
			return nil, nil
		},
	}
	h := newPositionHarness(t, uc)
	driverCookie := h.issueCookie(t, "drv_1", "driver")

	body := writePositionRequest{
		VehicleID: "veh_1",
		Lat:       91.0, // outside [-90, 90]
		Lng:       100.5018,
		RecordedAt: nowMs(),
	}
	req := jsonReqWithAuth(t, http.MethodPost, "/api/positions", body, driverCookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, readBodyP(t, resp))
	}
	if got := readBodyP(t, resp); !strings.Contains(got, `"error":"validation_failed"`) {
		t.Errorf("body should report validation_failed: %s", got)
	}
}

func TestPosition_BadJSON_400(t *testing.T) {
	uc := &programmablePositionUsecase{
		writeFn: func(context.Context, string, usecase.WritePositionInput) (*domain.Position, error) {
			t.Fatal("usecase should not be called on bad JSON")
			return nil, nil
		},
	}
	h := newPositionHarness(t, uc)
	driverCookie := h.issueCookie(t, "drv_1", "driver")

	req := httptest.NewRequest(http.MethodPost, "/api/positions", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: middleware.AuthCookieName, Value: driverCookie})
	req.AddCookie(&http.Cookie{Name: middleware.CSRFCookieName, Value: csrfTokenForTests})
	req.Header.Set(middleware.CSRFHeaderName, csrfTokenForTests)

	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPosition_UsecaseValidationError_400(t *testing.T) {
	// Caller passes validator/v10 (range tag passes for boundary values
	// — say 90 exactly — but our usecase has the same gate as defense
	// in depth). When the usecase rejects, the handler must map to 400.
	uc := &programmablePositionUsecase{
		writeFn: func(context.Context, string, usecase.WritePositionInput) (*domain.Position, error) {
			return nil, fmt.Errorf("simulated bad input: %w", domain.ErrValidation)
		},
	}
	h := newPositionHarness(t, uc)
	driverCookie := h.issueCookie(t, "drv_1", "driver")

	body := writePositionRequest{
		VehicleID: "veh_1", Lat: 13.7563, Lng: 100.5018, RecordedAt: nowMs(),
	}
	req := jsonReqWithAuth(t, http.MethodPost, "/api/positions", body, driverCookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, readBodyP(t, resp))
	}
}

func TestPosition_VehicleNotFound_404(t *testing.T) {
	uc := &programmablePositionUsecase{
		writeFn: func(context.Context, string, usecase.WritePositionInput) (*domain.Position, error) {
			return nil, fmt.Errorf("vehicle missing: %w", domain.ErrNotFound)
		},
	}
	h := newPositionHarness(t, uc)
	driverCookie := h.issueCookie(t, "drv_1", "driver")

	body := writePositionRequest{
		VehicleID: "veh_phantom", Lat: 13.7563, Lng: 100.5018, RecordedAt: nowMs(),
	}
	req := jsonReqWithAuth(t, http.MethodPost, "/api/positions", body, driverCookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", resp.StatusCode, readBodyP(t, resp))
	}
	if got := readBodyP(t, resp); !strings.Contains(got, `"error":"not_found"`) {
		t.Errorf("body should report not_found: %s", got)
	}
}

func TestPosition_InternalError_500(t *testing.T) {
	uc := &programmablePositionUsecase{
		writeFn: func(context.Context, string, usecase.WritePositionInput) (*domain.Position, error) {
			return nil, errors.New("D1 unreachable")
		},
	}
	h := newPositionHarness(t, uc)
	driverCookie := h.issueCookie(t, "drv_1", "driver")

	body := writePositionRequest{
		VehicleID: "veh_1", Lat: 13.7563, Lng: 100.5018, RecordedAt: nowMs(),
	}
	req := jsonReqWithAuth(t, http.MethodPost, "/api/positions", body, driverCookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", resp.StatusCode, readBodyP(t, resp))
	}
	if got := readBodyP(t, resp); !strings.Contains(got, `"error":"internal"`) {
		t.Errorf("body should report internal: %s", got)
	}
}

// ---------------------------------------------------------------------------
// Success.
// ---------------------------------------------------------------------------

func TestPosition_Success_201(t *testing.T) {
	uc := &programmablePositionUsecase{
		writeFn: func(_ context.Context, driverID string, in usecase.WritePositionInput) (*domain.Position, error) {
			// Echo input back with DB-assigned fields.
			return &domain.Position{
				ID:         42,
				VehicleID:  in.VehicleID,
				Lat:        in.Lat,
				Lng:        in.Lng,
				SpeedKmh:   in.SpeedKmh,
				RecordedAt: in.RecordedAt,
				CreatedAt:  1_700_000_001_234,
			}, nil
		},
	}
	h := newPositionHarness(t, uc)
	driverCookie := h.issueCookie(t, "drv_1", "driver")

	recorded := nowMs()
	body := writePositionRequest{
		VehicleID:  "veh_1",
		Lat:        13.7563,
		Lng:        100.5018,
		SpeedKmh:   42.5,
		RecordedAt: recorded,
	}
	req := jsonReqWithAuth(t, http.MethodPost, "/api/positions", body, driverCookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", resp.StatusCode, readBodyP(t, resp))
	}

	// Inspect the body shape — every documented field must be present.
	var out struct {
		Position positionDTO `json:"position"`
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if out.Position.ID != 42 {
		t.Errorf("ID: got %d, want 42", out.Position.ID)
	}
	if out.Position.VehicleID != "veh_1" {
		t.Errorf("VehicleID: got %q, want veh_1", out.Position.VehicleID)
	}
	if out.Position.Lat != 13.7563 {
		t.Errorf("Lat: got %f, want 13.7563", out.Position.Lat)
	}
	if out.Position.Lng != 100.5018 {
		t.Errorf("Lng: got %f, want 100.5018", out.Position.Lng)
	}
	if out.Position.SpeedKmh != 42.5 {
		t.Errorf("SpeedKmh: got %f, want 42.5", out.Position.SpeedKmh)
	}
	if out.Position.RecordedAt != recorded {
		t.Errorf("RecordedAt: got %d, want %d", out.Position.RecordedAt, recorded)
	}
	if out.Position.CreatedAt != 1_700_000_001_234 {
		t.Errorf("CreatedAt: got %d, want 1_700_000_001_234", out.Position.CreatedAt)
	}
}

func TestPosition_Success_OmitsZeroSpeed(t *testing.T) {
	// SpeedKmh=0 must be omitted from the response JSON (omitempty tag).
	uc := &programmablePositionUsecase{
		writeFn: func(_ context.Context, _ string, in usecase.WritePositionInput) (*domain.Position, error) {
			return &domain.Position{
				ID:         7,
				VehicleID:  in.VehicleID,
				Lat:        in.Lat,
				Lng:        in.Lng,
				SpeedKmh:   0, // unset
				RecordedAt: in.RecordedAt,
				CreatedAt:  1_700_000_001_000,
			}, nil
		},
	}
	h := newPositionHarness(t, uc)
	driverCookie := h.issueCookie(t, "drv_1", "driver")

	body := writePositionRequest{
		VehicleID: "veh_1", Lat: 13.7563, Lng: 100.5018, RecordedAt: nowMs(),
	}
	req := jsonReqWithAuth(t, http.MethodPost, "/api/positions", body, driverCookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", resp.StatusCode, readBodyP(t, resp))
	}
	bodyStr := readBodyP(t, resp)
	if strings.Contains(bodyStr, "speed_kmh") {
		t.Errorf("response should omit speed_kmh when zero: %s", bodyStr)
	}
}

// ---------------------------------------------------------------------------
// DTO unit checks.
// ---------------------------------------------------------------------------

func TestPositionDTO_NilSafe(t *testing.T) {
	dto := toPositionDTO(nil)
	if dto.ID != 0 || dto.VehicleID != "" || dto.Lat != 0 || dto.Lng != 0 {
		t.Errorf("toPositionDTO(nil) should return zero struct; got %+v", dto)
	}
}

func TestPositionDTO_OmitsZeroSpeedInJSON(t *testing.T) {
	// Direct JSON marshaling check, independent of the handler.
	dto := positionDTO{
		ID: 1, VehicleID: "v", Lat: 1, Lng: 2, SpeedKmh: 0,
		RecordedAt: 100, CreatedAt: 200,
	}
	b, _ := json.Marshal(dto)
	if strings.Contains(string(b), "speed_kmh") {
		t.Errorf("zero SpeedKmh should be omitted: %s", string(b))
	}
}

func TestPositionDTO_IncludesNonZeroSpeed(t *testing.T) {
	dto := positionDTO{
		ID: 1, VehicleID: "v", Lat: 1, Lng: 2, SpeedKmh: 50,
		RecordedAt: 100, CreatedAt: 200,
	}
	b, _ := json.Marshal(dto)
	if !strings.Contains(string(b), `"speed_kmh":50`) {
		t.Errorf("non-zero SpeedKmh should appear: %s", string(b))
	}
}
