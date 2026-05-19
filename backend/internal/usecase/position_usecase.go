package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
)

// positionRepo is the narrow contract PositionUsecase needs from the
// position storage layer. Declared at the consumer site so the usecase
// owns its dependency and tests can mock with minimal surface area.
//
// The repo populates p.ID (DB auto-increment) and p.CreatedAt
// (server-stamped) before returning; the caller's other fields are
// passed through unchanged.
type positionRepo interface {
	Insert(ctx context.Context, p *domain.Position) error
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

// EventPublisher is the seam where TASK-014 will plug in the Durable
// Object publisher (POST to gateway /internal/publish with HMAC-signed
// body). For TASK-011 the production wiring uses NoopPublisher; tests
// inject a spy to verify the call shape would be correct when TASK-014
// wires the real publisher.
//
// The contract is intentionally narrow — one method, one input — so the
// real implementation (built in TASK-014) can be swapped in without
// touching the usecase.
type EventPublisher interface {
	PublishPositionUpdate(ctx context.Context, p *domain.Position) error
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
type PositionUsecase struct {
	positions positionRepo
	vehicles  vehicleLookup
	publisher EventPublisher
	// now is the testable clock used for freshness validation.
	// Production leaves it nil and time.Now is used.
	now func() time.Time
}

// NewPositionUsecase constructs a usecase from its dependencies. All
// four arguments are required; passing any nil returns an error rather
// than panicking later in the request path.
//
// publisher may be NoopPublisher{} in the TASK-011 wiring — see the
// EventPublisher doc comment. It cannot be a literal nil because the
// usecase calls PublishPositionUpdate unconditionally (with error
// logging on failure); a nil interface would NPE.
func NewPositionUsecase(
	positions positionRepo,
	vehicles vehicleLookup,
	publisher EventPublisher,
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
	return &PositionUsecase{
		positions: positions,
		vehicles:  vehicles,
		publisher: publisher,
	}, nil
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

	return p, nil
}
