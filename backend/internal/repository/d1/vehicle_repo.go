package d1

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
)

// VehicleRepo is the D1-backed implementation of the vehicle storage
// interface declared at the consumer site (internal/usecase). Like
// DriverRepo it depends only on the narrow Executor contract so the
// sqlite3 test double satisfies it just as well as the production
// pkg/cfclient.D1Client does.
type VehicleRepo struct {
	exec Executor
}

// NewVehicleRepo constructs a repository bound to exec. exec is captured
// verbatim; callers are responsible for its lifecycle and concurrency
// guarantees (both real implementations are safe for shared use).
func NewVehicleRepo(exec Executor) *VehicleRepo {
	return &VehicleRepo{exec: exec}
}

// vehicleColumnList is the canonical SELECT projection for the vehicles
// table, ordered alphabetically. The D1 client's row scanner alphabetises
// columns (see pkg/cfclient.collectColumnOrder); aligning our SELECT list
// with that order means scanVehicle works against both the production
// backend and the sqlite test double without divergence.
const vehicleColumnList = "created_at, driver_id, id, model, plate_number, updated_at"

// scanVehicle reads a Row in the column order documented by
// vehicleColumnList. driver_id and model are SQL-nullable; the
// repository translates SQL NULL into the empty-string convention the
// domain layer uses.
func scanVehicle(row Row, v *domain.Vehicle) error {
	// We use *string for the nullable columns so Scan can distinguish NULL
	// from the empty string at the driver/D1 boundary, then collapse NULL
	// to "" on return so the domain stays free of pointer nullability.
	var (
		driverID, model *string
	)
	if err := row.Scan(
		&v.CreatedAt,
		&driverID,
		&v.ID,
		&model,
		&v.PlateNumber,
		&v.UpdatedAt,
	); err != nil {
		return err
	}
	if driverID != nil {
		v.DriverID = *driverID
	}
	if model != nil {
		v.Model = *model
	}
	return nil
}

// scanVehicleRows reads a multi-row iterator using the same column layout
// as scanVehicle. Pulled out so List can reuse the binding logic without
// re-introducing the *string null shim at each call site.
func scanVehicleRows(rows Rows, v *domain.Vehicle) error {
	var (
		driverID, model *string
	)
	if err := rows.Scan(
		&v.CreatedAt,
		&driverID,
		&v.ID,
		&model,
		&v.PlateNumber,
		&v.UpdatedAt,
	); err != nil {
		return err
	}
	if driverID != nil {
		v.DriverID = *driverID
	}
	if model != nil {
		v.Model = *model
	}
	return nil
}

