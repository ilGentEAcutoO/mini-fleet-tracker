package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
)

// geofenceRepo is the narrow contract GeofenceUsecase needs from the
// geofence storage layer. Declared at the consumer site so the usecase
// owns its dependency surface; the production
// internal/repository/d1.GeofenceRepo satisfies this directly.
type geofenceRepo interface {
	GetByVehicle(ctx context.Context, vehicleID string) (*domain.Geofence, error)
	Upsert(ctx context.Context, g *domain.Geofence) error
	Delete(ctx context.Context, vehicleID string) error
}

// vehicleExistenceLookup is the narrow contract GeofenceUsecase needs to
// verify that the vehicle a fence is being set for actually exists.
// Same shape as positionUsecase's vehicleLookup interface but declared
// separately so each usecase owns its own dependency surface — Go's
// structural typing means the d1.VehicleRepo concrete type satisfies
// both interfaces without any explicit coupling between the two.
type vehicleExistenceLookup interface {
	Get(ctx context.Context, id string) (*domain.Vehicle, error)
}

// Geofence radius bounds. The lower bound (50 m) is a "smaller than this
// is GPS noise, not a real fence" guard — consumer-grade GPS regularly
// reports points 20-30 m off the true location, so a smaller radius
// would trip enter/exit transitions on every reading even from a
// stationary vehicle. The upper bound (50 km) is a "this is a fence,
// not a region" guard — anything bigger than a city is almost certainly
// a misconfigured client and would dilute the alert signal.
const (
	geofenceMinRadiusM = 50
	geofenceMaxRadiusM = 50_000
)

// GeofenceUsecase wires the manager-only geofence workflow. Dependencies
// are immutable after construction; the struct is safe for concurrent
// use as long as the injected adapters are.
type GeofenceUsecase struct {
	fences   geofenceRepo
	vehicles vehicleExistenceLookup
	ids      IDGenerator
	// now is the testable clock. Production code leaves it nil so the
	// real time.Now is used; tests pin it to a fixed instant so
	// timestamp assertions are deterministic.
	now func() time.Time
}

// NewGeofenceUsecase constructs a usecase from its dependencies. Every
// argument is required; passing any nil returns an error rather than
// panicking on the first request, mirroring the other usecase
// constructors in this package.
func NewGeofenceUsecase(
	fences geofenceRepo,
	vehicles vehicleExistenceLookup,
	ids IDGenerator,
) (*GeofenceUsecase, error) {
	if fences == nil {
		return nil, errors.New("geofence usecase: fences repo is required")
	}
	if vehicles == nil {
		return nil, errors.New("geofence usecase: vehicles lookup is required")
	}
	if ids == nil {
		return nil, errors.New("geofence usecase: id generator is required")
	}
	return &GeofenceUsecase{fences: fences, vehicles: vehicles, ids: ids}, nil
}

// nowFunc returns the test-overridable clock or time.Now in production.
func (u *GeofenceUsecase) nowFunc() time.Time {
	if u.now != nil {
		return u.now()
	}
	return time.Now()
}

// GetByVehicle returns the fence for the given vehicle, or
// domain.ErrNotFound when no fence has been set. The vehicle's
// existence is NOT checked here — a missing fence on an existing
// vehicle and a missing fence on a missing vehicle both return
// ErrNotFound, which the handler maps onto 404. If the distinction
// matters to a caller they should look the vehicle up separately.
func (u *GeofenceUsecase) GetByVehicle(ctx context.Context, vehicleID string) (*domain.Geofence, error) {
	if u == nil {
		return nil, errors.New("geofence usecase: nil receiver")
	}
	vehicleID = strings.TrimSpace(vehicleID)
	if vehicleID == "" {
		return nil, fmt.Errorf("vehicle_id is required: %w", domain.ErrValidation)
	}
	return u.fences.GetByVehicle(ctx, vehicleID)
}

// SetGeofenceInput is the application-level input for Set. All four
// fields are required and validated against the documented bounds.
type SetGeofenceInput struct {
	VehicleID string
	CenterLat float64
	CenterLng float64
	RadiusM   int
}

// Set validates the input, verifies the vehicle exists, generates a
// fresh fence id and stamps the timestamp, and persists via the repo's
// Upsert (DELETE-then-INSERT, idempotent — see GeofenceRepo.Upsert).
//
// Validation rules (all failures wrap domain.ErrValidation):
//
//   - VehicleID: required, trimmed, non-empty
//   - CenterLat: ∈ [-90, 90]
//   - CenterLng: ∈ [-180, 180]
//   - RadiusM:   ∈ [50, 50_000]  (50 m..50 km — see constants above)
//
// Vehicle existence is checked AFTER the field-validation ladder so a
// malformed payload is rejected without burning a D1 round-trip on the
// existence probe. Missing-vehicle surfaces as ErrNotFound from the
// vehicles lookup; the handler maps that onto 404.
//
// The repo's Upsert replaces any existing fence wholesale — there is
// no "patch" semantics for geofences. Setting a new fence implicitly
// clears the old one.
func (u *GeofenceUsecase) Set(ctx context.Context, in SetGeofenceInput) (*domain.Geofence, error) {
	if u == nil {
		return nil, errors.New("geofence usecase: nil receiver")
	}

	vehicleID := strings.TrimSpace(in.VehicleID)
	if vehicleID == "" {
		return nil, fmt.Errorf("vehicle_id is required: %w", domain.ErrValidation)
	}
	if in.CenterLat < -90 || in.CenterLat > 90 {
		return nil, fmt.Errorf("center_lat %f out of range [-90, 90]: %w",
			in.CenterLat, domain.ErrValidation)
	}
	if in.CenterLng < -180 || in.CenterLng > 180 {
		return nil, fmt.Errorf("center_lng %f out of range [-180, 180]: %w",
			in.CenterLng, domain.ErrValidation)
	}
	if in.RadiusM < geofenceMinRadiusM || in.RadiusM > geofenceMaxRadiusM {
		return nil, fmt.Errorf("radius_m %d out of range [%d, %d]: %w",
			in.RadiusM, geofenceMinRadiusM, geofenceMaxRadiusM, domain.ErrValidation)
	}

	// Vehicle-must-exist check. ErrNotFound from the lookup is propagated
	// verbatim; the handler maps it to 404. Any other lookup error wraps
	// into a generic "lookup failed".
	v, err := u.vehicles.Get(ctx, vehicleID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("geofence set: vehicle lookup: %w", err)
	}
	if v == nil {
		// Defensive: a misbehaving lookup that returns (nil, nil) would
		// otherwise NPE further down. Surface as ErrNotFound so the
		// handler reports the right thing.
		return nil, fmt.Errorf("vehicle %s missing: %w", vehicleID, domain.ErrNotFound)
	}

	g := &domain.Geofence{
		ID:        u.ids.NewID(),
		VehicleID: vehicleID,
		CenterLat: in.CenterLat,
		CenterLng: in.CenterLng,
		RadiusM:   in.RadiusM,
		CreatedAt: u.nowFunc().UnixMilli(),
	}
	if err := u.fences.Upsert(ctx, g); err != nil {
		return nil, fmt.Errorf("geofence set: upsert: %w", err)
	}
	return g, nil
}
