package domain

// Position is one GPS reading reported by a driver-owned vehicle.
//
// Field types mirror the D1 schema exactly:
//
//   - ID is an auto-increment INTEGER assigned by D1 on INSERT (the
//     repository populates this field after persisting). It is the only
//     numeric primary key in the domain — every other entity uses a UUID
//     string — because positions are append-only telemetry where ordering
//     by recorded_at is the only access pattern that matters, and the
//     auto-increment rowid is the cheapest source of monotonic ordering
//     SQLite gives us.
//   - VehicleID is the TEXT foreign key into vehicles(id).
//   - Lat/Lng are degrees in the WGS-84 system; the usecase enforces
//     lat ∈ [-90, 90] and lng ∈ [-180, 180].
//   - SpeedKmh is the optional reported ground speed. The DB column is
//     nullable. We model "unset" as the Go zero value 0.0 rather than a
//     *float64 pointer because (a) every consumer of this struct treats
//     the field as a display-only convenience and (b) a real reading of
//     "exactly 0 km/h" is indistinguishable from "unset" in practice
//     (the vehicle is stationary). Callers that need explicit nullability
//     should consult the recorded_at timestamp and decide based on
//     domain context.
//   - RecordedAt is the unix-ms wall-clock instant the GPS reading was
//     captured, as reported by the driver's client. The usecase enforces
//     freshness (±5 minutes around the server's now) so replay-style
//     spoofing is bounded.
//   - CreatedAt is the unix-ms instant the server persisted the row,
//     stamped by the repository at insert time. This is the "trusted"
//     timeline for audit/history; RecordedAt is the "claimed" timeline.
//
// The struct is a plain data carrier: no methods, no construction-time
// invariants. The usecase layer validates; the repository persists.
type Position struct {
	ID         int64
	VehicleID  string
	Lat        float64
	Lng        float64
	SpeedKmh   float64
	RecordedAt int64
	CreatedAt  int64
}
