package middleware

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
)

// ---------------------------------------------------------------------------
// In-memory storage with a faked clock for deterministic rate-limit tests.
// ---------------------------------------------------------------------------

// memStorage is an in-process RateLimiterStorage. The clock function lets
// tests bind state to a controllable wall time; the TTL is honoured
// against that same clock so window rollovers are deterministic.
type memStorage struct {
	mu    sync.Mutex
	data  map[string]memEntry
	now   func() time.Time
	errAt func(key string) error
}

type memEntry struct {
	value     []byte
	expiresAt time.Time
}

func newMemStorage(now func() time.Time) *memStorage {
	return &memStorage{
		data: map[string]memEntry{},
		now:  now,
	}
}

func (s *memStorage) Read(_ context.Context, key string) ([]byte, time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.errAt != nil {
		if err := s.errAt(key); err != nil {
			return nil, 0, err
		}
	}
	e, ok := s.data[key]
	if !ok {
		return nil, 0, nil
	}
	now := s.now()
	if !e.expiresAt.IsZero() && !now.Before(e.expiresAt) {
		// Expired — pretend the key is absent. This is what KV would do.
		delete(s.data, key)
		return nil, 0, nil
	}
	var remaining time.Duration
	if !e.expiresAt.IsZero() {
		remaining = e.expiresAt.Sub(now)
	}
	cp := append([]byte(nil), e.value...)
	return cp, remaining, nil
}

func (s *memStorage) Write(_ context.Context, key string, state []byte, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.errAt != nil {
		if err := s.errAt(key); err != nil {
			return err
		}
	}
	cp := append([]byte(nil), state...)
	var expires time.Time
	if ttl > 0 {
		expires = s.now().Add(ttl)
	}
	s.data[key] = memEntry{value: cp, expiresAt: expires}
	return nil
}

