package d1

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
)

// PositionRepo is the D1-backed implementation of the position storage
// interface declared at the consumer site (internal/usecase). Depends only
// on the narrow Executor contract so the in-memory sqlite test double
// satisfies it just as well as the production pkg/cfclient.D1Client.
type PositionRepo struct {
	exec Executor
	// now is the testable clock used to stamp CreatedAt. Production
	// leaves it nil and time.Now is used; tests inject a deterministic
	// clock so CreatedAt is reproducible.
	now func() time.Time
}

// NewPositionRepo constructs a repository bound to exec. Pass a non-nil
// Executor; passing nil produces a repo whose methods return errors
// instead of panicking, mirroring the DriverRepo defensive-construction
// pattern.
func NewPositionRepo(exec Executor) *PositionRepo {
	return &PositionRepo{exec: exec}
}

// nowFunc returns the test-overridable clock or time.Now in production.
// Mirrors the AuthUsecase nowFunc helper for consistency across the
// codebase — tests that need a fixed clock can set repo.now directly
// from inside the package (package-private setter is intentional; this
// is only exercised by repo-internal tests).
func (r *PositionRepo) nowFunc() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// Insert persists a new position row. The DB assigns the primary key
// (INTEGER AUTOINCREMENT); CreatedAt is stamped by the repository at
// insert time. On return, p.ID and p.CreatedAt are populated; the
// caller's other fields (VehicleID, Lat, Lng, SpeedKmh, RecordedAt)
// are passed through unchanged.
//
// We use the SQLite RETURNING clause to read the assigned id back in
// the same round-trip. RETURNING has been supported since SQLite 3.35
// (March 2021) and is supported by D1's libsqlite. The alternative —
// reading meta.last_row_id from the D1 envelope — would require
// extending the Executor interface with metadata access, which the
// usecase layer doesn't need; RETURNING keeps the interface narrow.
//
// Errors propagate from the underlying executor verbatim — the
// position-write workflow has no domain-level uniqueness invariants to
// translate (the (vehicle_id, recorded_at) pair is *expected* to
// collide on the rare millisecond-granularity rate-limit edge case;
// FK violations on vehicle_id surface as a generic insert error which
// the usecase upgrades to ErrNotFound after its own pre-check).
func (r *PositionRepo) Insert(ctx context.Context, p *domain.Position) error {
	if r == nil || r.exec == nil {
		return errors.New("position repo: nil executor")
	}
	if p == nil {
		return errors.New("position repo: nil position")
	}

	createdAt := r.nowFunc().UnixMilli()

	// Read the assigned id back via RETURNING. Using QueryRow + scan
	// keeps the Executor interface narrow (no need for last_row_id
	// metadata).
	row := r.exec.QueryRow(ctx,
		`INSERT INTO positions (vehicle_id, lat, lng, speed_kmh, recorded_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 RETURNING id`,
		p.VehicleID, p.Lat, p.Lng, nullableSpeed(p.SpeedKmh), p.RecordedAt, createdAt,
	)
	var id int64
	if err := row.Scan(&id); err != nil {
		return fmt.Errorf("position repo: insert: %w", err)
	}
	p.ID = id
	p.CreatedAt = createdAt
	return nil
}

// nullableSpeed maps the Go zero value to a SQL NULL so the
// positions.speed_kmh column stays semantically "unset" when the driver
// did not report a speed. The Executor's argument binding passes
// untyped nil through to SQLite/D1 as NULL — both the in-memory
// modernc.org/sqlite test double and the D1 HTTP backend handle this.
func nullableSpeed(speed float64) any {
	if speed == 0 {
		return nil
	}
	return speed
}

// positionColumnList is the canonical SELECT projection for the
// positions table, ordered to match the alphabetical column order
// produced by the D1 client's Scan (the in-memory sqlite test
// executor preserves declared order, but our production D1 client
// alphabetises — aligning here means one Scan binding works for
// both backends, same trick driver_repo and vehicle_repo use).
const positionColumnList = "created_at, id, lat, lng, recorded_at, speed_kmh, vehicle_id"

// scanPositionRows reads a single row of a multi-row iterator into a
// domain.Position. The column order matches positionColumnList exactly.
// speed_kmh is SQL-nullable; the helper translates a NULL back to the
// Go zero value 0.0 (the "unset" sentinel per domain.Position).
func scanPositionRows(rows Rows, p *domain.Position) error {
	var speed any // captured as nil for SQL NULL, float64 otherwise
	if err := rows.Scan(
		&p.CreatedAt,
		&p.ID,
		&p.Lat,
		&p.Lng,
		&p.RecordedAt,
		&speed,
		&p.VehicleID,
	); err != nil {
		return err
	}
	switch v := speed.(type) {
	case nil:
		p.SpeedKmh = 0
	case float64:
		p.SpeedKmh = v
	case int64:
		// modernc/sqlite occasionally surfaces REAL as INTEGER when
		// the stored value is a whole number; cope rather than fail.
		p.SpeedKmh = float64(v)
	default:
		return fmt.Errorf("unexpected speed_kmh dynamic type %T", v)
	}
	return nil
}

