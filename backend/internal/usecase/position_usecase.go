package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/pkg/geo"
)

// positionRepo is the narrow contract PositionUsecase needs from the
// position storage layer. Declared at the consumer site so the usecase
// owns its dependency and tests can mock with minimal surface area.
//
// Insert populates p.ID (DB auto-increment) and p.CreatedAt
// (server-stamped) before returning; the caller's other fields are
// passed through unchanged.
//
// GetMostRecentByVehicleBeforeID (added in TASK-020) lets the usecase
// look up the immediate predecessor of a just-inserted position. Used
// by the geofence transition-detection step to compare inside/outside
// state across two consecutive readings. Returns domain.ErrNotFound
// when the just-inserted position was the vehicle's first ever — the
// transition-detection logic treats that as "no previous reading, no
// alert".
type positionRepo interface {
	Insert(ctx context.Context, p *domain.Position) error
	GetMostRecentByVehicleBeforeID(ctx context.Context, vehicleID string, excludeID int64) (*domain.Position, error)
}

// geofenceLookup is the narrow contract PositionUsecase needs for the
// transition-detection step. The production
// internal/repository/d1.GeofenceRepo satisfies this directly via its
// GetByVehicle method.
//
// Implementations should return (nil, domain.ErrNotFound) when no fence
// is configured for the vehicle. The transition-detection logic treats
// that as "no fence, no alert" — most vehicles in the fleet will not
// have a fence set, so the no-fence path must be cheap and silent.
type geofenceLookup interface {
	GetByVehicle(ctx context.Context, vehicleID string) (*domain.Geofence, error)
}

// vehicleLookup is the narrow contract PositionUsecase needs for the
// driver-ownership check. The concrete VehicleRepo from TASK-010
// satisfies this directly via its Get(ctx, id) method — declaring a
// local interface here keeps the dependency direction inward (the
// usecase owns its contract) and lets tests inject a fake without
// instantiating a real repo.
//
// Implementations should return (nil, domain.ErrNotFound) when the
// vehicle does not exist. Any other error is bubbled to the handler
// and surfaces as 500.
type vehicleLookup interface {
	Get(ctx context.Context, id string) (*domain.Vehicle, error)
}

// EventPublisher is the seam where TASK-014 plugs in the Durable
// Object publisher (POST to gateway /internal/publish with HMAC-signed
// body). For TASK-011 the production wiring uses NoopPublisher; tests
// inject a spy to verify the call shape would be correct when TASK-014
// wires the real publisher.
//
// The contract is intentionally narrow — one method per event type — so
// the real implementation (publisher.FleetPublisher) can be swapped in
// without touching the usecase.
//
// PublishGeofenceAlert (added in TASK-020) carries the per-vehicle
// enter/exit event the DO already understands per its
// workers/fleet-hub broadcast hook. alertType MUST be exactly "enter"
// or "exit"; the caller (this package's PositionUsecase) is the only
// thing that constructs the value, so an enum type would be overkill.
type EventPublisher interface {
	PublishPositionUpdate(ctx context.Context, p *domain.Position) error
	PublishGeofenceAlert(ctx context.Context, vehicleID string, alertType string, at int64) error
}

// NoopPublisher satisfies EventPublisher without doing anything.
// Production wires this until TASK-014 supplies the real Durable Object
// HTTP client. Keeping the type exported (rather than as an
// unexported zero value) lets the wiring code state its intent
// explicitly: `usecase.NoopPublisher{}` in bootstrap.go reads as
// "deliberately no broadcast yet".
type NoopPublisher struct{}

// PublishPositionUpdate is a no-op. Returns nil so the usecase's
// best-effort publish path is exercised in production too — keeping
// the code path warm avoids a "this only runs in tests" surprise when
// TASK-014 lands.
func (NoopPublisher) PublishPositionUpdate(_ context.Context, _ *domain.Position) error {
	return nil
}

// PublishGeofenceAlert is a no-op for the dev wiring path. Same
// rationale as PublishPositionUpdate: keeps the code path warm in
// production environments that have not yet configured the DO publish
// secret.
func (NoopPublisher) PublishGeofenceAlert(_ context.Context, _ string, _ string, _ int64) error {
	return nil
}

// Compile-time assertion that NoopPublisher satisfies EventPublisher.
// Keeps the contract stable if either side drifts.
var _ EventPublisher = NoopPublisher{}

