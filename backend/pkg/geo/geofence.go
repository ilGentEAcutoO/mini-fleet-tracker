package geo

// IsInsideCircle reports whether point lies within radiusMeters of center.
// Boundary points (distance == radius exactly) are considered INSIDE,
// matching the "inclusive boundary" convention used by Leaflet/OSM/Google
// circle-overlay APIs. Inclusive boundary keeps the transition-detection
// semantics monotone: a vehicle that sits exactly on the fence boundary
// will not flap between enter/exit on every refresh tick.
//
// Negative or zero radii reduce the predicate to "is this the same point
// as center?" (the only way HaversineMeters returns 0 is when the two
// points are identical). That degenerate case is allowed but the caller's
// validation layer should reject zero/negative radii up front — see
// internal/usecase.GeofenceUsecase.Set.
func IsInsideCircle(center Point, radiusMeters float64, point Point) bool {
	return HaversineMeters(center, point) <= radiusMeters
}
