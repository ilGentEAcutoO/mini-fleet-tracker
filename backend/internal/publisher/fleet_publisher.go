// Package publisher adapts the typed cfclient.DurableClient HTTP
// transport to the EventPublisher contract that the position usecase
// depends on. The split exists so the usecase package owns its narrow
// dependency interface (see internal/usecase.EventPublisher) and the
// wire-level concerns — HMAC signing, JSON encoding, timeouts — stay
// in pkg/cfclient where every other Cloudflare-facing client lives.
//
// The package contains no business logic. The only translation it does
// is shaping a domain.Position into the position.update event envelope
// the Durable Object's webSocketMessage broadcast hook already
// understands (see workers/fleet-hub/src/fleet-hub.ts:FleetEvent).
// Changing the wire envelope means changing both ends in lockstep.
package publisher

import (
	"context"
	"errors"
	"fmt"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/pkg/cfclient"
)

// FleetPublisher implements usecase.EventPublisher by delegating to a
// cfclient.DurableClient. It is intentionally a thin shell: nil-checks
// in the constructor and the publish method, plus a single struct-to-
// event-envelope conversion. Anything richer (retry, batching,
// best-effort circuit breaker) belongs in a wrapper around this type,
// not inside it.
//
// The struct is safe for concurrent use as long as the underlying
// cfclient.DurableClient is — which it is, by construction.
//
// Note on compile-time assertion: we deliberately do NOT add
//
//	var _ usecase.EventPublisher = (*FleetPublisher)(nil)
//
// here, because importing internal/usecase from this package would
// invert the dependency direction (usecase is the consumer; publisher
// is the implementation). The contract is still type-checked at the
// wiring site in cmd/api/bootstrap.go where NewPositionUsecase
// receives a *FleetPublisher — Go's structural interface satisfaction
// makes the assertion functionally equivalent. If FleetPublisher ever
// drifts from EventPublisher, bootstrap.go stops compiling, which is
// the same blast radius as a misplaced `var _` would give us.
type FleetPublisher struct {
	client *cfclient.DurableClient
}

// New returns a publisher that broadcasts events via the supplied
// Durable Object client. A nil client is a programmer error — return
// an error so the caller fails at bootstrap rather than at first
// request, matching the pattern every other constructor in this
// codebase uses for required dependencies.
func New(client *cfclient.DurableClient) (*FleetPublisher, error) {
	if client == nil {
		return nil, errors.New("publisher: durable client is required")
	}
	return &FleetPublisher{client: client}, nil
}

// positionUpdateEvent is the JSON wire shape the Durable Object's
// broadcast hook expects (workers/fleet-hub/src/fleet-hub.ts:FleetEvent
// — the position.update variant). Field tags lowercase + snake_case
// so the JSON keys match the TypeScript side byte-for-byte; changing
// either end without the other will trip the DO's isFleetEvent guard
// and the event will be rejected at /publish time.
//
// Type is a constant in practice — only set by PublishPositionUpdate
// below. Kept as a struct field rather than a method-local because
// json.Marshal needs a stable field to emit; a constant in the
// struct literal is the cleanest expression of "this value is always
// position.update for this event variant".
type positionUpdateEvent struct {
	Type       string  `json:"type"`        // always "position.update"
	VehicleID  string  `json:"vehicle_id"`  // domain.Position.VehicleID
	Lat        float64 `json:"lat"`         // domain.Position.Lat
	Lng        float64 `json:"lng"`         // domain.Position.Lng
	RecordedAt int64   `json:"recorded_at"` // domain.Position.RecordedAt (unix-ms)
}

// PublishPositionUpdate sends a position.update event to the Durable
// Object via the HMAC-signed POST that DurableClient.Publish handles.
//
// Errors are returned to the caller verbatim — the usecase layer is
// the only thing that decides what "publish failure" means in context,
// and currently treats it as best-effort (logged at warn, request
// still succeeds because the position is already durable in D1).
// Keeping this method strict-return makes the seam easy to wrap with a
// retry layer later without rewriting the usecase.
//
// A nil position is a programmer error — guarded explicitly so a
// caller bug surfaces as a clean error rather than a marshal panic
// downstream.
func (p *FleetPublisher) PublishPositionUpdate(ctx context.Context, pos *domain.Position) error {
	if pos == nil {
		return fmt.Errorf("publisher: nil position")
	}
	return p.client.Publish(ctx, positionUpdateEvent{
		Type:       "position.update",
		VehicleID:  pos.VehicleID,
		Lat:        pos.Lat,
		Lng:        pos.Lng,
		RecordedAt: pos.RecordedAt,
	})
}

// geofenceAlertEvent is the JSON wire shape the Durable Object's
// isFleetEvent guard expects for geofence.alert events
// (workers/fleet-hub/src/fleet-hub.ts:FleetEvent). Keys are
// lower_snake_case so the JSON matches the TypeScript side
// byte-for-byte; AlertType is constrained to "enter" or "exit" by the
// caller (PositionUsecase) before this struct is constructed — we do
// not validate again here because there is exactly one call site.
//
// Note on the "alert_type" key naming: the DO guard inspects this
// exact string. Renaming the key (to e.g. "type") would silently
// break the broadcast — the event would be rejected at /publish time
// with a 400 — so the JSON tag is load-bearing.
type geofenceAlertEvent struct {
	Type      string `json:"type"`       // always "geofence.alert"
	VehicleID string `json:"vehicle_id"` // owning vehicle
	AlertType string `json:"alert_type"` // "enter" | "exit"
	At        int64  `json:"at"`         // unix-ms when the transition happened
}

// PublishGeofenceAlert sends a geofence.alert event to the Durable
// Object via the same HMAC-signed POST mechanism. Same best-effort
// semantics as PublishPositionUpdate — the usecase logs failures at
// warn level and the request still succeeds.
//
// vehicleID and alertType are required and non-empty (the usecase
// guarantees this; we do not double-validate at this seam). The `at`
// timestamp is the position's recorded_at, not the server's now —
// operators care about when the transition happened in the field.
//
// alertType "enter" | "exit" is enforced by the upstream usecase via a
// branch on inside/outside transition direction. We deliberately keep
// the parameter as a string rather than a typed enum because the
// EventPublisher interface ships through three layers (usecase,
// publisher, cfclient) and adding a domain-level enum would create
// import-cycle pain for no real safety dividend — the value is set
// from exactly one branch in one function.
func (p *FleetPublisher) PublishGeofenceAlert(ctx context.Context, vehicleID, alertType string, at int64) error {
	return p.client.Publish(ctx, geofenceAlertEvent{
		Type:      "geofence.alert",
		VehicleID: vehicleID,
		AlertType: alertType,
		At:        at,
	})
}
