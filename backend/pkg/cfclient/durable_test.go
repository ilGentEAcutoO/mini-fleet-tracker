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
	"strconv"
	"strings"
	"testing"
	"time"
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
//
// LEGACY contract — body-only HMAC. Retained for the legacy-fallback
// tests (the gateway + DO verifier accept both modes during the 24h
// rollout window) but new tests should compose via computeHMACNew.
func computeHMAC(t *testing.T, secret string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// computeHMACNew mirrors the TASK-051 contract:
//
//	sig = HMAC-SHA256(body || '\n' || ts, secret)
//
// where ts is the unix-seconds string carried in the X-Timestamp header.
// Bytes-identical to the gateway + DO verifier in
// workers/gateway/src/index.ts and workers/fleet-hub/src/fleet-hub.ts.
// Tests that drift from this exact byte composition will produce a
// signature the verifiers reject, which is the point of mirroring the
// contract here.
func computeHMACNew(t *testing.T, secret string, body []byte, ts string) string {
	t.Helper()
	joined := append([]byte{}, body...)
	joined = append(joined, '\n')
	joined = append(joined, []byte(ts)...)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(joined)
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
		gotTS     string
		gotBody   []byte
	)
	c, secret := newDurableTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		gotSig = r.Header.Get("X-Signature")
		gotTS = r.Header.Get("X-Timestamp")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})

	beforePublish := time.Now().Unix()
	if err := c.Publish(context.Background(), event); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	afterPublish := time.Now().Unix()

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

	// TASK-051: X-Timestamp must be present and parse as a unix-seconds
	// integer within the same wall-clock window as the call.
	if gotTS == "" {
		t.Fatal("X-Timestamp header missing (TASK-051 replay protection)")
	}
	tsInt, err := strconv.ParseInt(gotTS, 10, 64)
	if err != nil {
		t.Fatalf("X-Timestamp %q is not an integer: %v", gotTS, err)
	}
	if tsInt < beforePublish || tsInt > afterPublish {
		t.Errorf("X-Timestamp = %d, want in [%d, %d]", tsInt, beforePublish, afterPublish)
	}

	// Signature MUST match the new contract HMAC-SHA256(body || '\n' || ts, secret).
	// The verifier accepts only this composition for new-mode requests; the
	// 24h legacy fallback (body-only) is unrelated to this assertion.
	wantSig := computeHMACNew(t, secret, gotBody, gotTS)
	if gotSig != wantSig {
		t.Fatalf("signature mismatch:\n got %q\nwant %q\n(must be HMAC over body || \\n || ts)", gotSig, wantSig)
	}
	// Belt-and-braces: hex output is 64 chars for SHA-256.
	if len(gotSig) != 64 {
		t.Fatalf("signature length = %d, want 64 (hex sha256)", len(gotSig))
	}
}

// TestDurable_Publish_SignatureBindsToTimestamp is the replay-protection
// regression: two publishes of the IDENTICAL body must produce different
// signatures whenever their X-Timestamp differs. This is what blocks
// captured-request replay — the verifier (gateway + DO) enforces a ±30s
// window AND requires the signature to bind the timestamp. If a refactor
// ever drops ts from the HMAC input, this test catches it before deploy.
func TestDurable_Publish_SignatureBindsToTimestamp(t *testing.T) {
	type capture struct {
		sig string
		ts  string
		bod []byte
	}
	caps := make([]capture, 0, 2)

	c, secret := newDurableTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		caps = append(caps, capture{
			sig: r.Header.Get("X-Signature"),
			ts:  r.Header.Get("X-Timestamp"),
			bod: body,
		})
		w.WriteHeader(http.StatusOK)
	})

	// Same event published twice. The body bytes will be identical because
	// json.Marshal is deterministic for the same struct; only X-Timestamp
	// can vary across the two calls. We sleep ~1.1s so the unix-seconds
	// timestamp must roll over at least once.
	event := map[string]any{"type": "position.update", "vehicle_id": "v1", "lat": 1.0, "lng": 2.0}
	if err := c.Publish(context.Background(), event); err != nil {
		t.Fatalf("publish 1: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := c.Publish(context.Background(), event); err != nil {
		t.Fatalf("publish 2: %v", err)
	}

	if len(caps) != 2 {
		t.Fatalf("expected 2 captures, got %d", len(caps))
	}
	if string(caps[0].bod) != string(caps[1].bod) {
		t.Fatalf("test setup error: bodies differ\n%s\n%s", caps[0].bod, caps[1].bod)
	}
	if caps[0].ts == caps[1].ts {
		t.Fatal("test setup error: timestamps equal — sleep was insufficient")
	}
	if caps[0].sig == caps[1].sig {
		t.Fatalf("signatures must differ when timestamps differ; both = %q (replay protection broken)", caps[0].sig)
	}

	// Belt-and-braces: each signature must match the computeHMACNew of
	// its own body + ts pair under the same secret.
	for i, cap := range caps {
		want := computeHMACNew(t, secret, cap.bod, cap.ts)
		if cap.sig != want {
			t.Errorf("publish %d: sig = %q, want %q", i, cap.sig, want)
		}
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
