package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseFlags_Valid(t *testing.T) {
	cfg, err := parseFlags([]string{
		"--email", "driver@demo.local",
		"--password", "secret123",
		"--vehicle-id", "veh_1",
		"--base-url", "http://localhost:8080",
		"--interval", "2s",
		"--speed", "42",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Email != "driver@demo.local" || cfg.Password != "secret123" {
		t.Errorf("credentials: got %q/%q", cfg.Email, cfg.Password)
	}
	if cfg.VehicleID != "veh_1" {
		t.Errorf("vehicle_id: got %q", cfg.VehicleID)
	}
	if cfg.Interval != 2*time.Second {
		t.Errorf("interval: got %v", cfg.Interval)
	}
	if cfg.SpeedKmh != 42 {
		t.Errorf("speed: got %v", cfg.SpeedKmh)
	}
}

func TestParseFlags_Defaults(t *testing.T) {
	cfg, err := parseFlags([]string{
		"--email", "driver@demo.local",
		"--password", "secret123",
		"--vehicle-id", "veh_1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BaseURL != "http://localhost:8080" {
		t.Errorf("default base-url: got %q", cfg.BaseURL)
	}
	if cfg.Interval != 2*time.Second {
		t.Errorf("default interval: got %v", cfg.Interval)
	}
	if cfg.SpeedKmh != 35.0 {
		t.Errorf("default speed: got %v", cfg.SpeedKmh)
	}
}

func TestParseFlags_MissingRequired(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no email", []string{"--password", "x", "--vehicle-id", "v"}},
		{"no password", []string{"--email", "e", "--vehicle-id", "v"}},
		{"no vehicle-id", []string{"--email", "e", "--password", "x"}},
		{"none", []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseFlags(tc.args); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

func TestParseFlags_IntervalTooSmall(t *testing.T) {
	_, err := parseFlags([]string{
		"--email", "e", "--password", "p", "--vehicle-id", "v",
		"--interval", "500ms",
	})
	if err == nil {
		t.Fatal("expected error for sub-second interval, got nil")
	}
	if !strings.Contains(err.Error(), "1s") {
		t.Errorf("error should mention 1s minimum, got %v", err)
	}
}

func TestParseFlags_InvalidBaseURL(t *testing.T) {
	_, err := parseFlags([]string{
		"--email", "e", "--password", "p", "--vehicle-id", "v",
		"--base-url", "ftp://nope",
	})
	if err == nil {
		t.Fatal("expected error for non-http scheme, got nil")
	}
}

func TestValidateBaseURL(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"http://localhost:8080", false},
		{"https://api.example.com", false},
		{"https://api.example.com/v1", false},
		{"", true},
		{"   ", true},
		{"not-a-url", true},        // no scheme
		{"http://", true},          // no host
		{"ftp://example.com", true}, // wrong scheme
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			err := validateBaseURL(tc.in)
			if tc.wantErr && err == nil {
				t.Errorf("validateBaseURL(%q) = nil, want error", tc.in)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateBaseURL(%q) = %v, want nil", tc.in, err)
			}
		})
	}
}

// TestRandomWalk_StaysNearOrigin verifies the step never exceeds the
// documented stepDeg bound on either axis across 1000 iterations.
// The walk drifts unbounded over time so we measure per-step delta,
// not cumulative distance.
func TestRandomWalk_StaysNearOrigin(t *testing.T) {
	lat, lng := 13.7563, 100.5018
	for i := 0; i < 1000; i++ {
		newLat, newLng := randomWalk(lat, lng)
		if abs(newLat-lat) > stepDeg {
			t.Errorf("iter %d: lat step %v exceeds stepDeg %v", i, newLat-lat, stepDeg)
		}
		if abs(newLng-lng) > stepDeg {
			t.Errorf("iter %d: lng step %v exceeds stepDeg %v", i, newLng-lng, stepDeg)
		}
		lat, lng = newLat, newLng
	}
}

// TestRandomWalk_Moves verifies that across 100 iterations the position
// actually changes — guards against a regression that returns the
// input unchanged.
func TestRandomWalk_Moves(t *testing.T) {
	lat, lng := 13.7563, 100.5018
	moved := 0
	for i := 0; i < 100; i++ {
		newLat, newLng := randomWalk(lat, lng)
		if newLat != lat || newLng != lng {
			moved++
		}
		lat, lng = newLat, newLng
	}
	// math/rand/v2's auto-seeded RNG returning zero on any given call
	// is astronomically unlikely; expect every iteration to move.
	if moved < 95 {
		t.Errorf("only %d/100 iterations produced movement; RNG looks stuck", moved)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func TestMaskPassword(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"a", "*"},
		{"ab", "**"},
		{"abc", "***"},
		{"abcd", "a**d"},
		{"secret123", "s*******3"},
		{"VeryLongPassword!", "V***************!"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := maskPassword(tc.in)
			if got != tc.want {
				t.Errorf("maskPassword(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestLogin_HappyPath spins up a stub /api/auth/login that mirrors the
// real handler's cookie behaviour (HttpOnly auth_token, plain
// csrf_token) and verifies login() extracts the csrf value.
func TestLogin_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/login" || r.Method != http.MethodPost {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body["email"] == "" || body["password"] == "" {
			http.Error(w, "missing", http.StatusBadRequest)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "auth_token", Value: "jwt.token.here", HttpOnly: true, Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "csrf_token", Value: "csrf-deadbeef", Path: "/"})
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"user":{"id":"d1"}}`)
	}))
	defer srv.Close()

	client, csrf, err := login(context.Background(), srv.URL, "e@x.com", "p")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if csrf != "csrf-deadbeef" {
		t.Errorf("csrf: got %q, want csrf-deadbeef", csrf)
	}
	// Also verify the jar holds the auth cookie for subsequent calls.
	jar := client.Jar
	if jar == nil {
		t.Fatal("client has nil jar")
	}
	u, _ := req(srv.URL).URL.Parse(srv.URL)
	var hasAuth bool
	for _, c := range jar.Cookies(u) {
		if c.Name == "auth_token" {
			hasAuth = true
		}
	}
	if !hasAuth {
		t.Error("jar did not retain auth_token")
	}
}

// req returns a request bound to the given URL — tiny helper to avoid
// importing net/url at the test top-level just for one parse.
func req(u string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, u, nil)
	return r
}

// TestLogin_NonOKStatusReturnsError covers the 401/etc unhappy path.
func TestLogin_NonOKStatusReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
	}))
	defer srv.Close()

	_, _, err := login(context.Background(), srv.URL, "e", "p")
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should include status code, got %v", err)
	}
}

// TestLogin_MissingCSRFCookie covers the case where the server returns
// 200 but somehow forgot to set csrf_token (broken deploy).
func TestLogin_MissingCSRFCookie(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "auth_token", Value: "x", Path: "/"})
		// no csrf cookie set
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, _, err := login(context.Background(), srv.URL, "e", "p")
	if err == nil {
		t.Fatal("expected error when csrf cookie missing")
	}
	if !strings.Contains(err.Error(), "csrf_token") {
		t.Errorf("error should mention csrf_token, got %v", err)
	}
}

// TestPostPosition_HappyPath verifies the request shape (method, path,
// CSRF header echo, body keys) and that 201 is treated as success.
func TestPostPosition_HappyPath(t *testing.T) {
	var seenCSRF string
	var seenBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/positions" || r.Method != http.MethodPost {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		seenCSRF = r.Header.Get("X-CSRF-Token")
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	err := postPosition(context.Background(), client, srv.URL, "csrf-xyz", "veh_1", 13.7, 100.5, 35)
	if err != nil {
		t.Fatalf("postPosition: %v", err)
	}
	if seenCSRF != "csrf-xyz" {
		t.Errorf("X-CSRF-Token: got %q, want csrf-xyz", seenCSRF)
	}
	if seenBody["vehicle_id"] != "veh_1" {
		t.Errorf("body.vehicle_id: got %v", seenBody["vehicle_id"])
	}
	if seenBody["lat"].(float64) != 13.7 {
		t.Errorf("body.lat: got %v", seenBody["lat"])
	}
	if seenBody["lng"].(float64) != 100.5 {
		t.Errorf("body.lng: got %v", seenBody["lng"])
	}
	if seenBody["speed_kmh"].(float64) != 35 {
		t.Errorf("body.speed_kmh: got %v", seenBody["speed_kmh"])
	}
	if _, ok := seenBody["recorded_at"]; !ok {
		t.Error("body missing recorded_at")
	}
}

// TestPostPosition_Non201ReturnsError covers a 400/429 server response.
func TestPostPosition_Non201ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate_limited"}`)
	}))
	defer srv.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	err := postPosition(context.Background(), client, srv.URL, "csrf", "veh", 0, 0, 0)
	if err == nil {
		t.Fatal("expected error on 429, got nil")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error should include status code, got %v", err)
	}
}

// TestRun_ShutdownOnContextCancel proves that ctx cancellation is the
// graceful-shutdown contract: run() returns nil (not an error) when
// the caller signals shutdown. We point it at a stub that handles
// both /api/auth/login and /api/positions so the loop can actually
// start before cancellation.
func TestRun_ShutdownOnContextCancel(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "csrf_token", Value: "csrf", Path: "/"})
			w.WriteHeader(http.StatusOK)
		case "/api/positions":
			posts.Add(1)
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cfg := config{
		Email: "e", Password: "p", VehicleID: "v",
		BaseURL:  srv.URL,
		Interval: time.Second, // smallest legal interval; we cancel before it fires
		SpeedKmh: 10,
	}
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg) }()

	// Wait until the first immediate post lands, then cancel.
	deadline := time.Now().Add(2 * time.Second)
	for posts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if posts.Load() == 0 {
		cancel()
		<-done
		t.Fatal("simulator did not post immediately on startup")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned error on shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not exit within 3s of cancel")
	}
}
