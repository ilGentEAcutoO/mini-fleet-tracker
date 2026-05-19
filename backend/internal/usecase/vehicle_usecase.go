package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
)

// VehicleRepo is the subset of the vehicle storage interface VehicleUsecase
// requires. Declared at the consumer site so the usecase owns its contract;
// any conforming repository can plug in (the production
// internal/repository/d1.VehicleRepo satisfies it directly).
type VehicleRepo interface {
	List(ctx context.Context) ([]*domain.Vehicle, error)
	Get(ctx context.Context, id string) (*domain.Vehicle, error)
	Create(ctx context.Context, v *domain.Vehicle) error
	Update(ctx context.Context, v *domain.Vehicle) error
	Delete(ctx context.Context, id string) error
}

// PositionLister is the narrow contract VehicleUsecase needs from the
// position storage layer to serve the history endpoint. Declared at the
// consumer site so the usecase keeps a minimal coupling — the production
// d1.PositionRepo satisfies this implicitly via its ListByVehicleAndRange
// method (added in TASK-011 prep, wired in TASK-018).
//
// Implementations must return a non-nil (possibly empty) slice on success.
// The DESC-by-recorded_at ordering and the "0 == no bound" semantics are
// the contract — see d1.PositionRepo.ListByVehicleAndRange for the
// reference implementation.
type PositionLister interface {
	ListByVehicleAndRange(ctx context.Context, vehicleID string, fromMs, toMs int64, limit int) ([]*domain.Position, error)
}

// VehicleUsecase wires the manager-only vehicle workflows. Dependencies are
// immutable after construction; the struct is safe for concurrent use as
// long as the injected adapters are.
type VehicleUsecase struct {
	repo      VehicleRepo
	positions PositionLister
	ids       IDGenerator
	// now is the testable clock. Production code leaves it nil so the real
	// time.Now is used; tests pin it to a fixed instant so timestamp
	// assertions are deterministic.
	now func() time.Time
}

// NewVehicleUsecase constructs a usecase from its dependencies. Every
// argument is required; passing any nil is a programmer error and returns
// an error rather than panicking on the first request.
//
// positions is the read-only position store used by ListPositions; the
// rest of VehicleUsecase does not touch it, so a dedicated history-only
// usecase would be a fair alternative. Keeping it on VehicleUsecase
// matches the resource modelling (positions are *of a vehicle*) and
// avoids growing a second wiring entry in bootstrap.go for one method.
func NewVehicleUsecase(repo VehicleRepo, positions PositionLister, ids IDGenerator) (*VehicleUsecase, error) {
	if repo == nil {
		return nil, errors.New("vehicle usecase: repo is required")
	}
	if positions == nil {
		return nil, errors.New("vehicle usecase: position lister is required")
	}
	if ids == nil {
		return nil, errors.New("vehicle usecase: id generator is required")
	}
	return &VehicleUsecase{repo: repo, positions: positions, ids: ids}, nil
}

// nowFunc returns the test-overridable clock or time.Now in production.
func (u *VehicleUsecase) nowFunc() time.Time {
	if u.now != nil {
		return u.now()
	}
	return time.Now()
}

// Field bounds — kept conservative so a malformed client payload cannot
// blow out a column or an ad-hoc index. Plate numbers in the wild (US,
// EU, Thailand etc.) all fit comfortably under 50; model strings rarely
// exceed 60 — 100 leaves headroom for trim levels without inviting abuse.
const (
	maxPlateLen = 50
	maxModelLen = 100
)

// History endpoint bounds. defaultHistoryLimit applies when the caller does
// not supply a `limit` query param — 1000 points renders smoothly as a
// Google Maps Polyline (the SDK simplifies above ~2k vertices anyway) and
// covers a full work shift at one-per-minute sampling.
//
// maxHistoryLimit is the hard cap — bigger windows produce JSON bodies
// that take seconds to render on slow networks and don't materially
// improve the visualization. The handler clamps silently rather than
// rejecting, so a "limit=99999" client request degrades to "limit=5000"
// with no surprise 400.
const (
	defaultHistoryLimit = 1000
	maxHistoryLimit     = 5000
)

