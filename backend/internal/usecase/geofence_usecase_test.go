package usecase

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
)

// ---------------------------------------------------------------------------
// In-memory fakes. Kept inline so each test case stays one screen.
// ---------------------------------------------------------------------------

// memGeofenceRepo is the in-memory store for fences. Tests can inspect
// the captured rows directly via .stored and inject errors via .failOn.
type memGeofenceRepo struct {
	mu     sync.Mutex
	stored map[string]*domain.Geofence // vehicleID -> fence (1:1 invariant)
	// failOn maps method name → error to return on the next call. Cleared
	// when consumed so a one-shot failure does not leak between cases.
	failOn map[string]error
}

func newMemGeofenceRepo() *memGeofenceRepo {
	return &memGeofenceRepo{
		stored: map[string]*domain.Geofence{},
		failOn: map[string]error{},
	}
}

func (m *memGeofenceRepo) consumeFail(method string) error {
	if err, ok := m.failOn[method]; ok {
		delete(m.failOn, method)
		return err
	}
	return nil
}

func (m *memGeofenceRepo) GetByVehicle(_ context.Context, vehicleID string) (*domain.Geofence, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.consumeFail("GetByVehicle"); err != nil {
		return nil, err
	}
	g, ok := m.stored[vehicleID]
	if !ok {
		return nil, fmt.Errorf("fence for vehicle %s: %w", vehicleID, domain.ErrNotFound)
	}
	cp := *g
	return &cp, nil
}

func (m *memGeofenceRepo) Upsert(_ context.Context, g *domain.Geofence) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.consumeFail("Upsert"); err != nil {
		return err
	}
	cp := *g
	m.stored[g.VehicleID] = &cp
	return nil
}

func (m *memGeofenceRepo) Delete(_ context.Context, vehicleID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.consumeFail("Delete"); err != nil {
		return err
	}
	delete(m.stored, vehicleID)
	return nil
}

// queuedIDs is a deterministic IDGenerator that hands out a queue of
// pre-set strings. Tests reset / extend the queue per case so the
// generated fence IDs are predictable. Distinct from vehicle_usecase_test's
// fixedIDs (a simple counter) — we need to assert on specific IDs here.
type queuedIDs struct {
	mu  sync.Mutex
	ids []string
}

func newQueuedIDs(ids ...string) *queuedIDs { return &queuedIDs{ids: ids} }

func (f *queuedIDs) NewID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ids) == 0 {
		return "id-pool-exhausted"
	}
	id := f.ids[0]
	f.ids = f.ids[1:]
	return id
}

// newTestGeo wires a usecase + fakes with a fixed clock and ID pool.
// Returns the usecase + every collaborator so tests can both drive
// inputs and inspect captured state without leaking helpers into
// every case.
func newTestGeo(t *testing.T, now time.Time, ids ...string) (
	*GeofenceUsecase,
	*memGeofenceRepo,
	*memVehicleLookup,
	*queuedIDs,
) {
	t.Helper()
	repo := newMemGeofenceRepo()
	lookup := newMemVehicleLookup()
	idGen := newQueuedIDs(ids...)
	uc, err := NewGeofenceUsecase(repo, lookup, idGen)
	if err != nil {
		t.Fatalf("NewGeofenceUsecase: %v", err)
	}
	uc.now = fixedNow(now)
	return uc, repo, lookup, idGen
}

func validSetInput(vehicleID string) SetGeofenceInput {
	return SetGeofenceInput{
		VehicleID: vehicleID,
		CenterLat: 13.7563, // Bangkok
		CenterLng: 100.5018,
		RadiusM:   500,
	}
}

// ---------------------------------------------------------------------------
// Construction.
// ---------------------------------------------------------------------------

