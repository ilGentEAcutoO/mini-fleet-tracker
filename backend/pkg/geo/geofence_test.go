package geo

import "testing"

// TestIsInsideCircle is a table-driven sweep through the inside/outside/
// boundary cases. Coordinates are picked from a Bangkok-area fence to
// match the realistic scale our usecase deals with (50m..50km radii).
//
// "Boundary" cases use the same lat/lng for the point and computed-from-
// distance pair so the math is reproducible: at Bangkok's latitude the
// per-degree-longitude length is ~108 km, so 100 m offset corresponds to
// 100/108000 degrees of lng.
func TestIsInsideCircle(t *testing.T) {
	center := Point{Lat: 13.7563, Lng: 100.5018}
	// degLngFor100m approximates the longitude delta that produces a
	// 100 m east-west offset at Bangkok's latitude. Verified against
	// HaversineMeters: this value yields ~100 m.
	const degLngFor100m = 100.0 / 108_000.0

	cases := []struct {
		name        string
		center      Point
		radius      float64
		point       Point
		wantInside  bool
		description string
	}{
		{
			name:        "exact center",
			center:      center,
			radius:      500,
			point:       center,
			wantInside:  true,
			description: "the center is trivially inside any positive-radius circle",
		},
		{
			name:        "10 m inside a 100 m fence",
			center:      center,
			radius:      100,
			point:       Point{Lat: center.Lat, Lng: center.Lng + degLngFor100m*0.1},
			wantInside:  true,
			description: "comfortably inside",
		},
		{
			name:        "90 m inside a 100 m fence",
			center:      center,
			radius:      100,
			point:       Point{Lat: center.Lat, Lng: center.Lng + degLngFor100m*0.9},
			wantInside:  true,
			description: "near boundary but still inside",
		},
		{
			name:        "200 m outside a 100 m fence",
			center:      center,
			radius:      100,
			point:       Point{Lat: center.Lat, Lng: center.Lng + degLngFor100m*2.0},
			wantInside:  false,
			description: "clearly outside",
		},
		{
			name:        "1 km outside a 100 m fence",
			center:      center,
			radius:      100,
			point:       Point{Lat: center.Lat, Lng: center.Lng + degLngFor100m*10.0},
			wantInside:  false,
			description: "very outside",
		},
		{
			name:        "approximately on the boundary, inclusive",
			center:      center,
			radius:      HaversineMeters(center, Point{Lat: center.Lat, Lng: center.Lng + degLngFor100m*0.95}),
			point:       Point{Lat: center.Lat, Lng: center.Lng + degLngFor100m*0.95},
			wantInside:  true,
			description: "boundary points must be classified inside per our convention",
		},
		{
			name:        "epsilon outside the boundary",
			center:      center,
			radius:      100,
			point:       Point{Lat: center.Lat, Lng: center.Lng + degLngFor100m*1.01},
			wantInside:  false,
			description: "just past the boundary must be outside",
		},
		{
			name:        "north-south offset, inside",
			center:      center,
			radius:      500,
			point:       Point{Lat: center.Lat + 0.001, Lng: center.Lng}, // ~111 m north
			wantInside:  true,
			description: "lat offset is independent of longitude scale; ~111 m < 500 m",
		},
		{
			name:        "large radius, far point inside",
			center:      center,
			radius:      50_000,
			point:       Point{Lat: 14.0, Lng: 100.5},
			wantInside:  true,
			description: "27 km north is comfortably inside 50 km radius",
		},
		{
			name:        "large radius, far point outside",
			center:      center,
			radius:      50_000,
			point:       Point{Lat: 18.7883, Lng: 98.9853}, // Chiang Mai
			wantInside:  false,
			description: "Chiang Mai is 580 km away — outside any 50 km fence",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsInsideCircle(tc.center, tc.radius, tc.point)
			if got != tc.wantInside {
				dist := HaversineMeters(tc.center, tc.point)
				t.Errorf("%s: want inside=%v, got %v (distance=%.2f m, radius=%.2f m)",
					tc.description, tc.wantInside, got, dist, tc.radius)
			}
		})
	}
}

func TestIsInsideCircle_BoundaryInclusive(t *testing.T) {
	// Boundary inclusivity is a contract — separate test (not a table
	// entry) because the rationale is documented at the function level.
	// We construct a point whose distance from center is exactly some
	// known value R, then check that IsInsideCircle(center, R, point)
	// returns true.
	center := Point{Lat: 13.7563, Lng: 100.5018}
	point := Point{Lat: 13.7563, Lng: 100.5018 + 0.01}

	dist := HaversineMeters(center, point)
	// Use the computed distance as the radius — the boundary is exactly
	// at the point. The point must classify as inside.
	if !IsInsideCircle(center, dist, point) {
		t.Errorf("boundary point at distance=%.6f m must be inside fence with radius=%.6f m",
			dist, dist)
	}
}

func TestIsInsideCircle_ZeroRadius(t *testing.T) {
	// Radius 0 is degenerate but the contract still has a defined
	// answer: only the exact center is inside (HaversineMeters returns 0
	// for identical points, and 0 <= 0). Any other point is outside.
	center := Point{Lat: 13.7563, Lng: 100.5018}
	if !IsInsideCircle(center, 0, center) {
		t.Error("radius=0 should include the exact center (distance 0 <= 0)")
	}
	other := Point{Lat: 13.7564, Lng: 100.5018}
	if IsInsideCircle(center, 0, other) {
		t.Error("radius=0 should exclude any non-center point")
	}
}