// CreateVehicleInput is the application-level input for Create. PlateNumber
// is required; Model and DriverID are optional and pass an empty string
// when unset.
type CreateVehicleInput struct {
	PlateNumber string
	Model       string
	DriverID    string
}

// UpdateVehicleInput is the application-level input for Update. Pointer
// fields encode "intent to change" — a nil field is preserved as-is, a
// non-nil field overwrites the stored value. This matches PATCH semantics
// at the HTTP layer.
type UpdateVehicleInput struct {
	PlateNumber *string
	Model       *string
	DriverID    *string
}

// List proxies to the repo. The usecase exists for symmetry with the
// future filter/pagination work in TASK-018+; today it does no extra
// work beyond delegation.
func (u *VehicleUsecase) List(ctx context.Context) ([]*domain.Vehicle, error) {
	if u == nil {
		return nil, errors.New("vehicle usecase: nil receiver")
	}
	return u.repo.List(ctx)
}

// Get returns the vehicle with the given ID. Repo errors surface verbatim;
// in particular domain.ErrNotFound passes through so the handler can map
// it onto a 404.
func (u *VehicleUsecase) Get(ctx context.Context, id string) (*domain.Vehicle, error) {
	if u == nil {
		return nil, errors.New("vehicle usecase: nil receiver")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("id is required: %w", domain.ErrValidation)
	}
	return u.repo.Get(ctx, id)
}

// ListPositions returns up to `limit` positions for the given vehicle in
// the range [fromMs, toMs] (each bound is "no bound" when zero), ordered
// DESC by recorded_at. Used by the TASK-018 history endpoint.
//
// Validation:
//   - id required (trimmed)
//   - when both fromMs and toMs are non-zero, fromMs <= toMs
//   - limit < 0 → ErrValidation
//   - limit == 0 → defaultHistoryLimit
//   - limit > maxHistoryLimit → clamped to maxHistoryLimit (silent, no
//     error — the handler doc-comments the cap so clients aren't
//     surprised, but a "give me everything" request stays useful instead
//     of failing)
//
// Vehicle existence is checked first (via repo.Get) so a missing ID
// returns domain.ErrNotFound rather than a confusing empty list. The
// handler maps ErrNotFound to 404 — see vehicle_handler.History.
func (u *VehicleUsecase) ListPositions(
	ctx context.Context,
	id string,
	fromMs, toMs int64,
	limit int,
) ([]*domain.Position, error) {
	if u == nil {
		return nil, errors.New("vehicle usecase: nil receiver")
	}

	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("id is required: %w", domain.ErrValidation)
	}

	if fromMs < 0 {
		return nil, fmt.Errorf("from must be non-negative: %w", domain.ErrValidation)
	}
	if toMs < 0 {
		return nil, fmt.Errorf("to must be non-negative: %w", domain.ErrValidation)
	}
	if fromMs > 0 && toMs > 0 && fromMs > toMs {
		return nil, fmt.Errorf("from must be <= to: %w", domain.ErrValidation)
	}

	if limit < 0 {
		return nil, fmt.Errorf("limit must be non-negative: %w", domain.ErrValidation)
	}
	if limit == 0 {
		limit = defaultHistoryLimit
	}
	if limit > maxHistoryLimit {
		limit = maxHistoryLimit
	}

	// Existence probe — propagates ErrNotFound verbatim. A missing vehicle
	// returning 200-with-empty-array is a worse UX (clients can't tell
	// "no data in window" from "bad id"); the explicit 404 is unambiguous.
	if _, err := u.repo.Get(ctx, id); err != nil {
		return nil, err
	}

	return u.positions.ListByVehicleAndRange(ctx, id, fromMs, toMs, limit)
}

