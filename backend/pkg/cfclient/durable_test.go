package cfclient

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newDurableTestClient(t *testing.T, handler http.HandlerFunc) (*DurableClient, string) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	const secret = "hmac-secret-xyz"
	c, err := NewDurableClient(DurableConfig{
		PublishURL: srv.URL + "/internal/publish",
		Secret:     secret,
	})
	if err != nil {
		t.Fatalf("NewDurableClient: %v", err)
	}
	return c, secret
}

func TestNewDurableClient_RequiredFields(t *testing.T) {
	tests := []struct {
		name string
		cfg  DurableConfig
		want string
	}{
		{"missing PublishURL", DurableConfig{Secret: "s"}, "PublishURL"},
		{"missing Secret", DurableConfig{PublishURL: "http://x/p"}, "Secret"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewDurableClient(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

// computeHMAC mirrors the client's signing so tests can verify the
// signature byte-for-byte.
func computeHMAC(t *testing.T, secret string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestDurable_Publish_SignsAndPosts(t *testing.T) {
	type positionUpdate struct {
		Type      string  `json:"type"`
		VehicleID string  `json:"vehicle_id"`
		Lat       float64 `json:"lat"`
		Lng       float64 `json:"lng"`
	}
	event := positionUpdate{
		Type:      "position.update",
		VehicleID: "veh-1",
		Lat:       13.7563,
		Lng:       100.5018,
	}

	var (
		gotMethod string
		gotPath   string
		gotCT     string
		gotSig    string
		gotBody   []byte
	)
	c, secret := newDurableTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		gotSig = r.Header.Get("X-Signature")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})

	if err := c.Publish(context.Background(), event); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q", gotMethod)
	}
	if gotPath != "/internal/publish" {
		t.Errorf("path = %q", gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}

	// Body must round-trip the event with the same field ordering JSON
	// would produce; instead of asserting the exact byte string, decode
	// and compare structurally.
	var roundTrip positionUpdate
	if err := json.Unmarshal(gotBody, &roundTrip); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if roundTrip != event {
		t.Fatalf("body roundtrip mismatch: %+v vs %+v", roundTrip, event)
	}

	// Signature must match HMAC-SHA256(body, secret).
	wantSig := computeHMAC(t, secret, gotBody)
	if gotSig != wantSig {
		t.Fatalf("signature mismatch:\n got %q\nwant %q", gotSig, wantSig)
	}
	// Belt-and-braces: hex output is 64 chars for SHA-256.
	if len(gotSig) != 64 {
		t.Fatalf("signature length = %d, want 64 (hex sha256)", len(gotSig))
	}
}

func TestDurable_Publish_SigChangesWithBody(t *testing.T) {
	// Send two distinct events; their signatures must differ. This
	// catches the "we hashed something constant" class of bugs.
	var sigs []string
	c, _ := newDurableTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		sigs = append(sigs, r.Header.Get("X-Signature"))
		w.WriteHeader(http.StatusOK)
	})
	if err := c.Publish(context.Background(), map[string]any{"k": "v1"}); err != nil {
		t.Fatalf("publish 1: %v", err)
	}
	if err := c.Publish(context.Background(), map[string]any{"k": "v2"}); err != nil {
		t.Fatalf("publish 2: %v", err)
	}
	if len(sigs) != 2 {
		t.Fatalf("expected 2 sigs, got %d", len(sigs))
	}
	if sigs[0] == sigs[1] {
		t.Fatal("signatures should differ between distinct payloads")
	}
}

func TestDurable_Publish_Unauthorized(t *testing.T) {
	c, _ := newDurableTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "bad sig")
	})
	err := c.Publish(context.Background(), map[string]any{"t": "x"})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

func TestDurable_Publish_5xx(t *testing.T) {
	c, _ := newDurableTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	})
	err := c.Publish(context.Background(), map[string]any{"t": "x"})
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if errors.Is(err, ErrUnauthorized) {
		t.Fatalf("500 should not be ErrUnauthorized: %v", err)
	}
}

func TestDurable_Publish_MarshalFailure(t *testing.T) {
	c, _ := NewDurableClient(DurableConfig{
		PublishURL: "http://localhost:0/p",
		Secret:     "s",
	})
	// chan is unencodable by encoding/json — forces a marshal error
	// without making any HTTP call.
	err := c.Publish(context.Background(), make(chan int))
	if err == nil {
		t.Fatal("expected marshal error")
	}
	if !strings.Contains(err.Error(), "marshal") {
		t.Fatalf("expected marshal in error, got %v", err)
	}
}