func TestNewGeofenceUsecase_RejectsNilDeps(t *testing.T) {
	repo := newMemGeofenceRepo()
	lookup := newMemVehicleLookup()
	ids := newQueuedIDs("x")

	cases := []struct {
		name string
		r    geofenceRepo
		v    vehicleExistenceLookup
		i    IDGenerator
	}{
		{"nil fences", nil, lookup, ids},
		{"nil vehicles", repo, nil, ids},
		{"nil ids", repo, lookup, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewGeofenceUsecase(tc.r, tc.v, tc.i); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// GetByVehicle.
// ---------------------------------------------------------------------------

func TestGetByVehicle_RequiresID(t *testing.T) {
	uc, _, _, _ := newTestGeo(t, time.Unix(1_700_000_000, 0))
	_, err := uc.GetByVehicle(context.Background(), "   ")
	if err == nil {
		t.Fatal("empty vehicle_id must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

func TestGetByVehicle_NotFoundPropagates(t *testing.T) {
	uc, _, _, _ := newTestGeo(t, time.Unix(1_700_000_000, 0))
	_, err := uc.GetByVehicle(context.Background(), "veh_1")
	if err == nil {
		t.Fatal("missing fence must fail")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestGetByVehicle_Success(t *testing.T) {
	uc, repo, _, _ := newTestGeo(t, time.Unix(1_700_000_000, 0))
	repo.stored["veh_1"] = &domain.Geofence{
		ID: "fence_1", VehicleID: "veh_1",
		CenterLat: 13.0, CenterLng: 100.0, RadiusM: 250, CreatedAt: 1,
	}

	got, err := uc.GetByVehicle(context.Background(), "veh_1")
	if err != nil {
		t.Fatalf("GetByVehicle: %v", err)
	}
	if got.ID != "fence_1" || got.RadiusM != 250 {
		t.Errorf("unexpected fence: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Set — validation branches.
// ---------------------------------------------------------------------------

func TestSet_RequiresVehicleID(t *testing.T) {
	uc, _, _, _ := newTestGeo(t, time.Unix(1_700_000_000, 0), "fid")
	in := validSetInput("   ")
	_, err := uc.Set(context.Background(), in)
	if err == nil {
		t.Fatal("empty VehicleID must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

func TestSet_LatOutOfRange(t *testing.T) {
	for _, lat := range []float64{-90.0001, 90.0001} {
		uc, _, lookup, _ := newTestGeo(t, time.Unix(1_700_000_000, 0), "fid")
		lookup.Set("veh_1", "drv_1")
		in := validSetInput("veh_1")
		in.CenterLat = lat
		_, err := uc.Set(context.Background(), in)
		if err == nil {
			t.Fatalf("lat=%f must fail", lat)
		}
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("lat=%f: expected ErrValidation, got: %v", lat, err)
		}
	}
}

func TestSet_LngOutOfRange(t *testing.T) {
	for _, lng := range []float64{-180.0001, 180.0001} {
		uc, _, lookup, _ := newTestGeo(t, time.Unix(1_700_000_000, 0), "fid")
		lookup.Set("veh_1", "drv_1")
		in := validSetInput("veh_1")
		in.CenterLng = lng
		_, err := uc.Set(context.Background(), in)
		if err == nil {
			t.Fatalf("lng=%f must fail", lng)
		}
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("lng=%f: expected ErrValidation, got: %v", lng, err)
		}
	}
}

func TestSet_RadiusOutOfRange(t *testing.T) {
	for _, r := range []int{49, 0, -10, 50_001, 1_000_000} {
		uc, _, lookup, _ := newTestGeo(t, time.Unix(1_700_000_000, 0), "fid")
		lookup.Set("veh_1", "drv_1")
		in := validSetInput("veh_1")
		in.RadiusM = r
		_, err := uc.Set(context.Background(), in)
		if err == nil {
			t.Fatalf("radius=%d must fail", r)
		}
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("radius=%d: expected ErrValidation, got: %v", r, err)
		}
	}
}

func TestSet_RadiusBoundaryInclusive(t *testing.T) {
	// 50 m and 50_000 m must both succeed — the bounds are inclusive.
	for _, r := range []int{50, 50_000} {
		uc, _, lookup, _ := newTestGeo(t, time.Unix(1_700_000_000, 0), "fid")
		lookup.Set("veh_1", "drv_1")
		in := validSetInput("veh_1")
		in.RadiusM = r
		if _, err := uc.Set(context.Background(), in); err != nil {
			t.Fatalf("radius=%d at boundary should succeed: %v", r, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Set — vehicle existence.
// ---------------------------------------------------------------------------

func TestSet_MissingVehicle(t *testing.T) {
	uc, _, _, _ := newTestGeo(t, time.Unix(1_700_000_000, 0), "fid")
	// No Set on the lookup → ErrNotFound from Get.
	in := validSetInput("veh_does_not_exist")
	_, err := uc.Set(context.Background(), in)
	if err == nil {
		t.Fatal("missing vehicle must fail")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestSet_LookupInfraError(t *testing.T) {
	uc, _, lookup, _ := newTestGeo(t, time.Unix(1_700_000_000, 0), "fid")
	lookup.Fail("veh_1", errors.New("D1 timeout"))
	in := validSetInput("veh_1")
	_, err := uc.Set(context.Background(), in)
	if err == nil {
		t.Fatal("infra error must propagate")
	}
	if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrValidation) {
		t.Fatalf("infra error must not be classified as a domain error: %v", err)
	}
}

func TestSet_LookupNilNil(t *testing.T) {
	// Defensive: a misbehaving lookup that returns (nil, nil) must
	// surface as ErrNotFound, not NPE.
	repo := newMemGeofenceRepo()
	ids := newQueuedIDs("fid")
	uc, err := NewGeofenceUsecase(repo, nilNilLookup{}, ids)
	if err != nil {
		t.Fatalf("NewGeofenceUsecase: %v", err)
	}
	uc.now = fixedNow(time.Unix(1_700_000_000, 0))

	_, err = uc.Set(context.Background(), validSetInput("veh_x"))
	if err == nil {
		t.Fatal("nil-nil lookup must fail")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Set — repo errors.
// ---------------------------------------------------------------------------

func TestSet_UpsertErrorPropagates(t *testing.T) {
	uc, repo, lookup, _ := newTestGeo(t, time.Unix(1_700_000_000, 0), "fid")
	lookup.Set("veh_1", "drv_1")
	repo.failOn["Upsert"] = errors.New("D1 upsert blew up")

	_, err := uc.Set(context.Background(), validSetInput("veh_1"))
	if err == nil {
		t.Fatal("upsert error must propagate")
	}
}

// ---------------------------------------------------------------------------
// Set — happy path.
// ---------------------------------------------------------------------------

func TestSet_Success(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	uc, repo, lookup, ids := newTestGeo(t, now, "fence_uuid_1")
	lookup.Set("veh_1", "drv_1")

	in := validSetInput("veh_1")
	got, err := uc.Set(context.Background(), in)
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Returned struct echoes input + injected ID + clock-stamped timestamp.
	if got.ID != "fence_uuid_1" {
		t.Errorf("ID: got %q, want fence_uuid_1", got.ID)
	}
	if got.VehicleID != "veh_1" {
		t.Errorf("VehicleID: got %q, want veh_1", got.VehicleID)
	}
	if got.CenterLat != 13.7563 || got.CenterLng != 100.5018 {
		t.Errorf("center: got (%f, %f), want Bangkok", got.CenterLat, got.CenterLng)
	}
	if got.RadiusM != 500 {
		t.Errorf("RadiusM: got %d, want 500", got.RadiusM)
	}
	if got.CreatedAt != now.UnixMilli() {
		t.Errorf("CreatedAt: got %d, want %d", got.CreatedAt, now.UnixMilli())
	}

	// Persisted via repo.Upsert exactly once.
	if len(repo.stored) != 1 {
		t.Fatalf("expected 1 stored fence, got %d", len(repo.stored))
	}
	stored := repo.stored["veh_1"]
	if stored.ID != "fence_uuid_1" {
		t.Errorf("stored ID: got %q, want fence_uuid_1", stored.ID)
	}
	// ID pool consumed once.
	if len(ids.ids) != 0 {
		t.Errorf("expected ID pool drained, remaining: %v", ids.ids)
	}
}

func TestSet_ReplacesExisting(t *testing.T) {
	// Two consecutive Set calls must produce exactly one stored fence
	// (the latest) — the repo's Upsert is responsible for the
	// replacement, the usecase doesn't need extra logic for this.
	uc, repo, lookup, _ := newTestGeo(t, time.Unix(1_700_000_000, 0), "f1", "f2")
	lookup.Set("veh_1", "drv_1")

	in1 := validSetInput("veh_1")
	if _, err := uc.Set(context.Background(), in1); err != nil {
		t.Fatalf("first Set: %v", err)
	}
	in2 := validSetInput("veh_1")
	in2.RadiusM = 1000
	got, err := uc.Set(context.Background(), in2)
	if err != nil {
		t.Fatalf("second Set: %v", err)
	}

	if got.ID != "f2" || got.RadiusM != 1000 {
		t.Errorf("second Set must return the new fence, got: %+v", got)
	}
	if len(repo.stored) != 1 {
		t.Fatalf("expected exactly 1 stored fence after replace, got %d", len(repo.stored))
	}
	if repo.stored["veh_1"].ID != "f2" {
		t.Errorf("stored fence should be f2, got %q", repo.stored["veh_1"].ID)
	}
}