// clock is a controllable time source used by both the limiter and the
// memStorage so they share the same notion of "now".
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *clock { return &clock{t: t} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// ---------------------------------------------------------------------------
// Test helpers.
// ---------------------------------------------------------------------------

// newTestFiberApp returns a Fiber app whose IP resolution honours the
// X-Forwarded-For header. In production the backend will sit behind the
// Cloudflare gateway Worker which sets CF-Connecting-IP, but the test
// harness uses the standard XFF for portability. Trusting 0.0.0.0/0 is
// safe in the test process because there is no real network egress.
func newTestFiberApp() *fiber.App {
	return fiber.New(fiber.Config{
		DisableStartupMessage:   true,
		EnableTrustedProxyCheck: true,
		TrustedProxies:          []string{"0.0.0.0/0"},
		ProxyHeader:             "X-Forwarded-For",
	})
}

func newGlobalApp(t *testing.T, storage RateLimiterStorage, now func() time.Time) *fiber.App {
	t.Helper()
	app := newTestFiberApp()
	app.Use(RequestID())
	h, err := NewGlobal(GlobalConfig{Storage: storage, Now: now})
	if err != nil {
		t.Fatalf("NewGlobal: %v", err)
	}
	app.Use(h)
	app.Get("/", func(c *fiber.Ctx) error { return c.SendString("ok") })
	return app
}

// requestFromIP runs a single request through the app with the given
// X-Forwarded-For header. The test app is configured with
// EnableTrustedProxyCheck so XFF is respected — which mirrors how the
// production deploy will see CF-Connecting-IP from the gateway Worker.
func requestFromIP(t *testing.T, app *fiber.App, ip string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", ip)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return resp
}

func readRespBody(t *testing.T, resp *http.Response) string {
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

func TestGlobal_NilStorage_Errors(t *testing.T) {
	if _, err := NewGlobal(GlobalConfig{}); err == nil {
		t.Fatal("NewGlobal with nil storage must return an error")
	}
}

func TestGlobal_Under600PerMinute_AllOK(t *testing.T) {
	start := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	clk := newClock(start)
	storage := newMemStorage(clk.now)
	app := newGlobalApp(t, storage, clk.now)

	// Fire exactly 600 requests within the same minute. All should be 200.
	for i := 0; i < GlobalPerMinuteLimit; i++ {
		resp := requestFromIP(t, app, "10.0.0.1")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("req %d: status = %d, want 200", i+1, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestGlobal_601stRequest_429(t *testing.T) {
	start := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	clk := newClock(start)
	storage := newMemStorage(clk.now)
	app := newGlobalApp(t, storage, clk.now)

	for i := 0; i < GlobalPerMinuteLimit; i++ {
		resp := requestFromIP(t, app, "10.0.0.2")
		resp.Body.Close()
	}
	// The next one must be 429.
	resp := requestFromIP(t, app, "10.0.0.2")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("601st request: status = %d, want 429; body=%s", resp.StatusCode, readRespBody(t, resp))
	}
	retryAfter := resp.Header.Get(fiber.HeaderRetryAfter)
	if retryAfter == "" {
		t.Errorf("Retry-After header missing")
	}
	sec, err := strconv.Atoi(retryAfter)
	if err != nil || sec <= 0 {
		t.Errorf("Retry-After = %q, want positive integer", retryAfter)
	}
	body := readRespBody(t, resp)
	if !strings.Contains(body, `"error":"too_many_requests"`) {
		t.Errorf("body should report too_many_requests: %s", body)
	}
}

func TestGlobal_MinuteWindowRolls(t *testing.T) {
	start := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	clk := newClock(start)
	storage := newMemStorage(clk.now)
	app := newGlobalApp(t, storage, clk.now)

	// Burn through the per-minute cap.
	for i := 0; i < GlobalPerMinuteLimit; i++ {
		resp := requestFromIP(t, app, "10.0.0.3")
		resp.Body.Close()
	}
	// Confirm the next request in this minute is denied.
	resp := requestFromIP(t, app, "10.0.0.3")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 within minute; got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Advance 90s — well past the rollover (60s) plus a margin so the
	// in-memory TTL has had a chance to expire too.
	clk.advance(90 * time.Second)

	// New window: a single request must succeed.
	resp2 := requestFromIP(t, app, "10.0.0.3")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("after rollover: status = %d, want 200; body=%s", resp2.StatusCode, readRespBody(t, resp2))
	}
	resp2.Body.Close()
}

func TestGlobal_DailyCapHit_429(t *testing.T) {
	// Use a stub storage that says minute=0 always but reports a day count
	// at the cap. This avoids cycling 10K requests through the app
	// (which would still run in <1s but burns test wall-clock).
	start := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	clk := newClock(start)
	storage := newMemStorage(clk.now)

	ip := "10.0.0.4"
	daySlot := start.Format("2006-01-02")
	dayKey := globalDayKeyPrefix + ":" + ip + ":" + daySlot
	// Seed the storage at the daily cap so the very next request is the
	// 10,001st.
	if err := storage.Write(context.Background(), dayKey, []byte(strconv.Itoa(GlobalPerDayLimit)), globalDayTTL); err != nil {
		t.Fatalf("seed: %v", err)
	}

	app := newGlobalApp(t, storage, clk.now)
	resp := requestFromIP(t, app, ip)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("10,001st req: status = %d, want 429; body=%s", resp.StatusCode, readRespBody(t, resp))
	}
	body := readRespBody(t, resp)
	if !strings.Contains(body, "per-day") {
		t.Errorf("response should mention per-day limit: %s", body)
	}
}

func TestGlobal_DailyWindowRollsAtUTCMidnight(t *testing.T) {
	// 23:59:30 — fill the daily counter; advance past UTC midnight; the
	// new day key has a zero counter so the request succeeds.
	start := time.Date(2026, 5, 19, 23, 59, 30, 0, time.UTC)
	clk := newClock(start)
	storage := newMemStorage(clk.now)
	app := newGlobalApp(t, storage, clk.now)

	ip := "10.0.0.5"

	// Pre-seed the day counter at the cap for today's date.
	daySlot := start.Format("2006-01-02")
	dayKey := globalDayKeyPrefix + ":" + ip + ":" + daySlot
	if err := storage.Write(context.Background(), dayKey, []byte(strconv.Itoa(GlobalPerDayLimit)), globalDayTTL); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Request inside today: 429.
	resp := requestFromIP(t, app, ip)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("before midnight: status = %d, want 429", resp.StatusCode)
	}
	resp.Body.Close()

	// Cross UTC midnight.
	clk.advance(2 * time.Minute) // now 2026-05-20 00:01:30 UTC

	// Day key is a different slot string → counter starts at zero.
	resp2 := requestFromIP(t, app, ip)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("after midnight: status = %d, want 200; body=%s", resp2.StatusCode, readRespBody(t, resp2))
	}
	resp2.Body.Close()
}

