package publisher

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/pkg/cfclient"
)

// newTestPublisher spins an httptest.Server with the supplied handler
// and returns a FleetPublisher pointed at it. The server is registered
// with t.Cleanup so the test pool closes deterministically — no
// goroutine leaks under -race.
//
// Kept private + factored so every test case stays one screen and the
// "publisher backed by a real Durable Object client backed by a real
// HTTP server" wiring is built once.
func newTestPublisher(t *testing.T, handler http.HandlerFunc) *FleetPublisher {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	doClient, err := cfclient.NewDurableClient(cfclient.DurableConfig{
		PublishURL: srv.URL + "/internal/publish",
		Secret:     "test-secret-x",
	})
	if err != nil {
		t.Fatalf("NewDurableClient: %v", err)
	}

	pub, err := New(doClient)
	if err != nil {
		t.Fatalf("publisher.New: %v", err)
	}
	return pub
}

func TestNew_NilClientErrors(t *testing.T) {
	// A nil DurableClient is a programmer error. The constructor must
	// refuse rather than letting a NPE surface on first Publish.
	if _, err := New(nil); err == nil {
		t.Fatal("New(nil) must return an error")
	}
}

func TestNew_ValidClientSucceeds(t *testing.T) {
	doClient, err := cfclient.NewDurableClient(cfclient.DurableConfig{
		PublishURL: "http://localhost:0/internal/publish",
		Secret:     "s",
	})
	if err != nil {
		t.Fatalf("NewDurableClient: %v", err)
	}
	pub, err := New(doClient)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if pub == nil {
		t.Fatal("New must return a non-nil publisher")
	}
}

func TestPublishPositionUpdate_NilPositionErrors(t *testing.T) {
	// Nil-position must short-circuit before the HTTP layer is touched.
	// The test handler fails loudly if it sees a request — proves no
	// network I/O happened on the bad-input branch.
	pub := newTestPublisher(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("HTTP handler should not be invoked when pos is nil")
	})
	if err := pub.PublishPositionUpdate(context.Background(), nil); err == nil {
		t.Fatal("PublishPositionUpdate(nil) must return an error")
	}
}

// captured holds everything the test server sees so each assertion is
// a flat field access rather than re-parsing the request inside the
// handler closure. The fields mirror what we want to verify about the
// position.update wire shape.
type captured struct {
	method      string
	path        string
	contentType string
	signature   string
	body        []byte
}

func TestPublishPositionUpdate_PostsExpectedRequest(t *testing.T) {
	// Happy path: a populated domain.Position must produce a POST with
	// the right method/path/header set and a JSON body the DO would
	// accept as a position.update event.
	var got captured
	pub := newTestPublisher(t, func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.contentType = r.Header.Get("Content-Type")
		got.signature = r.Header.Get("X-Signature")
		got.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})

	pos := &domain.Position{
		ID:         42,
		VehicleID:  "veh-bangkok-1",
		Lat:        13.7563,
		Lng:        100.5018,
		SpeedKmh:   42.5,
		RecordedAt: 1_700_000_000_000,
		CreatedAt:  1_700_000_000_500,
	}
	if err := pub.PublishPositionUpdate(context.Background(), pos); err != nil {
		t.Fatalf("PublishPositionUpdate: %v", err)
	}

	// Method + path.
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/internal/publish" {
		t.Errorf("path = %q, want /internal/publish", got.path)
	}

	// Content-Type set by DurableClient.
	if got.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got.contentType)
	}

	// X-Signature must be present. The signature value itself is
	// DurableClient's concern (the cfclient test suite verifies the
	// HMAC byte-for-byte); here we just confirm a hex digest of the
	// right length actually rode along.
	if got.signature == "" {
		t.Error("X-Signature header must be set")
	}
	if len(got.signature) != 64 {
		t.Errorf("X-Signature length = %d, want 64 (sha256 hex)", len(got.signature))
	}

	// Body must round-trip into the wire-shape struct with every field
	// populated from the source position. The DO's isFleetEvent guard
	// (workers/fleet-hub) rejects events missing any of these keys, so
	// each one is asserted.
	var ev positionUpdateEvent
	if err := json.Unmarshal(got.body, &ev); err != nil {
		t.Fatalf("unmarshal body: %v\nbody=%s", err, got.body)
	}
	if ev.Type != "position.update" {
		t.Errorf("type = %q, want position.update", ev.Type)
	}
	if ev.VehicleID != pos.VehicleID {
		t.Errorf("vehicle_id = %q, want %q", ev.VehicleID, pos.VehicleID)
	}
	if ev.Lat != pos.Lat {
		t.Errorf("lat = %v, want %v", ev.Lat, pos.Lat)
	}
	if ev.Lng != pos.Lng {
		t.Errorf("lng = %v, want %v", ev.Lng, pos.Lng)
	}
	if ev.RecordedAt != pos.RecordedAt {
		t.Errorf("recorded_at = %d, want %d", ev.RecordedAt, pos.RecordedAt)
	}
}

