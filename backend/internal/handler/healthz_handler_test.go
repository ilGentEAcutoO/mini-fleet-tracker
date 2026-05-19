package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// stubD1 is a minimal d1Pinger implementation that returns a fixed
// error (or nil) on Exec. The SQL text and args are ignored — the
// handler only ever sends `SELECT 1`, so there is nothing to assert
// against, and a more elaborate stub would just be ceremony.
type stubD1 struct {
	err error
}

func (s stubD1) Exec(_ context.Context, _ string, _ ...any) error {
	return s.err
}

// stubKV is a minimal kvPinger implementation. The (value, found) pair
// is fixed at (nil, false) on the happy path — the handler discards
// both anyway, so the only knob worth exposing is the error.
type stubKV struct {
	err error
}

func (s stubKV) Get(_ context.Context, _ string) ([]byte, bool, error) {
	return nil, false, s.err
}

// healthzBody is the response shape we decode into. Kept private to
// the test file so it does not become an accidental public DTO.
type healthzBody struct {
	Status        string `json:"status"`
	DB            string `json:"db"`
	KV            string `json:"kv"`
	Commit        string `json:"commit"`
	DemoExpiresAt string `json:"demo_expires_at"`
}

// callHealthz boots a tiny Fiber app with the handler wired in, fires
// a single GET /healthz, and returns the decoded body + status code.
// Centralised so each table row is one line of construction instead of
// twenty.
func callHealthz(t *testing.T, d1Err, kvErr error) (healthzBody, int) {
	t.Helper()

	h, err := NewHealthzHandler(
		stubD1{err: d1Err},
		stubKV{err: kvErr},
		"abc1234",
		"2026-05-31T23:59:59+07:00",
	)
	if err != nil {
		t.Fatalf("NewHealthzHandler: %v", err)
	}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/healthz", h.Check)

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var body healthzBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body %q: %v", raw, err)
	}
	return body, resp.StatusCode
}

func TestHealthz_AllOK(t *testing.T) {
	body, status := callHealthz(t, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want %q", body.Status, "ok")
	}
	if body.DB != "ok" {
		t.Errorf("db = %q, want %q", body.DB, "ok")
	}
	if body.KV != "ok" {
		t.Errorf("kv = %q, want %q", body.KV, "ok")
	}
	if body.Commit != "abc1234" {
		t.Errorf("commit = %q, want %q", body.Commit, "abc1234")
	}
	if body.DemoExpiresAt != "2026-05-31T23:59:59+07:00" {
		t.Errorf("demo_expires_at = %q, want %q", body.DemoExpiresAt, "2026-05-31T23:59:59+07:00")
	}
}

func TestHealthz_DBFails(t *testing.T) {
	body, status := callHealthz(t, errors.New("d1 unreachable"), nil)
	if status != http.StatusOK {
		// Critical: the HTTP status is always 200. Operators read the
		// granular JSON body to triage; a load balancer that flipped
		// instances out of rotation on a single bad ping would
		// amplify partial outages instead of containing them.
		t.Fatalf("status = %d, want 200 (overall HTTP status must stay 200 even when a dep fails)", status)
	}
	if body.Status != "degraded" {
		t.Errorf("status = %q, want %q", body.Status, "degraded")
	}
	if body.DB != "fail" {
		t.Errorf("db = %q, want %q", body.DB, "fail")
	}
	if body.KV != "ok" {
		t.Errorf("kv = %q, want %q", body.KV, "ok")
	}
}

func TestHealthz_KVFails(t *testing.T) {
	body, status := callHealthz(t, nil, errors.New("kv unauthorized"))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if body.Status != "degraded" {
		t.Errorf("status = %q, want %q", body.Status, "degraded")
	}
	if body.DB != "ok" {
		t.Errorf("db = %q, want %q", body.DB, "ok")
	}
	if body.KV != "fail" {
		t.Errorf("kv = %q, want %q", body.KV, "fail")
	}
}

func TestHealthz_BothFail(t *testing.T) {
	body, status := callHealthz(t,
		errors.New("d1 unreachable"),
		errors.New("kv unauthorized"),
	)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if body.Status != "degraded" {
		t.Errorf("status = %q, want %q", body.Status, "degraded")
	}
	if body.DB != "fail" {
		t.Errorf("db = %q, want %q", body.DB, "fail")
	}
	if body.KV != "fail" {
		t.Errorf("kv = %q, want %q", body.KV, "fail")
	}
}

func TestNewHealthzHandler_RejectsNilDeps(t *testing.T) {
	if _, err := NewHealthzHandler(nil, stubKV{}, "c", "e"); err == nil {
		t.Errorf("expected error for nil d1 pinger")
	}
	if _, err := NewHealthzHandler(stubD1{}, nil, "c", "e"); err == nil {
		t.Errorf("expected error for nil kv pinger")
	}
}
