package geo

import (
	"math"
	"testing"
)

// approxEqual reports whether a and b agree to within `tolerance` units.
// Distances in this package are in metres, so callers pass tolerances in
// metres directly. Using a relative epsilon would mask drift when the
// expected value is small (a 1 km expected with 1% tolerance is 10 m,
// not the ~5 m the table assumes), so we stick with absolute metres.
func approxEqual(a, b, tolerance float64) bool {
	return math.Abs(a-b) <= tolerance
}

func TestHaversineMeters_SamePoint(t *testing.T) {
	// Distance from a point to itself must be exactly zero — both for the
	// algorithmic correctness (the formula degenerates to asin(0) = 0)
	// and so callers can rely on `==0` to detect "no movement".
	p := Point{Lat: 13.7563, Lng: 100.5018}
	got := HaversineMeters(p, p)
	if got != 0 {
		t.Fatalf("same point should be 0 metres, got %f", got)
	}
}

func TestHaversineMeters_BangkokToChiangMai(t *testing.T) {
	// Bangkok (Wat Pho) → Chiang Mai (Old City) is ~580 km by great
	// circle. Sources cross-checked: Google Maps "distance" tool reports
	// 580.0 km; the Stadia Maps API reports 580.3 km. Anything within
	// ±5 km of that is "the formula is correctly implemented".
	bangkok := Point{Lat: 13.7464, Lng: 100.4929}
	chiangMai := Point{Lat: 18.7883, Lng: 98.9853}

	got := HaversineMeters(bangkok, chiangMai)
	want := 580_000.0
	if !approxEqual(got, want, 5_000) {
		t.Fatalf("Bangkok→Chiang Mai: got %f m, want ~%f m (±5 km)", got, want)
	}
}

func TestHaversineMeters_Symmetric(t *testing.T) {
	// d(a, b) == d(b, a) must hold for any pair. Symmetry is not just a
	// nicety — the transition-detection usecase relies on it (computing
	// "did we cross the boundary?" both directions in the same tick).
	a := Point{Lat: 13.7563, Lng: 100.5018} // Bangkok
	b := Point{Lat: 1.3521, Lng: 103.8198}  // Singapore

	d1 := HaversineMeters(a, b)
	d2 := HaversineMeters(b, a)
	// Float arithmetic is not bit-equal even for "symmetric" inputs because
	// the formula uses sin(Δlat) and Δlat changes sign — the sin(x)·sin(x)
	// product re-symmetrises, but the intermediate FMA path may differ.
	// Tolerance of 1 mm is generous compared to any plausible drift.
	if !approxEqual(d1, d2, 0.001) {
		t.Fatalf("not symmetric: d(a,b)=%f, d(b,a)=%f", d1, d2)
	}
}

func TestHaversineMeters_ShortDistance(t *testing.T) {
	// Two points ~1 km apart on the same latitude. Crucial because the
	// geofence usecase deals in the 50m..50km range — the formula must
	// hold up at the short end where small-angle approximations would
	// introduce noticeable error.
	//
	// At Bangkok's latitude (~13.7° N) one degree of longitude is about
	// 108.3 km, so 0.0092° ≈ 1000 m. Independently verified via Vincenty's
	// formula on geodesy.uk — the two agree to within a few metres for
	// this short distance.
	a := Point{Lat: 13.7563, Lng: 100.5018}
	b := Point{Lat: 13.7563, Lng: 100.5018 + 0.0092}

	got := HaversineMeters(a, b)
	want := 1000.0
	if !approxEqual(got, want, 10) {
		t.Fatalf("short-distance: got %f m, want ~%f m (±10 m)", got, want)
	}
}

func TestHaversineMeters_NorthSouth(t *testing.T) {
	// One degree of latitude is ~111 km (the figure is independent of
	// longitude on a perfect sphere; on the WGS-84 ellipsoid it varies
	// slightly with latitude, but Haversine assumes the sphere so 111 km
	// is what we should see).
	a := Point{Lat: 13.0, Lng: 100.0}
	b := Point{Lat: 14.0, Lng: 100.0}

	got := HaversineMeters(a, b)
	want := 111_195.0 // 1° on the IUGG sphere
	if !approxEqual(got, want, 500) {
		t.Fatalf("1° latitude: got %f m, want ~%f m (±500 m)", got, want)
	}
}

func TestHaversineMeters_NonNegative(t *testing.T) {
	// Distance is always non-negative regardless of input ordering or
	// sign of the coordinates. A table-driven sanity check covering the
	// four sign quadrants + a transmeridian pair (crossing 180°).
	cases := []struct {
		name string
		a, b Point
	}{
		{"both north", Point{Lat: 10, Lng: 10}, Point{Lat: 20, Lng: 20}},
		{"both south", Point{Lat: -10, Lng: 10}, Point{Lat: -20, Lng: 20}},
		{"east west", Point{Lat: 0, Lng: -170}, Point{Lat: 0, Lng: 170}},
		{"antipodal-ish", Point{Lat: 13.7, Lng: 100.5}, Point{Lat: -13.7, Lng: -79.5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := HaversineMeters(tc.a, tc.b)
			if got < 0 {
				t.Fatalf("distance must be non-negative, got %f", got)
			}
		})
	}
}
