package d1

import (
	"context"
	"errors"
	"fmt"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
)

// GeofenceRepo is the D1-backed implementation of the geofence storage
// interface declared at the consumer site (internal/usecase). Like the
// other repos in this package it depends only on the narrow Executor
// contract so the in-memory sqlite test double satisfies it just as
// well as the production D1 client does.
type GeofenceRepo struct {
	exec Executor
}

// NewGeofenceRepo constructs a repository bound to exec. exec is captured
// verbatim; callers are responsible for its lifecycle.
func NewGeofenceRepo(exec Executor) *GeofenceRepo {
	return &GeofenceRepo{exec: exec}
}

// geofenceColumnList is the canonical SELECT projection ordered to match
// the alphabetical column order the D1 client's row scanner produces —
// the same pattern driver_repo / vehicle_repo / position_repo use so one
// scan binding works against both the in-memory sqlite test double and
// the production D1 HTTP client.
const geofenceColumnList = "center_lat, center_lng, created_at, id, radius_m, vehicle_id"

// scanGeofence reads a single row in the column order documented by
// geofenceColumnList. RadiusM is stored as INTEGER in the schema and
// returned as int64 by the driver; we narrow to int (the domain type)
// here because the usecase's validation bounds (50..50_000) fit easily
// inside a 32-bit int, and the wider int64 would force casts at every
// call site.
func scanGeofence(row Row, g *domain.Geofence) error {
	var radius int64
	if err := row.Scan(
		&g.CenterLat,
		&g.CenterLng,
		&g.CreatedAt,
		&g.ID,
		&radius,
		&g.VehicleID,
	); err != nil {
		return err
	}
	g.RadiusM = int(radius)
	return nil
}

// GetByVehicle returns the geofence for the vehicle, or domain.ErrNotFound
// when no fence has been set. The 1:1 invariant ("at most one fence per
// vehicle") is enforced by Upsert; callers can rely on this method
// returning either the single row or ErrNotFound, never multiple rows.
func (r *GeofenceRepo) GetByVehicle(ctx context.Context, vehicleID string) (*domain.Geofence, error) {
	if r == nil || r.exec == nil {
		return nil, errors.New("geofence repo: nil executor")
	}
	row := r.exec.QueryRow(ctx,
		`SELECT `+geofenceColumnList+` FROM geofences WHERE vehicle_id = ?`,
		vehicleID,
	)
	var g domain.Geofence
	if err := scanGeofence(row, &g); err != nil {
		if isNoRowsErr(err) {
			return nil, fmt.Errorf("geofence for vehicle %s: %w", vehicleID, domain.ErrNotFound)
		}
		return nil, fmt.Errorf("geofence repo: get-by-vehicle: %w", err)
	}
	return &g, nil
}

// Upsert sets (or replaces) the geofence for a vehicle.
//
// Implementation note: D1 does not support multi-statement transactions
// over the HTTP API today, so we cannot wrap DELETE + INSERT in a true
// transaction. We pick "DELETE WHERE vehicle_id=? followed by INSERT"
// as the idempotent two-step:
//
//   - DELETE removes any pre-existing fence for this vehicle (no-op if
//     none exists).
//   - INSERT writes the new row.
//
// If the request is interrupted between the two statements, the
// vehicle ends up fence-less — which is the same outcome as a no-op
// from the caller's perspective: re-running Upsert is safe and produces
// the intended end state. We deliberately favour this over the inverse
// (INSERT first, then DELETE the older row) because the inverse leaves
// the user with TWO active fences on failure, which violates the 1:1
// invariant.
//
// The vehicle's existence is the caller's (usecase's) responsibility to
// validate before calling here; the foreign-key constraint will reject
// an orphan insert with a wrapped error.
func (r *GeofenceRepo) Upsert(ctx context.Context, g *domain.Geofence) error {
	if r == nil || r.exec == nil {
		return errors.New("geofence repo: nil executor")
	}
	if g == nil {
		return errors.New("geofence repo: nil geofence")
	}

	if err := r.exec.Exec(ctx,
		`DELETE FROM geofences WHERE vehicle_id = ?`,
		g.VehicleID,
	); err != nil {
		return fmt.Errorf("geofence repo: upsert (delete prior): %w", err)
	}

	if err := r.exec.Exec(ctx,
		`INSERT INTO geofences (id, vehicle_id, center_lat, center_lng, radius_m, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		g.ID, g.VehicleID, g.CenterLat, g.CenterLng, g.RadiusM, g.CreatedAt,
	); err != nil {
		return fmt.Errorf("geofence repo: upsert (insert): %w", err)
	}
	return nil
}

// Delete removes the fence for a vehicle. Returns nil even when no row
// exists — idempotent by design so callers can issue an unconditional
// "clear the fence" without first probing existence.
func (r *GeofenceRepo) Delete(ctx context.Context, vehicleID string) error {
	if r == nil || r.exec == nil {
		return errors.New("geofence repo: nil executor")
	}
	if err := r.exec.Exec(ctx,
		`DELETE FROM geofences WHERE vehicle_id = ?`,
		vehicleID,
	); err != nil {
		return fmt.Errorf("geofence repo: delete: %w", err)
	}
	return nil
}
