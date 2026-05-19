// Package geo provides geographic primitives used by the fleet tracker.
//
// Haversine distance is exact enough for our use case (geofence radii in
// the 50m–10km range over short transitions); we explicitly do NOT use
// flat-earth approximations because typical Bangkok routes cross enough
// latitude to make them inaccurate.
package geo

import "math"

// EarthRadiusMeters is the WGS-84 mean Earth radius. Used by Haversine.
//
// The exact figure 6_371_008.8 m is the IUGG-recommended mean radius —
// the value used by every Maps SDK we interoperate with (Google, Leaflet,
// Mapbox). Using a different constant here would produce a "fence is at
// 502 metres on the map but the server thinks 498" mismatch that is
// impossible to debug at the user layer.
const EarthRadiusMeters = 6_371_008.8

// Point is a geographic coordinate in decimal degrees.
//
// The struct is a plain data carrier: no methods, no construction-time
// invariants. Callers that need bounds enforcement (lat ∈ [-90, 90],
// lng ∈ [-180, 180]) do it themselves — the usecase layer already
// re-checks at the HTTP boundary, so adding a constructor here would
// only duplicate that work.
type Point struct {
	Lat float64 // -90..90
	Lng float64 // -180..180
}

// HaversineMeters returns the great-circle distance in meters between
// two points. Always non-negative; identical points return 0.
//
// The implementation is the textbook Haversine formula:
//
//	a = sin²(Δlat/2) + cos(lat1) · cos(lat2) · sin²(Δlng/2)
//	c = 2 · asin(min(1, √a))
//	d = R · c
//
// The min(1, √a) guard handles floating-point overshoot for antipodal
// points where rounding can push a above 1.0 and produce NaN from asin.
// In practice it never trips for our use case (Bangkok-area geofences),
// but the guard costs one comparison and removes a sharp edge from the
// contract.
func HaversineMeters(a, b Point) float64 {
	lat1 := a.Lat * math.Pi / 180
	lat2 := b.Lat * math.Pi / 180
	dlat := (b.Lat - a.Lat) * math.Pi / 180
	dlng := (b.Lng - a.Lng) * math.Pi / 180

	h := math.Sin(dlat/2)*math.Sin(dlat/2) +
		math.Cos(lat1)*math.Cos(lat2)*math.Sin(dlng/2)*math.Sin(dlng/2)
	c := 2 * math.Asin(math.Min(1, math.Sqrt(h)))
	return EarthRadiusMeters * c
}
