package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
// Programmable usecase mock — each test case can pin Presign / List
// behaviour without re-running the real implementation.
// ---------------------------------------------------------------------------

type programmablePhotoUsecase struct {
	presignFn func(ctx context.Context, userID, vehicleID, filename string) (*usecase.PresignUploadOutput, error)
	listFn    func(ctx context.Context, vehicleID string) ([]usecase.PhotoListEntry, error)
}

func (u *programmablePhotoUsecase) PresignUpload(
	ctx context.Context, userID, vehicleID, filename string,
) (*usecase.PresignUploadOutput, error) {
	if u.presignFn == nil {
		return nil, errors.New("presignFn not configured")
	}
	return u.presignFn(ctx, userID, vehicleID, filename)
}

func (u *programmablePhotoUsecase) List(ctx context.Context, vehicleID string) ([]usecase.PhotoListEntry, error) {
	if u.listFn == nil {
		return nil, errors.New("listFn not configured")
	}
	return u.listFn(ctx, vehicleID)
}

// ---------------------------------------------------------------------------
// Harness — mirrors the pattern in vehicle_handler_test.go: real auth
// middleware so the cookie + role-gate flow is exercised end-to-end.
// ---------------------------------------------------------------------------

type photoHarness struct {
	app     *fiber.App
	signer  *jwt.Signer
	mock    *programmablePhotoUsecase
	handler *PhotoHandler
}

type neverBlockedPhoto struct{}

func (neverBlockedPhoto) IsRevoked(_ context.Context, _ *jwt.Claims) (bool, error) {
	return false, nil
}

func newPhotoHarness(t *testing.T, uc PhotoUsecase) *photoHarness {
	t.Helper()
	signer, err := jwt.NewSigner("photo-test-secret-please-replace-1234567890", time.Hour)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	h, err := NewPhotoHandler(uc)
	if err != nil {
		t.Fatalf("NewPhotoHandler: %v", err)
	}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(middleware.RequestID())
	app.Use(middleware.NewAuth(signer, neverBlockedPhoto{}))
	app.Use(middleware.NewCSRF())

	// The colon in ":photos:presign" is part of the path literal — Fiber
	// supports it because params start with `:<name>` and any subsequent
	// `:` is treated as a path char. We mirror the production wiring
	// shape so the route key matches what bootstrap.go will register.
	app.Post("/api/vehicles/:id/photos:presign", h.Presign)
	app.Get("/api/vehicles/:id/photos", h.List)

	var mock *programmablePhotoUsecase
	if pu, ok := uc.(*programmablePhotoUsecase); ok {
		mock = pu
	}
	return &photoHarness{app: app, signer: signer, mock: mock, handler: h}
}

func (h *photoHarness) issueCookie(t *testing.T, sub, role string) string {
	t.Helper()
	token, _, err := h.signer.Issue(sub, role)
	if err != nil {
		t.Fatalf("issue cookie: %v", err)
	}
	return token
}