func TestGlobal_StorageFailureFailsOpen(t *testing.T) {
	// A KV outage must not turn every request into a 5xx — fail open.
	start := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	clk := newClock(start)
	storage := newMemStorage(clk.now)
	storage.errAt = func(key string) error { return errors.New("kv outage") }
	app := newGlobalApp(t, storage, clk.now)

	resp := requestFromIP(t, app, "10.0.0.6")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("on storage error: status = %d, want 200 (fail-open)", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestGlobal_DifferentIPsDoNotShareBuckets(t *testing.T) {
	start := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	clk := newClock(start)
	storage := newMemStorage(clk.now)
	app := newGlobalApp(t, storage, clk.now)

	// IP A burns its minute cap.
	for i := 0; i < GlobalPerMinuteLimit; i++ {
		resp := requestFromIP(t, app, "10.0.0.10")
		resp.Body.Close()
	}
	// IP A is now blocked.
	respA := requestFromIP(t, app, "10.0.0.10")
	if respA.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("IP A: status = %d, want 429", respA.StatusCode)
	}
	respA.Body.Close()

	// IP B must still be ok.
	respB := requestFromIP(t, app, "10.0.0.11")
	if respB.StatusCode != http.StatusOK {
		t.Fatalf("IP B: status = %d, want 200 (separate bucket)", respB.StatusCode)
	}
	respB.Body.Close()
}

func TestGlobal_BodyIncludesRequestID(t *testing.T) {
	start := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	clk := newClock(start)
	storage := newMemStorage(clk.now)
	app := newGlobalApp(t, storage, clk.now)

	// Seed the per-minute counter at the cap so the next request is 429.
	ip := "10.0.0.20"
	minSlot := start.Format("2006-01-02T15:04")
	key := globalMinuteKeyPrefix + ":" + ip + ":" + minSlot
	_ = storage.Write(context.Background(), key, []byte(strconv.Itoa(GlobalPerMinuteLimit)), globalMinuteTTL)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", ip)
	req.Header.Set(RequestIDHeader, "rl-test-9001")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	body := readRespBody(t, resp)
	if !strings.Contains(body, `"request_id":"rl-test-9001"`) {
		t.Errorf("expected request_id in body, got: %s", body)
	}
}

// ---------------------------------------------------------------------------
// Per-IP / per-user bucket tests.
// ---------------------------------------------------------------------------

func TestPerIP_RejectsInvalidConfig(t *testing.T) {
	storage := newMemStorage(time.Now)
	cases := []struct {
		name string
		cfg  PerIPConfig
	}{
		{"nil storage", PerIPConfig{Bucket: Bucket{Capacity: 1, RefillRate: 1}, KeyPrefix: "k"}},
		{"empty prefix", PerIPConfig{Storage: storage, Bucket: Bucket{Capacity: 1, RefillRate: 1}}},
		{"zero capacity", PerIPConfig{Storage: storage, KeyPrefix: "k", Bucket: Bucket{Capacity: 0, RefillRate: 1}}},
		{"zero refill", PerIPConfig{Storage: storage, KeyPrefix: "k", Bucket: Bucket{Capacity: 1, RefillRate: 0}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewPerIP(tc.cfg); err == nil {
				t.Fatalf("NewPerIP(%s) should fail", tc.name)
			}
		})
	}
}

func TestPerIP_AllowsThenDenies(t *testing.T) {
	start := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	clk := newClock(start)
	storage := newMemStorage(clk.now)

	app := newTestFiberApp()
	app.Use(RequestID())
	h, err := NewPerIP(PerIPConfig{
		Storage:   storage,
		KeyPrefix: "rl-test",
		Bucket:    Bucket{Capacity: 3, RefillRate: 0.1}, // refills slowly
		TTL:       5 * time.Minute,
		Now:       clk.now,
	})
	if err != nil {
		t.Fatalf("NewPerIP: %v", err)
	}
	app.Use(h)
	app.Get("/", func(c *fiber.Ctx) error { return c.SendString("ok") })

	// Three requests should pass (capacity=3).
	for i := 0; i < 3; i++ {
		resp := requestFromIP(t, app, "10.0.0.30")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("req %d: status = %d, want 200", i+1, resp.StatusCode)
		}
		resp.Body.Close()
	}
	// The next is denied (no tokens left, very slow refill).
	resp := requestFromIP(t, app, "10.0.0.30")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("4th: status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get(fiber.HeaderRetryAfter) == "" {
		t.Error("Retry-After header missing")
	}
	resp.Body.Close()

	// Advance enough time for one token to refill.
	clk.advance(20 * time.Second) // 0.1 * 20 = 2 tokens added
	resp2 := requestFromIP(t, app, "10.0.0.30")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("after refill: status = %d, want 200", resp2.StatusCode)
	}
	resp2.Body.Close()
}