// Create validates the input, generates a fresh ID, stamps both timestamps,
// and persists. The returned *domain.Vehicle is the just-written entity —
// callers do not need to issue a follow-up Get.
//
// Validation rules:
//   - PlateNumber: required, max 50 chars, trimmed
//   - Model: optional, max 100 chars
//   - DriverID: optional; when non-empty must look like a UUID
//
// All validation failures wrap domain.ErrValidation so the handler layer
// maps them onto 400. Repo-level errors (ErrAlreadyExists, etc.) pass
// through verbatim.
func (u *VehicleUsecase) Create(ctx context.Context, in CreateVehicleInput) (*domain.Vehicle, error) {
	if u == nil {
		return nil, errors.New("vehicle usecase: nil receiver")
	}

	plate := strings.TrimSpace(in.PlateNumber)
	model := strings.TrimSpace(in.Model)
	driverID := strings.TrimSpace(in.DriverID)

	if plate == "" {
		return nil, fmt.Errorf("plate_number is required: %w", domain.ErrValidation)
	}
	if len(plate) > maxPlateLen {
		return nil, fmt.Errorf("plate_number too long (max %d): %w", maxPlateLen, domain.ErrValidation)
	}
	if len(model) > maxModelLen {
		return nil, fmt.Errorf("model too long (max %d): %w", maxModelLen, domain.ErrValidation)
	}
	if driverID != "" {
		if _, err := uuid.Parse(driverID); err != nil {
			return nil, fmt.Errorf("driver_id is not a valid UUID: %w", domain.ErrValidation)
		}
	}

	now := u.nowFunc().UnixMilli()
	v := &domain.Vehicle{
		ID:          u.ids.NewID(),
		PlateNumber: plate,
		Model:       model,
		DriverID:    driverID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := u.repo.Create(ctx, v); err != nil {
		return nil, err
	}
	return v, nil
}

// Update applies the patch in in to the row identified by id. Only the
// fields whose pointer is non-nil are touched; nil fields preserve the
// stored value. UpdatedAt is always bumped to the usecase's current
// clock so out-of-band tooling can spot a touched row.
//
// Returns the post-update entity so handlers can echo it in the response
// without a follow-up Get round-trip.
func (u *VehicleUsecase) Update(ctx context.Context, id string, in UpdateVehicleInput) (*domain.Vehicle, error) {
	if u == nil {
		return nil, errors.New("vehicle usecase: nil receiver")
	}

	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("id is required: %w", domain.ErrValidation)
	}

	// Load current state so we can apply the partial patch onto a real row.
	// The repo.Update existence probe would catch a missing ID too, but
	// loading first keeps the validation/error mapping uniform — the
	// handler sees the same domain.ErrNotFound regardless of which leg of
	// the call detected the absence.
	current, err := u.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	// Apply the patch. Each field has its own validation envelope so the
	// error message points at the offending key, not at the whole struct.
	if in.PlateNumber != nil {
		plate := strings.TrimSpace(*in.PlateNumber)
		if plate == "" {
			return nil, fmt.Errorf("plate_number cannot be empty: %w", domain.ErrValidation)
		}
		if len(plate) > maxPlateLen {
			return nil, fmt.Errorf("plate_number too long (max %d): %w", maxPlateLen, domain.ErrValidation)
		}
		current.PlateNumber = plate
	}
	if in.Model != nil {
		model := strings.TrimSpace(*in.Model)
		if len(model) > maxModelLen {
			return nil, fmt.Errorf("model too long (max %d): %w", maxModelLen, domain.ErrValidation)
		}
		current.Model = model
	}
	if in.DriverID != nil {
		driverID := strings.TrimSpace(*in.DriverID)
		if driverID != "" {
			if _, parseErr := uuid.Parse(driverID); parseErr != nil {
				return nil, fmt.Errorf("driver_id is not a valid UUID: %w", domain.ErrValidation)
			}
		}
		current.DriverID = driverID
	}
	current.UpdatedAt = u.nowFunc().UnixMilli()

	if err := u.repo.Update(ctx, current); err != nil {
		return nil, err
	}
	return current, nil
}

// Delete removes the vehicle with the given ID. Repo errors surface
// verbatim; in particular domain.ErrNotFound passes through so the
// handler can map it onto a 404.
func (u *VehicleUsecase) Delete(ctx context.Context, id string) error {
	if u == nil {
		return errors.New("vehicle usecase: nil receiver")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("id is required: %w", domain.ErrValidation)
	}
	return u.repo.Delete(ctx, id)
}