// photoReq builds an authenticated request with CSRF double-submit
// already populated when authCookie is non-empty.
func photoReq(t *testing.T, method, path string, body any, authCookie string) *http.Request {
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

func TestNewPhotoHandler_RejectsNilUsecase(t *testing.T) {
	if _, err := NewPhotoHandler(nil); err == nil {
		t.Fatal("expected error for nil usecase")
	}
}

// ---------------------------------------------------------------------------
// Presign — auth + role guard.
// ---------------------------------------------------------------------------

func TestPhotoPresign_NoAuth_401(t *testing.T) {
	uc := &programmablePhotoUsecase{}
	h := newPhotoHarness(t, uc)
	req := photoReq(t, http.MethodPost, "/api/vehicles/veh-001/photos:presign",
		map[string]string{"filename": "x.jpg"}, "")
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestPhotoPresign_AsDriver_403(t *testing.T) {
	uc := &programmablePhotoUsecase{}
	h := newPhotoHarness(t, uc)
	cookie := h.issueCookie(t, "drv-001", "driver")
	req := photoReq(t, http.MethodPost, "/api/vehicles/veh-001/photos:presign",
		map[string]string{"filename": "x.jpg"}, cookie)
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

// ---------------------------------------------------------------------------
// Presign — success, validation, 404, 429.
// ---------------------------------------------------------------------------

func TestPhotoPresign_AsManager_200(t *testing.T) {
	var gotUserID, gotVehicleID, gotFilename string
	uc := &programmablePhotoUsecase{
		presignFn: func(_ context.Context, userID, vehicleID, filename string) (*usecase.PresignUploadOutput, error) {
			gotUserID = userID
			gotVehicleID = vehicleID
			gotFilename = filename
			return &usecase.PresignUploadOutput{
				URL:              "https://r2.test/mft/vehicles/veh-001/abc-x.jpg?sig=1",
				Method:           http.MethodPut,
				Headers:          map[string]string{"Content-Length": "5242880"},
				Key:              "vehicles/veh-001/abc-x.jpg",
				ContentLengthMax: usecase.MaxPhotoBytes,
				ExpiresAt:        time.Now().Add(5 * time.Minute).UnixMilli(),
				QuotaRemaining:   2,
			}, nil
		},
	}
	h := newPhotoHarness(t, uc)
	cookie := h.issueCookie(t, "mgr-001", "manager")
	req := photoReq(t, http.MethodPost, "/api/vehicles/veh-001/photos:presign",
		map[string]string{"filename": "front_view.jpg"}, cookie)

	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, readBody(t, resp))
	}

	if gotUserID != "mgr-001" {
		t.Errorf("usecase received userID = %q, want mgr-001", gotUserID)
	}
	if gotVehicleID != "veh-001" {
		t.Errorf("usecase received vehicleID = %q, want veh-001", gotVehicleID)
	}
	if gotFilename != "front_view.jpg" {
		t.Errorf("usecase received filename = %q, want front_view.jpg", gotFilename)
	}

	body := readBody(t, resp)
	for _, want := range []string{`"url":"https://r2.test/mft/`, `"method":"PUT"`, `"key":"vehicles/veh-001/abc-x.jpg"`, `"content_length_max":5242880`, `"quota_remaining":2`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
}

func TestPhotoPresign_QuotaExceeded_429(t *testing.T) {
	uc := &programmablePhotoUsecase{
		presignFn: func(_ context.Context, _, vid, _ string) (*usecase.PresignUploadOutput, error) {
			return nil, fmt.Errorf("daily upload limit reached for vehicle %s: %w", vid, domain.ErrTooMany)
		},
	}
	h := newPhotoHarness(t, uc)
	cookie := h.issueCookie(t, "mgr-001", "manager")
	req := photoReq(t, http.MethodPost, "/api/vehicles/veh-001/photos:presign",
		map[string]string{"filename": "x.jpg"}, cookie)

	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%s", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"error":"quota_exceeded"`) {
		t.Errorf("body missing quota_exceeded code: %s", body)
	}
	if !strings.Contains(body, "try again tomorrow") && !strings.Contains(body, "Try again tomorrow") {
		t.Errorf("body should suggest retry tomorrow: %s", body)
	}
}

// TestPhotoPresign_QuotaStorageDown_503 pins TASK-052 / security review M2.
// When the usecase returns domain.ErrUnavailable (the fail-closed branch
// in PhotoUsecase.PresignUpload after a writeQuota error) the handler
// must emit 503 + Retry-After with a service_unavailable code so the
// SPA's retryable-error pill engages.
func TestPhotoPresign_QuotaStorageDown_503(t *testing.T) {
	uc := &programmablePhotoUsecase{
		presignFn: func(_ context.Context, _, _, _ string) (*usecase.PresignUploadOutput, error) {
			return nil, fmt.Errorf("kv write blew up: %w", domain.ErrUnavailable)
		},
	}
	h := newPhotoHarness(t, uc)
	cookie := h.issueCookie(t, "mgr-001", "manager")
	req := photoReq(t, http.MethodPost, "/api/vehicles/veh-001/photos:presign",
		map[string]string{"filename": "x.jpg"}, cookie)

	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", resp.StatusCode, readBody(t, resp))
	}
	if ra := resp.Header.Get(fiber.HeaderRetryAfter); ra == "" {
		t.Errorf("Retry-After header missing on 503")
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"error":"service_unavailable"`) {
		t.Errorf("body missing service_unavailable code: %s", body)
	}
}

func TestPhotoPresign_VehicleNotFound_404(t *testing.T) {
	uc := &programmablePhotoUsecase{
		presignFn: func(_ context.Context, _, _, _ string) (*usecase.PresignUploadOutput, error) {
			return nil, fmt.Errorf("missing: %w", domain.ErrNotFound)
		},
	}
	h := newPhotoHarness(t, uc)
	cookie := h.issueCookie(t, "mgr-001", "manager")
	req := photoReq(t, http.MethodPost, "/api/vehicles/nope/photos:presign",
		map[string]string{"filename": "x.jpg"}, cookie)
	resp, err := h.app.Test(req)
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

func TestPhotoPresign_MissingFilename_400(t *testing.T) {
	uc := &programmablePhotoUsecase{
		presignFn: func(_ context.Context, _, _, _ string) (*usecase.PresignUploadOutput, error) {
			t.Fatal("usecase should not be called on validator failure")
			return nil, nil
		},
	}
	h := newPhotoHarness(t, uc)
	cookie := h.issueCookie(t, "mgr-001", "manager")
	// Empty body — validator should reject before usecase runs.
	req := photoReq(t, http.MethodPost, "/api/vehicles/veh-001/photos:presign",
		map[string]string{}, cookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, readBody(t, resp))
	}
	if got := readBody(t, resp); !strings.Contains(got, `"validation_failed"`) {
		t.Errorf("body should report validation_failed: %s", got)
	}
}

func TestPhotoPresign_UsecaseValidation_400(t *testing.T) {
	uc := &programmablePhotoUsecase{
		presignFn: func(_ context.Context, _, _, _ string) (*usecase.PresignUploadOutput, error) {
			return nil, fmt.Errorf("filename %q invalid: %w", "...", domain.ErrValidation)
		},
	}
	h := newPhotoHarness(t, uc)
	cookie := h.issueCookie(t, "mgr-001", "manager")
	req := photoReq(t, http.MethodPost, "/api/vehicles/veh-001/photos:presign",
		map[string]string{"filename": "..."}, cookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestPhotoPresign_UsecaseInternal_500(t *testing.T) {
	uc := &programmablePhotoUsecase{
		presignFn: func(_ context.Context, _, _, _ string) (*usecase.PresignUploadOutput, error) {
			return nil, errors.New("r2 melted")
		},
	}
	h := newPhotoHarness(t, uc)
	cookie := h.issueCookie(t, "mgr-001", "manager")
	req := photoReq(t, http.MethodPost, "/api/vehicles/veh-001/photos:presign",
		map[string]string{"filename": "x.jpg"}, cookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestPhotoPresign_BadJSONBody_400(t *testing.T) {
	uc := &programmablePhotoUsecase{}
	h := newPhotoHarness(t, uc)
	cookie := h.issueCookie(t, "mgr-001", "manager")
	// BodyParser can't decode a string into a struct → 400.
	req := jsonReq(t, http.MethodPost, "/api/vehicles/veh-001/photos:presign", "not-json")
	req.AddCookie(&http.Cookie{Name: middleware.AuthCookieName, Value: cookie})
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

// ---------------------------------------------------------------------------
// List — auth, role, success, 404, 500.
// ---------------------------------------------------------------------------

func TestPhotoList_NoAuth_401(t *testing.T) {
	uc := &programmablePhotoUsecase{}
	h := newPhotoHarness(t, uc)
	req := photoReq(t, http.MethodGet, "/api/vehicles/veh-001/photos", nil, "")
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestPhotoList_AsDriver_403(t *testing.T) {
	uc := &programmablePhotoUsecase{}
	h := newPhotoHarness(t, uc)
	cookie := h.issueCookie(t, "drv-001", "driver")
	req := photoReq(t, http.MethodGet, "/api/vehicles/veh-001/photos", nil, cookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestPhotoList_AsManager_200(t *testing.T) {
	uc := &programmablePhotoUsecase{
		listFn: func(_ context.Context, vehicleID string) ([]usecase.PhotoListEntry, error) {
			if vehicleID != "veh-001" {
				t.Errorf("unexpected vehicleID %q", vehicleID)
			}
			return []usecase.PhotoListEntry{
				{Key: "vehicles/veh-001/a.jpg", URL: "https://r2.test/a", ExpiresAt: 1_000},
				{Key: "vehicles/veh-001/b.jpg", URL: "https://r2.test/b", ExpiresAt: 2_000},
			}, nil
		},
	}
	h := newPhotoHarness(t, uc)
	cookie := h.issueCookie(t, "mgr-001", "manager")
	req := photoReq(t, http.MethodGet, "/api/vehicles/veh-001/photos", nil, cookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)
	for _, want := range []string{`"vehicle_id":"veh-001"`, `"count":2`, `"key":"vehicles/veh-001/a.jpg"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
}

func TestPhotoList_VehicleNotFound_404(t *testing.T) {
	uc := &programmablePhotoUsecase{
		listFn: func(_ context.Context, _ string) ([]usecase.PhotoListEntry, error) {
			return nil, fmt.Errorf("not here: %w", domain.ErrNotFound)
		},
	}
	h := newPhotoHarness(t, uc)
	cookie := h.issueCookie(t, "mgr-001", "manager")
	req := photoReq(t, http.MethodGet, "/api/vehicles/nope/photos", nil, cookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestPhotoList_UsecaseError_500(t *testing.T) {
	uc := &programmablePhotoUsecase{
		listFn: func(_ context.Context, _ string) ([]usecase.PhotoListEntry, error) {
			return nil, errors.New("r2 outage")
		},
	}
	h := newPhotoHarness(t, uc)
	cookie := h.issueCookie(t, "mgr-001", "manager")
	req := photoReq(t, http.MethodGet, "/api/vehicles/veh-001/photos", nil, cookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestPhotoList_EmptyList_200(t *testing.T) {
	uc := &programmablePhotoUsecase{
		listFn: func(_ context.Context, _ string) ([]usecase.PhotoListEntry, error) {
			return []usecase.PhotoListEntry{}, nil
		},
	}
	h := newPhotoHarness(t, uc)
	cookie := h.issueCookie(t, "mgr-001", "manager")
	req := photoReq(t, http.MethodGet, "/api/vehicles/veh-001/photos", nil, cookie)
	resp, err := h.app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"count":0`) {
		t.Errorf("body should report count=0: %s", body)
	}
}