func TestPerUser_KeysByUserIDWhenAuthenticated(t *testing.T) {
	start := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	clk := newClock(start)
	storage := newMemStorage(clk.now)

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	// Mimic auth middleware effect by setting the locals key.
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(AuthUserIDLocalsKey, c.Get("X-Test-User"))
		return c.Next()
	})
	h, err := NewPerUser(PerUserConfig{
		Storage:   storage,
		KeyPrefix: "rl-user",
		Bucket:    Bucket{Capacity: 2, RefillRate: 0.01},
		TTL:       time.Minute,
		Now:       clk.now,
	})
	if err != nil {
		t.Fatalf("NewPerUser: %v", err)
	}
	app.Use(h)
	app.Get("/", func(c *fiber.Ctx) error { return c.SendString("ok") })

	// User A burns through their bucket.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Test-User", "user_A")
		resp, terr := app.Test(req)
		if terr != nil {
			t.Fatalf("app.Test: %v", terr)
		}
		resp.Body.Close()
	}
	// User A's 3rd request denied.
	reqA := httptest.NewRequest(http.MethodGet, "/", nil)
	reqA.Header.Set("X-Test-User", "user_A")
	respA, err := app.Test(reqA)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if respA.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("user A 3rd: status = %d, want 429", respA.StatusCode)
	}
	respA.Body.Close()

	// User B is independent and should succeed.
	reqB := httptest.NewRequest(http.MethodGet, "/", nil)
	reqB.Header.Set("X-Test-User", "user_B")
	respB, err := app.Test(reqB)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if respB.StatusCode != http.StatusOK {
		t.Fatalf("user B 1st: status = %d, want 200", respB.StatusCode)
	}
	respB.Body.Close()
}

// Ensure key choice helps observability: scan storage for keys after a
// burst and verify per-IP buckets are isolated by prefix.
func TestPerIP_KeyScopingByPrefix(t *testing.T) {
	start := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	clk := newClock(start)
	storage := newMemStorage(clk.now)

	app := newTestFiberApp()
	h, err := NewPerIP(PerIPConfig{
		Storage:   storage,
		KeyPrefix: "rl-route-a",
		Bucket:    Bucket{Capacity: 5, RefillRate: 1},
		TTL:       time.Minute,
		Now:       clk.now,
	})
	if err != nil {
		t.Fatalf("NewPerIP: %v", err)
	}
	app.Use(h)
	app.Get("/", func(c *fiber.Ctx) error { return c.SendString("ok") })

	resp := requestFromIP(t, app, "10.0.0.40")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	storage.mu.Lock()
	defer storage.mu.Unlock()
	var got []string
	for k := range storage.data {
		got = append(got, k)
	}
	wantPrefix := "rl-route-a:"
	found := false
	for _, k := range got {
		if strings.HasPrefix(k, wantPrefix) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("storage keys missing %s prefix; got=%v", wantPrefix, got)
	}
}

// TestGlobal_CounterIncrementsCorrectly verifies the per-minute counter
// stored in KV moves from 0 → 1 → 2 across two requests in the same
// minute, so the rollover logic has a stable foundation.
func TestGlobal_CounterIncrementsCorrectly(t *testing.T) {
	start := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	clk := newClock(start)
	storage := newMemStorage(clk.now)
	app := newGlobalApp(t, storage, clk.now)

	for i := 1; i <= 3; i++ {
		resp := requestFromIP(t, app, "10.0.0.50")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("req %d: status = %d", i, resp.StatusCode)
		}
		resp.Body.Close()

		// After request N, the per-minute counter should be N.
		minSlot := start.Format("2006-01-02T15:04")
		key := globalMinuteKeyPrefix + ":10.0.0.50:" + minSlot
		raw, _, err := storage.Read(context.Background(), key)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		got, err := strconv.Atoi(string(raw))
		if err != nil {
			t.Fatalf("parse counter %q: %v", string(raw), err)
		}
		if got != i {
			t.Errorf("after req %d: counter = %d, want %d", i, got, i)
		}
	}
}

// quick sanity check that the test infrastructure boots cleanly
func TestGlobal_TestInfra_Smoke(t *testing.T) {
	storage := newMemStorage(time.Now)
	if _, _, err := storage.Read(context.Background(), "missing"); err != nil {
		t.Fatalf("Read on missing: %v", err)
	}
	if err := storage.Write(context.Background(), "k", []byte("v"), time.Minute); err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, _, err := storage.Read(context.Background(), "k")
	if err != nil || string(raw) != "v" {
		t.Fatalf("round-trip: raw=%q err=%v", raw, err)
	}
}

// failingApp builds a Fiber app whose handler returns an explicit error.
// Used to make sure the rate-limit middleware does not swallow handler
// errors when it admits a request.
func failingApp(t *testing.T, storage RateLimiterStorage, now func() time.Time) *fiber.App {
	t.Helper()
	app := newTestFiberApp()
	h, err := NewGlobal(GlobalConfig{Storage: storage, Now: now})
	if err != nil {
		t.Fatalf("NewGlobal: %v", err)
	}
	app.Use(h)
	app.Get("/", func(c *fiber.Ctx) error {
		return fmt.Errorf("handler boom")
	})
	return app
}

func TestGlobal_DoesNotSwallowHandlerErrors(t *testing.T) {
	start := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	clk := newClock(start)
	storage := newMemStorage(clk.now)
	app := failingApp(t, storage, clk.now)

	resp := requestFromIP(t, app, "10.0.0.99")
	defer resp.Body.Close()
	// Fiber translates a handler-returned error into a 500 by default.
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (handler error)", resp.StatusCode)
	}
}
