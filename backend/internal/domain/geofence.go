package domain

// Geofence is a circular boundary attached 1:1 to a vehicle. When a
// position update crosses the boundary (in or out) the position
// usecase emits a `geofence.alert` event for live broadcast.
//
// Field types mirror the D1 schema (see migrations/000001_init.up.sql):
//
//   - ID is a UUID v7 string generated app-side. The DB column is
//     unique; the schema enforces at most one fence row per fence-id
//     but does NOT enforce one fence per vehicle at the DDL level. The
//     repository layer enforces "one fence per vehicle" by DELETE-then-
//     INSERT on Upsert.
//   - VehicleID is the TEXT foreign key into vehicles(id) with ON
//     DELETE CASCADE — deleting a vehicle drops its fence automatically.
//   - CenterLat / CenterLng are degrees in WGS-84. The usecase
//     validates lat ∈ [-90, 90], lng ∈ [-180, 180].
//   - RadiusM is the fence radius in metres. INTEGER per the schema —
//     fractional metres serve no operational purpose and the integer
//     type forces clients to pick a sensible bound (the usecase
//     enforces [50, 50_000]).
//   - CreatedAt is the unix-ms instant the fence was last set. We do
//     not track UpdatedAt because Upsert replaces the row wholesale —
//     "updated" and "created" are the same instant for our purposes.
//
// The struct is a plain data carrier: no methods, no construction-time
// invariants. The usecase layer validates; the repository persists.
type Geofence struct {
	ID        string  // UUID v7
	VehicleID string  // 1:1 with vehicles; one fence per vehicle by repo invariant
	CenterLat float64 // degrees, [-90, 90]
	CenterLng float64 // degrees, [-180, 180]
	RadiusM   int     // metres, usecase bounds [50, 50_000]
	CreatedAt int64   // unix-ms
}