// positionFreshnessWindow bounds how far a caller-supplied recorded_at
// timestamp may drift from the server's now. ±5 minutes balances two
// concerns:
//   - clock skew on driver devices is rarely more than a minute or two
//     after a successful NTP sync; 5 minutes gives generous headroom
//   - replay-style spoofing of "old" positions is bounded — a stolen
//     credential cannot stitch together a fake history older than five
//     minutes per submission
//
// The window is symmetric (a recorded_at in the near future is also
// rejected) because a future timestamp would indicate either a badly
// synced clock or a deliberate attempt to push out the cutoff for the
// next valid window.
const positionFreshnessWindow = 5 * time.Minute

// positionMaxSpeedKmh is the upper bound for SpeedKmh validation.
// 500 km/h covers commercial-aviation ground speeds, well above any
// land vehicle we'd track on a fleet — yet still a clear "this is
// telemetry noise" gate (a corrupted reading easily produces values
// in the millions). Lower than this and we risk rejecting legitimate
// outlier readings from high-speed rail; higher and the gate stops
// catching obviously-broken sensors.
const positionMaxSpeedKmh = 500.0

// PositionUsecase wires the position-write workflow. Dependencies are
// immutable after construction; the struct is safe for concurrent use
// as long as the injected adapters are.
//
// fences is OPTIONAL (may be nil) — when nil the geofence transition-
// detection step is skipped entirely. Callers wiring the usecase
// before TASK-020 ships in their environment can pass nil to opt out;
// production wiring should always pass the real GeofenceRepo so live
// alerts work.
type PositionUsecase struct {
	positions positionRepo
	vehicles  vehicleLookup
	fences    geofenceLookup // optional — nil disables transition detection
	publisher EventPublisher
	// now is the testable clock used for freshness validation.
	// Production leaves it nil and time.Now is used.
	now func() time.Time
}

// NewPositionUsecase constructs a usecase from its dependencies.
// positions, vehicles, and publisher are required; passing any nil
// returns an error rather than panicking later in the request path.
//
// fences is OPTIONAL — passing nil disables the geofence transition-
// detection step. This keeps the constructor backwards-compatible with
// callers wired before TASK-020 (only the bootstrap wiring needs to
// be updated to pass the real fences repo; old tests stay green).
//
// publisher may be NoopPublisher{} in environments without a real DO
// publish URL — see the EventPublisher doc comment. It cannot be a
// literal nil because the usecase calls publish methods
// unconditionally (with error logging on failure); a nil interface
// would NPE.
func NewPositionUsecase(
	positions positionRepo,
	vehicles vehicleLookup,
	publisher EventPublisher,
	opts ...PositionUsecaseOption,
) (*PositionUsecase, error) {
	if positions == nil {
		return nil, errors.New("position usecase: positions repo is required")
	}
	if vehicles == nil {
		return nil, errors.New("position usecase: vehicles repo is required")
	}
	if publisher == nil {
		return nil, errors.New("position usecase: publisher is required (use NoopPublisher{} explicitly)")
	}
	u := &PositionUsecase{
		positions: positions,
		vehicles:  vehicles,
		publisher: publisher,
	}
	for _, opt := range opts {
		opt(u)
	}
	return u, nil
}

// PositionUsecaseOption is the functional-options shape for
// NewPositionUsecase. The functional-options pattern is preferred here
// over additional positional arguments because (a) the fences
// dependency is optional and (b) future TASK-* may add more optional
// hooks (e.g. an anomaly detector) without breaking every caller.
type PositionUsecaseOption func(*PositionUsecase)

// WithGeofences enables the TASK-020 transition-detection step by
// supplying the geofence lookup. Production wiring in bootstrap.go
// passes the d1.GeofenceRepo here; tests that exercise the alert
// branches pass an in-memory fake.
func WithGeofences(fences geofenceLookup) PositionUsecaseOption {
	return func(u *PositionUsecase) {
		u.fences = fences
	}
}

// nowFunc returns the test-overridable clock or time.Now in production.
func (u *PositionUsecase) nowFunc() time.Time {
	if u.now != nil {
		return u.now()
	}
	return time.Now()
}

// WritePositionInput is the validated-input shape the handler hands to
// Write. Fields mirror the handler request DTO so the conversion at the
// boundary is trivial.
type WritePositionInput struct {
	VehicleID string
	Lat       float64
	Lng       float64
	// SpeedKmh is optional. The Go zero value 0.0 means "unset" — the
	// usecase only enforces the [0, 500] range when the field is
	// non-zero. See the domain.Position doc comment for why we model
	// "unset" as zero rather than *float64.
	SpeedKmh   float64
	RecordedAt int64 // unix-ms; caller-provided GPS timestamp
}