// nullableString turns the empty-string "unset" convention back into a
// SQL NULL on write. Both nullable columns (driver_id, model) use it.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Create inserts a new vehicle row. The caller has already generated the
// ID and stamped timestamps. A plate_number collision surfaces as
// domain.ErrAlreadyExists so the usecase can map it onto HTTP 409 without
// importing storage-specific symbols.
func (r *VehicleRepo) Create(ctx context.Context, v *domain.Vehicle) error {
	if r == nil || r.exec == nil {
		return errors.New("vehicle repo: nil executor")
	}
	if v == nil {
		return errors.New("vehicle repo: nil vehicle")
	}

	err := r.exec.Exec(ctx,
		`INSERT INTO vehicles (id, plate_number, model, driver_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		v.ID,
		v.PlateNumber,
		nullableString(v.Model),
		nullableString(v.DriverID),
		v.CreatedAt,
		v.UpdatedAt,
	)
	if err != nil {
		if isVehiclePlateUnique(err) {
			return fmt.Errorf("vehicle plate %s: %w", v.PlateNumber, domain.ErrAlreadyExists)
		}
		return fmt.Errorf("vehicle repo: insert: %w", err)
	}
	return nil
}

// Get returns the vehicle row with the given UUID, or domain.ErrNotFound.
func (r *VehicleRepo) Get(ctx context.Context, id string) (*domain.Vehicle, error) {
	if r == nil || r.exec == nil {
		return nil, errors.New("vehicle repo: nil executor")
	}
	row := r.exec.QueryRow(ctx,
		`SELECT `+vehicleColumnList+` FROM vehicles WHERE id = ?`,
		id,
	)
	var v domain.Vehicle
	if err := scanVehicle(row, &v); err != nil {
		if isNoRowsErr(err) {
			return nil, fmt.Errorf("vehicle id %s: %w", id, domain.ErrNotFound)
		}
		return nil, fmt.Errorf("vehicle repo: get: %w", err)
	}
	return &v, nil
}

// List returns every vehicle ordered by created_at DESC (newest first).
// For the small demo dataset this is fine; production deployments would
// add cursor pagination as a follow-up.
//
// The Rows iterator is closed on every exit path — including the deferred
// error path — so a partial scan does not leak the underlying connection.
func (r *VehicleRepo) List(ctx context.Context) ([]*domain.Vehicle, error) {
	if r == nil || r.exec == nil {
		return nil, errors.New("vehicle repo: nil executor")
	}
	rows, err := r.exec.Query(ctx,
		`SELECT `+vehicleColumnList+` FROM vehicles ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("vehicle repo: list query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*domain.Vehicle
	for rows.Next() {
		var v domain.Vehicle
		if scanErr := scanVehicleRows(rows, &v); scanErr != nil {
			return nil, fmt.Errorf("vehicle repo: list scan: %w", scanErr)
		}
		out = append(out, &v)
	}
	if iterErr := rows.Err(); iterErr != nil {
		return nil, fmt.Errorf("vehicle repo: list iterate: %w", iterErr)
	}
	if out == nil {
		// Return a non-nil empty slice so callers can safely len/range
		// without a special-case nil check.
		out = []*domain.Vehicle{}
	}
	return out, nil
}

// Update sets the modifiable fields (plate_number, model, driver_id,
// updated_at). Returns domain.ErrNotFound if the row does not exist and
// domain.ErrAlreadyExists if plate_number collides with another row.
//
// SQLite/D1 does not report "rows affected" through our narrow Executor
// interface, so we run an explicit existence probe via QueryRow before
// attempting the UPDATE. The two-query pattern is acceptable for a demo
// (single-row probes are cheap on D1); a future iteration could add an
// affected-rows accessor to Executor if the round-trip becomes a hotspot.
func (r *VehicleRepo) Update(ctx context.Context, v *domain.Vehicle) error {
	if r == nil || r.exec == nil {
		return errors.New("vehicle repo: nil executor")
	}
	if v == nil {
		return errors.New("vehicle repo: nil vehicle")
	}

	// Existence probe: a missing row must surface as ErrNotFound, not
	// silently succeed with zero rows updated.
	row := r.exec.QueryRow(ctx,
		`SELECT id FROM vehicles WHERE id = ?`,
		v.ID,
	)
	var foundID string
	if err := row.Scan(&foundID); err != nil {
		if isNoRowsErr(err) {
			return fmt.Errorf("vehicle id %s: %w", v.ID, domain.ErrNotFound)
		}
		return fmt.Errorf("vehicle repo: update probe: %w", err)
	}

	err := r.exec.Exec(ctx,
		`UPDATE vehicles
		   SET plate_number = ?, model = ?, driver_id = ?, updated_at = ?
		 WHERE id = ?`,
		v.PlateNumber,
		nullableString(v.Model),
		nullableString(v.DriverID),
		v.UpdatedAt,
		v.ID,
	)
	if err != nil {
		if isVehiclePlateUnique(err) {
			return fmt.Errorf("vehicle plate %s: %w", v.PlateNumber, domain.ErrAlreadyExists)
		}
		return fmt.Errorf("vehicle repo: update: %w", err)
	}
	return nil
}

// Delete removes the vehicle with the given ID. Returns domain.ErrNotFound
// if the row does not exist. Like Update, Delete uses an existence probe
// because the narrow Executor interface does not expose affected-rows.
//
// The D1 schema declares ON DELETE CASCADE for positions and geofences
// referencing this vehicle, so children rows are cleaned up by the
// database without an explicit second statement.
func (r *VehicleRepo) Delete(ctx context.Context, id string) error {
	if r == nil || r.exec == nil {
		return errors.New("vehicle repo: nil executor")
	}

	row := r.exec.QueryRow(ctx,
		`SELECT id FROM vehicles WHERE id = ?`,
		id,
	)
	var foundID string
	if err := row.Scan(&foundID); err != nil {
		if isNoRowsErr(err) {
			return fmt.Errorf("vehicle id %s: %w", id, domain.ErrNotFound)
		}
		return fmt.Errorf("vehicle repo: delete probe: %w", err)
	}

	if err := r.exec.Exec(ctx, `DELETE FROM vehicles WHERE id = ?`, id); err != nil {
		return fmt.Errorf("vehicle repo: delete: %w", err)
	}
	return nil
}

// isVehiclePlateUnique reports whether err looks like a SQLite UNIQUE
// constraint failure on the vehicles.plate_number column. The string
// match mirrors driver_repo.isUniqueViolation — both modernc.org/sqlite
// (tests) and D1 (production) emit the canonical
//
//	UNIQUE constraint failed: vehicles.plate_number
//
// message today. If that ever changes the matching unit test will surface
// the drift loudly and the substring set below is the only place to update.
func isVehiclePlateUnique(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "UNIQUE constraint failed: vehicles.plate_number") {
		return true
	}
	if strings.Contains(msg, "UNIQUE constraint failed") && strings.Contains(msg, "plate_number") {
		return true
	}
	return false
}