func TestPublishPositionUpdate_OmitsExtraFields(t *testing.T) {
	// The wire envelope must not leak fields the DO doesn't know about.
	// Specifically: ID, SpeedKmh, CreatedAt are server-side concepts and
	// must NOT appear in the JSON — the DO's isFleetEvent guard inspects
	// the keys, so an extra `id` or `created_at` would be rejected.
	var rawBody []byte
	pub := newTestPublisher(t, func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})

	pos := &domain.Position{
		ID:         99, // must NOT leak
		VehicleID:  "veh-1",
		Lat:        1.0,
		Lng:        2.0,
		SpeedKmh:   10.0, // must NOT leak
		RecordedAt: 1_700_000_000_000,
		CreatedAt:  1_700_000_000_500, // must NOT leak
	}
	if err := pub.PublishPositionUpdate(context.Background(), pos); err != nil {
		t.Fatalf("PublishPositionUpdate: %v", err)
	}

	bodyStr := string(rawBody)
	for _, forbidden := range []string{`"id"`, `"speed_kmh"`, `"created_at"`, `"SpeedKmh"`, `"CreatedAt"`} {
		if strings.Contains(bodyStr, forbidden) {
			t.Errorf("body must not contain %s; got: %s", forbidden, bodyStr)
		}
	}

	// And the four legitimate keys MUST be present.
	for _, required := range []string{`"type"`, `"vehicle_id"`, `"lat"`, `"lng"`, `"recorded_at"`} {
		if !strings.Contains(bodyStr, required) {
			t.Errorf("body missing required key %s; got: %s", required, bodyStr)
		}
	}
}

func TestPublishPositionUpdate_UpstreamErrorPropagates(t *testing.T) {
	// Non-2xx from the gateway must surface as an error so the
	// usecase's best-effort log path is exercised. We test both an
	// auth-style 401 (so we can errors.Is against ErrUnauthorized) and
	// a generic 5xx (so we can confirm those don't get classified as
	// auth failures).

	t.Run("401 maps to ErrUnauthorized", func(t *testing.T) {
		pub := newTestPublisher(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, "bad sig")
		})
		pos := &domain.Position{VehicleID: "v", Lat: 0, Lng: 0, RecordedAt: 1}
		err := pub.PublishPositionUpdate(context.Background(), pos)
		if err == nil {
			t.Fatal("401 must propagate as an error")
		}
		if !errors.Is(err, cfclient.ErrUnauthorized) {
			t.Errorf("expected ErrUnauthorized, got: %v", err)
		}
	})

	t.Run("500 propagates as a non-auth error", func(t *testing.T) {
		pub := newTestPublisher(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "DO blew up")
		})
		pos := &domain.Position{VehicleID: "v", Lat: 0, Lng: 0, RecordedAt: 1}
		err := pub.PublishPositionUpdate(context.Background(), pos)
		if err == nil {
			t.Fatal("500 must propagate as an error")
		}
		if errors.Is(err, cfclient.ErrUnauthorized) {
			t.Errorf("500 must NOT be classified as ErrUnauthorized: %v", err)
		}
	})
}

func TestPublishPositionUpdate_ContextCancelled(t *testing.T) {
	// Cancelling the parent context before the call must surface as an
	// error — the publisher does NOT swallow cancellations. Important
	// because the usecase wraps everything in a request-scoped context;
	// a cancelled request must not block on the publish.
	pub := newTestPublisher(t, func(_ http.ResponseWriter, _ *http.Request) {
		// Handler should not be reached, but if it is, do nothing — the
		// http stack will already have returned an error to the caller.
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE the call so the round-trip can't start

	pos := &domain.Position{VehicleID: "v", Lat: 0, Lng: 0, RecordedAt: 1}
	if err := pub.PublishPositionUpdate(ctx, pos); err == nil {
		t.Fatal("cancelled context must surface as an error")
	}
}