// Write validates the input, verifies the driver owns the target
// vehicle, persists the position to D1, and best-effort publishes a
// `position.update` event to the Durable Object for live broadcast.
//
// Validation order is deliberately the cheap-first ladder: pure field
// checks (no I/O) precede the DB lookup, so a malformed request is
// rejected without burning a D1 round-trip. The DB lookup precedes
// the insert, so a wrong-owner reject doesn't write a row that has to
// be cleaned up.
//
// The publish step is best-effort: a publisher failure is logged at
// warn level but does NOT fail the request. The position is durably
// written before the publisher is called, so the SPA can refetch
// history to recover from a missed broadcast — the WS push is a
// notification convenience, not the source of truth.
//
// On success the returned Position has ID and CreatedAt populated by
// the repo; other fields are echoed back from the input.
func (u *PositionUsecase) Write(
	ctx context.Context,
	driverID string,
	in WritePositionInput,
) (*domain.Position, error) {
	driverID = strings.TrimSpace(driverID)
	if driverID == "" {
		// The handler/middleware should never call us without a driverID,
		// but defensive checks here mean an upstream bug surfaces as a
		// 400 not a panic.
		return nil, fmt.Errorf("driver id is required: %w", domain.ErrValidation)
	}

	in.VehicleID = strings.TrimSpace(in.VehicleID)
	if in.VehicleID == "" {
		return nil, fmt.Errorf("vehicle_id is required: %w", domain.ErrValidation)
	}

	// Coordinate bounds — fail fast on numeric out-of-range before any
	// I/O. validator/v10's latitude/longitude tags catch this at the
	// handler boundary too; the usecase re-checks so a non-HTTP caller
	// (e.g. the seed script) cannot bypass the gate.
	if in.Lat < -90 || in.Lat > 90 {
		return nil, fmt.Errorf("lat %f out of range [-90, 90]: %w", in.Lat, domain.ErrValidation)
	}
	if in.Lng < -180 || in.Lng > 180 {
		return nil, fmt.Errorf("lng %f out of range [-180, 180]: %w", in.Lng, domain.ErrValidation)
	}

	// Speed bound — only when non-zero. Zero means "unset" by domain
	// convention (see domain.Position).
	if in.SpeedKmh != 0 {
		if in.SpeedKmh < 0 || in.SpeedKmh > positionMaxSpeedKmh {
			return nil, fmt.Errorf("speed_kmh %f out of range [0, %f]: %w",
				in.SpeedKmh, positionMaxSpeedKmh, domain.ErrValidation)
		}
	}

	// Freshness — symmetric ±5 minutes around the server clock.
	now := u.nowFunc()
	recordedAt := time.UnixMilli(in.RecordedAt)
	earliest := now.Add(-positionFreshnessWindow)
	latest := now.Add(positionFreshnessWindow)
	if recordedAt.Before(earliest) {
		return nil, fmt.Errorf("recorded_at too old (>%s in the past): %w",
			positionFreshnessWindow, domain.ErrValidation)
	}
	if recordedAt.After(latest) {
		return nil, fmt.Errorf("recorded_at too far in the future (>%s ahead): %w",
			positionFreshnessWindow, domain.ErrValidation)
	}

	// Driver-owns-vehicle check. ErrNotFound from the repo is propagated
	// verbatim (the handler maps it to 404); any other repo error wraps
	// into a generic "lookup failed" with the original chained through.
	vehicle, err := u.vehicles.Get(ctx, in.VehicleID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, err // propagate ErrNotFound verbatim for the handler
		}
		return nil, fmt.Errorf("position write: vehicle lookup: %w", err)
	}
	if vehicle == nil {
		// Defensive: a repo that returns (nil, nil) violates the contract,
		// but we'd rather surface that as 404 than NPE downstream.
		return nil, fmt.Errorf("vehicle %s missing: %w", in.VehicleID, domain.ErrNotFound)
	}
	if vehicle.DriverID != driverID {
		// Authorisation gate. Even an authenticated driver cannot write
		// positions for a vehicle they don't own. Returns ErrForbidden so
		// the handler emits 403, not 404 — 404 here would leak the
		// existence of vehicles the caller does not own.
		return nil, fmt.Errorf(
			"driver %s does not own vehicle %s: %w",
			driverID, in.VehicleID, domain.ErrForbidden,
		)
	}

	// Persist. The repo populates ID and CreatedAt before returning.
	p := &domain.Position{
		VehicleID:  in.VehicleID,
		Lat:        in.Lat,
		Lng:        in.Lng,
		SpeedKmh:   in.SpeedKmh,
		RecordedAt: in.RecordedAt,
	}
	if err := u.positions.Insert(ctx, p); err != nil {
		return nil, fmt.Errorf("position write: insert: %w", err)
	}

	// Best-effort publish. The position is already durably written; a
	// publish failure must NOT fail the request. We log at warn level so
	// operators can spot a misconfigured DO publisher without flooding
	// the error stream.
	if pubErr := u.publisher.PublishPositionUpdate(ctx, p); pubErr != nil {
		log.Warn().
			Err(pubErr).
			Int64("position_id", p.ID).
			Str("vehicle_id", p.VehicleID).
			Msg("position published to D1 but DO publish failed; live clients will miss this update until refetch")
	}

	// Geofence transition detection — TASK-020. Best-effort: any error
	// in this block is logged at warn level and the request still
	// succeeds. The position is the source of truth; the alert is a
	// notification convenience.
	u.maybeEmitGeofenceAlert(ctx, p)

	return p, nil
}