// GetMostRecentByVehicleBeforeID returns the most-recent position for
// the given vehicle whose id is strictly less than excludeID. Returns
// domain.ErrNotFound when no such row exists (i.e. excludeID was the
// vehicle's first ever position, or the vehicle has no positions).
//
// Used by the geofence transition-detection step in PositionUsecase:
// after inserting a new position we ask "what was the prior position
// for this vehicle?" to decide whether the inside/outside state
// changed across the two readings. We use id (an auto-increment) as
// the ordering key rather than recorded_at because:
//
//   - id is monotonic by insert order, so "previous" is unambiguous
//     even when two readings share a recorded_at millisecond
//   - the usecase always calls this with the id it just got back from
//     Insert, so we have the exclusive upper bound right there
//
// Returns at most one row. The DESC ordering + LIMIT 1 picks the
// immediate predecessor regardless of how many older rows exist.
func (r *PositionRepo) GetMostRecentByVehicleBeforeID(
	ctx context.Context,
	vehicleID string,
	excludeID int64,
) (*domain.Position, error) {
	if r == nil || r.exec == nil {
		return nil, errors.New("position repo: nil executor")
	}
	rows, err := r.exec.Query(ctx,
		`SELECT `+positionColumnList+` FROM positions
		 WHERE vehicle_id = ? AND id < ?
		 ORDER BY id DESC
		 LIMIT 1`,
		vehicleID, excludeID,
	)
	if err != nil {
		return nil, fmt.Errorf("position repo: get-previous query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		if iterErr := rows.Err(); iterErr != nil {
			return nil, fmt.Errorf("position repo: get-previous iterate: %w", iterErr)
		}
		return nil, fmt.Errorf("previous position for vehicle %s: %w", vehicleID, domain.ErrNotFound)
	}
	var p domain.Position
	if scanErr := scanPositionRows(rows, &p); scanErr != nil {
		return nil, fmt.Errorf("position repo: get-previous scan: %w", scanErr)
	}
	return &p, nil
}

// ListByVehicleAndRange returns positions for vehicleID whose recorded_at
// is in the inclusive range [fromMs, toMs], ordered DESC by recorded_at,
// limited to `limit` rows.
//
// Range semantics:
//   - fromMs == 0 means "no lower bound" (positions back to the start
//     of time)
//   - toMs == 0 means "no upper bound" (positions up to now)
//   - both can be 0 to fetch the most recent `limit` positions
//
// Limit semantics:
//   - limit <= 0 is treated as "no row cap"; the caller is trusted to
//     bound the result at the HTTP layer (TASK-018 enforces a hard cap)
//
// Defined here for TASK-018's vehicle-history endpoint. TASK-011 only
// uses Insert; production wiring of this method comes with TASK-018.
func (r *PositionRepo) ListByVehicleAndRange(
	ctx context.Context,
	vehicleID string,
	fromMs, toMs int64,
	limit int,
) ([]*domain.Position, error) {
	if r == nil || r.exec == nil {
		return nil, errors.New("position repo: nil executor")
	}

	// Build the WHERE clause incrementally so we only bind the
	// args that actually matter. A NULL-aware SQL approach (`AND
	// (?1 = 0 OR recorded_at >= ?1)`) would also work but is harder
	// to read and the parameter count would still vary by call.
	sqlText := `SELECT ` + positionColumnList + ` FROM positions WHERE vehicle_id = ?`
	args := []any{vehicleID}
	if fromMs > 0 {
		sqlText += ` AND recorded_at >= ?`
		args = append(args, fromMs)
	}
	if toMs > 0 {
		sqlText += ` AND recorded_at <= ?`
		args = append(args, toMs)
	}
	sqlText += ` ORDER BY recorded_at DESC`
	if limit > 0 {
		sqlText += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := r.exec.Query(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("position repo: list query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*domain.Position
	for rows.Next() {
		var p domain.Position
		if scanErr := scanPositionRows(rows, &p); scanErr != nil {
			return nil, fmt.Errorf("position repo: list scan: %w", scanErr)
		}
		out = append(out, &p)
	}
	if iterErr := rows.Err(); iterErr != nil {
		return nil, fmt.Errorf("position repo: list iterate: %w", iterErr)
	}
	if out == nil {
		// Non-nil empty slice so callers can safely len/range without
		// a nil guard (same pattern as VehicleRepo.List).
		out = []*domain.Position{}
	}
	return out, nil
}