// maybeEmitGeofenceAlert compares the just-inserted position against
// the vehicle's prior position (if any) and emits a geofence.alert
// event when the inside/outside state changed across the two readings.
//
// Best-effort: every error path is logged at warn level (or, for the
// no-fence / no-previous cases, silently skipped — those are normal
// states for most positions), and the function always returns void so
// the caller's request path is not influenced by this step.
//
// Logic:
//
//  1. If fences is nil (usecase wired without WithGeofences) → skip.
//  2. Look up the fence for this vehicle. ErrNotFound → most vehicles
//     don't have fences; skip silently.
//  3. Look up the immediate-predecessor position (id < current.ID
//     for the same vehicle). ErrNotFound → first position for this
//     vehicle; we have no transition to detect, skip silently.
//  4. Compute current.inside and previous.inside using
//     pkg/geo.IsInsideCircle.
//  5. If they differ, emit a geofence.alert with the new state's type:
//     previous-was-inside, current-is-outside → "exit".
//     previous-was-outside, current-is-inside → "enter".
//
// The `at` field of the event is the just-inserted position's
// recorded_at, not the server's now — operators care about when the
// transition happened in the field, not when the server processed it.
func (u *PositionUsecase) maybeEmitGeofenceAlert(ctx context.Context, current *domain.Position) {
	if u.fences == nil {
		return
	}

	fence, err := u.fences.GetByVehicle(ctx, current.VehicleID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// No fence configured — silent skip. This is the common case
			// for most vehicles and must not produce log noise.
			return
		}
		log.Warn().
			Err(err).
			Str("vehicle_id", current.VehicleID).
			Msg("geofence transition detection: fence lookup failed; skipping alert")
		return
	}

	previous, err := u.positions.GetMostRecentByVehicleBeforeID(ctx, current.VehicleID, current.ID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// First-ever position for this vehicle. No transition to
			// compare against — skip silently. This is the documented
			// semantic for the first reading.
			return
		}
		log.Warn().
			Err(err).
			Int64("position_id", current.ID).
			Str("vehicle_id", current.VehicleID).
			Msg("geofence transition detection: previous-position lookup failed; skipping alert")
		return
	}

	center := geo.Point{Lat: fence.CenterLat, Lng: fence.CenterLng}
	radius := float64(fence.RadiusM)
	prevInside := geo.IsInsideCircle(center, radius, geo.Point{Lat: previous.Lat, Lng: previous.Lng})
	currInside := geo.IsInsideCircle(center, radius, geo.Point{Lat: current.Lat, Lng: current.Lng})

	if prevInside == currInside {
		return // no transition
	}

	alertType := "exit"
	if currInside {
		alertType = "enter"
	}

	if pubErr := u.publisher.PublishGeofenceAlert(ctx, current.VehicleID, alertType, current.RecordedAt); pubErr != nil {
		log.Warn().
			Err(pubErr).
			Str("vehicle_id", current.VehicleID).
			Str("alert_type", alertType).
			Int64("recorded_at", current.RecordedAt).
			Msg("geofence alert publish failed; live clients will miss this transition")
	}
}
