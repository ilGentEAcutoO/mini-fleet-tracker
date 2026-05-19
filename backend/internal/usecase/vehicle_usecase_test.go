package usecase

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
)

// memVehicleRepo is an in-memory implementation of VehicleRepo for tests.
// It mirrors the production repository's error contract:
//   - Create returns domain.ErrAlreadyExists on plate collision
//   - Get / Update / Delete return domain.ErrNotFound when the ID is missing
//   - Update returns domain.ErrAlreadyExists when a plate rename collides
type memVehicleRepo struct {
	mu       sync.Mutex
	byID     map[string]*domain.Vehicle
	plateIdx map[string]string // plate -> id, kept in sync for collision checks
}

func newMemVehicleRepo() *memVehicleRepo {
	return &memVehicleRepo{
		byID:     map[string]*domain.Vehicle{},
		plateIdx: map[string]string{},
	}
}

func (m *memVehicleRepo) List(_ context.Context) ([]*domain.Vehicle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*domain.Vehicle, 0, len(m.byID))
	for _, v := range m.byID {
		cp := *v
		out = append(out, &cp)
	}
	return out, nil
}

func (m *memVehicleRepo) Get(_ context.Context, id string) (*domain.Vehicle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.byID[id]
	if !ok {
		return nil, fmt.Errorf("id %s: %w", id, domain.ErrNotFound)
	}
	cp := *v
	return &cp, nil
}

func (m *memVehicleRepo) Create(_ context.Context, v *domain.Vehicle) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.plateIdx[v.PlateNumber]; ok {
		return fmt.Errorf("dup plate %s: %w", v.PlateNumber, domain.ErrAlreadyExists)
	}
	cp := *v
	m.byID[v.ID] = &cp
	m.plateIdx[v.PlateNumber] = v.ID
	return nil
}

func (m *memVehicleRepo) Update(_ context.Context, v *domain.Vehicle) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.byID[v.ID]
	if !ok {
		return fmt.Errorf("id %s: %w", v.ID, domain.ErrNotFound)
	}
	// If the plate moved, check for collisions and update the index.
	if existing.PlateNumber != v.PlateNumber {
		if otherID, taken := m.plateIdx[v.PlateNumber]; taken && otherID != v.ID {
			return fmt.Errorf("dup plate %s: %w", v.PlateNumber, domain.ErrAlreadyExists)
		}
		delete(m.plateIdx, existing.PlateNumber)
		m.plateIdx[v.PlateNumber] = v.ID
	}
	cp := *v
	m.byID[v.ID] = &cp
	return nil
}

func (m *memVehicleRepo) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.byID[id]
	if !ok {
		return fmt.Errorf("id %s: %w", id, domain.ErrNotFound)
	}
	delete(m.plateIdx, v.PlateNumber)
	delete(m.byID, id)
	return nil
}

// fixedIDs hands out predictable IDs so tests can assert against the
// generated ID without coupling to UUID randomness.
type fixedIDs struct {
	mu sync.Mutex
	n  int
}

func (f *fixedIDs) NewID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return fmt.Sprintf("veh_%03d", f.n)
}

// memPositionLister is the in-memory PositionLister used by tests that
// exercise VehicleUsecase.ListPositions. Insertion order is preserved so
// tests can assert deterministic DESC ordering. byVehicle maps vehicle id
// -> chronologically-sorted positions (oldest first); ListByVehicleAndRange
// reverses that to satisfy the DESC contract documented on the interface.
type memPositionLister struct {
	mu        sync.Mutex
	byVehicle map[string][]*domain.Position
	// errOverride lets a specific test inject an arbitrary error from
	// the lister without restructuring the fake.
	errOverride error
}

func newMemPositionLister() *memPositionLister {
	return &memPositionLister{byVehicle: map[string][]*domain.Position{}}
}

// add seeds a position. Inserts at the end so the slice stays in insert
// (chronological-ish) order; callers should use monotonic RecordedAt
// values if they want chronological semantics.
func (m *memPositionLister) add(p *domain.Position) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byVehicle[p.VehicleID] = append(m.byVehicle[p.VehicleID], p)
}

func (m *memPositionLister) ListByVehicleAndRange(_ context.Context, vehicleID string, fromMs, toMs int64, limit int) ([]*domain.Position, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.errOverride != nil {
		return nil, m.errOverride
	}
	rows := m.byVehicle[vehicleID]
	// Build the windowed result (caller filters by bounds) and reverse
	// into DESC order to match the production repo's contract.
	out := make([]*domain.Position, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		p := rows[i]
		if fromMs > 0 && p.RecordedAt < fromMs {
			continue
		}
		if toMs > 0 && p.RecordedAt > toMs {
			continue
		}
		cp := *p
		out = append(out, &cp)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

// newTestVehicleUsecase builds a usecase with an in-memory repo, position
// lister, and a fixed clock so assertions on timestamps are deterministic.
func newTestVehicleUsecase(t *testing.T) (*VehicleUsecase, *memVehicleRepo) {
	t.Helper()
	repo := newMemVehicleRepo()
	uc, err := NewVehicleUsecase(repo, newMemPositionLister(), &fixedIDs{})
	if err != nil {
		t.Fatalf("NewVehicleUsecase: %v", err)
	}
	// Pin the clock at 2025-01-01T00:00:00Z so tests can assert exact
	// timestamps without flaking on wall-clock drift.
	fixed := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	uc.now = func() time.Time { return fixed }
	return uc, repo
}

// newTestVehicleUsecaseWithPositions returns the usecase plus the
// position-lister handle so history-specific tests can seed rows or set
// the error override. Mirrors newTestVehicleUsecase otherwise.
func newTestVehicleUsecaseWithPositions(t *testing.T) (*VehicleUsecase, *memVehicleRepo, *memPositionLister) {
	t.Helper()
	repo := newMemVehicleRepo()
	pl := newMemPositionLister()
	uc, err := NewVehicleUsecase(repo, pl, &fixedIDs{})
	if err != nil {
		t.Fatalf("NewVehicleUsecase: %v", err)
	}
	fixed := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	uc.now = func() time.Time { return fixed }
	return uc, repo, pl
}

// ---------------------------------------------------------------------------
// Construction.
// ---------------------------------------------------------------------------

func TestNewVehicleUsecase_RejectsNilDependencies(t *testing.T) {
	repo := newMemVehicleRepo()
	pl := newMemPositionLister()
	ids := &fixedIDs{}

	cases := []struct {
		name string
		r    VehicleRepo
		p    PositionLister
		id   IDGenerator
	}{
		{"nil repo", nil, pl, ids},
		{"nil positions", repo, nil, ids},
		{"nil ids", repo, pl, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewVehicleUsecase(tc.r, tc.p, tc.id); err == nil {
				t.Fatalf("NewVehicleUsecase should reject %s", tc.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// List.
// ---------------------------------------------------------------------------

func TestVehicleUsecase_List_Empty(t *testing.T) {
	uc, _ := newTestVehicleUsecase(t)
	got, err := uc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got == nil {
		t.Fatal("List must return a non-nil slice")
	}
	if len(got) != 0 {
		t.Fatalf("expected 0, got %d", len(got))
	}
}

func TestVehicleUsecase_List_PopulatedAfterCreate(t *testing.T) {
	uc, _ := newTestVehicleUsecase(t)
	if _, err := uc.Create(context.Background(), CreateVehicleInput{PlateNumber: "ABC-1"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := uc.Create(context.Background(), CreateVehicleInput{PlateNumber: "ABC-2"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := uc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 vehicles, got %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// Create.
// ---------------------------------------------------------------------------

func TestVehicleUsecase_Create_Success(t *testing.T) {
	uc, repo := newTestVehicleUsecase(t)
	in := CreateVehicleInput{
		PlateNumber: "  ABC-1234  ", // whitespace must be trimmed
		Model:       "Toyota Hilux",
	}
	v, err := uc.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if v.ID != "veh_001" {
		t.Errorf("ID: want veh_001, got %q", v.ID)
	}
	if v.PlateNumber != "ABC-1234" {
		t.Errorf("PlateNumber not trimmed: %q", v.PlateNumber)
	}
	if v.Model != "Toyota Hilux" {
		t.Errorf("Model: got %q", v.Model)
	}
	if v.DriverID != "" {
		t.Errorf("DriverID: want empty, got %q", v.DriverID)
	}
	// Timestamps must equal the pinned clock.
	wantTS := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	if v.CreatedAt != wantTS || v.UpdatedAt != wantTS {
		t.Errorf("timestamps: got created=%d updated=%d, want %d", v.CreatedAt, v.UpdatedAt, wantTS)
	}
	// The repo should hold the same row.
	if _, ok := repo.byID[v.ID]; !ok {
		t.Fatal("vehicle not persisted in repo")
	}
}

func TestVehicleUsecase_Create_WithValidDriverID(t *testing.T) {
	uc, _ := newTestVehicleUsecase(t)
	driverID := uuid.NewString()
	v, err := uc.Create(context.Background(), CreateVehicleInput{
		PlateNumber: "ABC-1234",
		DriverID:    driverID,
	})
	if err != nil {
		t.Fatalf("Create with driver_id: %v", err)
	}
	if v.DriverID != driverID {
		t.Errorf("DriverID not stored: got %q, want %q", v.DriverID, driverID)
	}
}

func TestVehicleUsecase_Create_EmptyPlate(t *testing.T) {
	uc, _ := newTestVehicleUsecase(t)
	cases := []string{"", "   "}
	for _, plate := range cases {
		_, err := uc.Create(context.Background(), CreateVehicleInput{PlateNumber: plate})
		if err == nil {
			t.Fatalf("Create with plate %q must fail", plate)
		}
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("plate %q: expected ErrValidation, got: %v", plate, err)
		}
	}
}

func TestVehicleUsecase_Create_PlateTooLong(t *testing.T) {
	uc, _ := newTestVehicleUsecase(t)
	long := make([]byte, maxPlateLen+1)
	for i := range long {
		long[i] = 'A'
	}
	_, err := uc.Create(context.Background(), CreateVehicleInput{PlateNumber: string(long)})
	if err == nil {
		t.Fatal("Create with over-long plate must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

func TestVehicleUsecase_Create_ModelTooLong(t *testing.T) {
	uc, _ := newTestVehicleUsecase(t)
	long := make([]byte, maxModelLen+1)
	for i := range long {
		long[i] = 'M'
	}
	_, err := uc.Create(context.Background(), CreateVehicleInput{
		PlateNumber: "OK-1",
		Model:       string(long),
	})
	if err == nil {
		t.Fatal("Create with over-long model must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

func TestVehicleUsecase_Create_InvalidDriverID(t *testing.T) {
	uc, _ := newTestVehicleUsecase(t)
	_, err := uc.Create(context.Background(), CreateVehicleInput{
		PlateNumber: "ABC-1234",
		DriverID:    "not-a-uuid",
	})
	if err == nil {
		t.Fatal("Create with invalid driver_id must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

func TestVehicleUsecase_Create_DuplicatePlate(t *testing.T) {
	uc, _ := newTestVehicleUsecase(t)
	if _, err := uc.Create(context.Background(), CreateVehicleInput{PlateNumber: "DUP-1"}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := uc.Create(context.Background(), CreateVehicleInput{PlateNumber: "DUP-1"})
	if err == nil {
		t.Fatal("second Create with dup plate must fail")
	}
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Get.
// ---------------------------------------------------------------------------

func TestVehicleUsecase_Get_Success(t *testing.T) {
	uc, _ := newTestVehicleUsecase(t)
	v, err := uc.Create(context.Background(), CreateVehicleInput{PlateNumber: "ABC-1234"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := uc.Get(context.Background(), v.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != v.ID {
		t.Fatalf("Get returned wrong row: %+v", got)
	}
}

func TestVehicleUsecase_Get_NotFound(t *testing.T) {
	uc, _ := newTestVehicleUsecase(t)
	_, err := uc.Get(context.Background(), "veh_does_not_exist")
	if err == nil {
		t.Fatal("Get on missing id must fail")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestVehicleUsecase_Get_EmptyID(t *testing.T) {
	uc, _ := newTestVehicleUsecase(t)
	_, err := uc.Get(context.Background(), "   ")
	if err == nil {
		t.Fatal("Get with empty id must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Update.
// ---------------------------------------------------------------------------

func TestVehicleUsecase_Update_Partial(t *testing.T) {
	uc, _ := newTestVehicleUsecase(t)
	v, err := uc.Create(context.Background(), CreateVehicleInput{
		PlateNumber: "ABC-1234",
		Model:       "Toyota Hilux",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Move the clock forward so the UpdatedAt bump is observable.
	uc.now = func() time.Time { return time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC) }

	newPlate := "XYZ-9999"
	got, err := uc.Update(context.Background(), v.ID, UpdateVehicleInput{
		PlateNumber: &newPlate,
		// Model: nil — must remain "Toyota Hilux"
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.PlateNumber != "XYZ-9999" {
		t.Errorf("Plate not updated: %q", got.PlateNumber)
	}
	if got.Model != "Toyota Hilux" {
		t.Errorf("Model should be unchanged, got %q", got.Model)
	}
	wantTS := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC).UnixMilli()
	if got.UpdatedAt != wantTS {
		t.Errorf("UpdatedAt: got %d, want %d", got.UpdatedAt, wantTS)
	}
	// CreatedAt must NOT move on Update.
	if got.CreatedAt != v.CreatedAt {
		t.Errorf("CreatedAt drifted: got %d, want %d", got.CreatedAt, v.CreatedAt)
	}
}

func TestVehicleUsecase_Update_ClearOptionalField(t *testing.T) {
	uc, _ := newTestVehicleUsecase(t)
	v, err := uc.Create(context.Background(), CreateVehicleInput{
		PlateNumber: "ABC-1234",
		Model:       "Toyota Hilux",
		DriverID:    uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Explicit empty string in the patch should clear both nullable fields.
	empty := ""
	got, err := uc.Update(context.Background(), v.ID, UpdateVehicleInput{
		Model:    &empty,
		DriverID: &empty,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Model != "" {
		t.Errorf("Model should be cleared, got %q", got.Model)
	}
	if got.DriverID != "" {
		t.Errorf("DriverID should be cleared, got %q", got.DriverID)
	}
}

func TestVehicleUsecase_Update_NotFound(t *testing.T) {
	uc, _ := newTestVehicleUsecase(t)
	newPlate := "any"
	_, err := uc.Update(context.Background(), "veh_does_not_exist", UpdateVehicleInput{
		PlateNumber: &newPlate,
	})
	if err == nil {
		t.Fatal("Update on missing id must fail")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestVehicleUsecase_Update_EmptyPlate(t *testing.T) {
	uc, _ := newTestVehicleUsecase(t)
	v, err := uc.Create(context.Background(), CreateVehicleInput{PlateNumber: "OK-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	empty := "   "
	_, err = uc.Update(context.Background(), v.ID, UpdateVehicleInput{PlateNumber: &empty})
	if err == nil {
		t.Fatal("Update with blank plate must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

func TestVehicleUsecase_Update_InvalidDriverID(t *testing.T) {
	uc, _ := newTestVehicleUsecase(t)
	v, err := uc.Create(context.Background(), CreateVehicleInput{PlateNumber: "OK-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	bad := "not-a-uuid"
	_, err = uc.Update(context.Background(), v.ID, UpdateVehicleInput{DriverID: &bad})
	if err == nil {
		t.Fatal("Update with invalid driver_id must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

func TestVehicleUsecase_Update_PlateTooLong(t *testing.T) {
	uc, _ := newTestVehicleUsecase(t)
	v, err := uc.Create(context.Background(), CreateVehicleInput{PlateNumber: "OK-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	long := make([]byte, maxPlateLen+1)
	for i := range long {
		long[i] = 'A'
	}
	s := string(long)
	_, err = uc.Update(context.Background(), v.ID, UpdateVehicleInput{PlateNumber: &s})
	if err == nil {
		t.Fatal("Update with over-long plate must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

func TestVehicleUsecase_Update_ModelTooLong(t *testing.T) {
	uc, _ := newTestVehicleUsecase(t)
	v, err := uc.Create(context.Background(), CreateVehicleInput{PlateNumber: "OK-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	long := make([]byte, maxModelLen+1)
	for i := range long {
		long[i] = 'M'
	}
	s := string(long)
	_, err = uc.Update(context.Background(), v.ID, UpdateVehicleInput{Model: &s})
	if err == nil {
		t.Fatal("Update with over-long model must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

func TestVehicleUsecase_Update_EmptyID(t *testing.T) {
	uc, _ := newTestVehicleUsecase(t)
	_, err := uc.Update(context.Background(), "  ", UpdateVehicleInput{})
	if err == nil {
		t.Fatal("Update with empty id must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Delete.
// ---------------------------------------------------------------------------

func TestVehicleUsecase_Delete_Success(t *testing.T) {
	uc, _ := newTestVehicleUsecase(t)
	v, err := uc.Create(context.Background(), CreateVehicleInput{PlateNumber: "GONE-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := uc.Delete(context.Background(), v.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := uc.Get(context.Background(), v.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after Delete, got: %v", err)
	}
}

func TestVehicleUsecase_Delete_NotFound(t *testing.T) {
	uc, _ := newTestVehicleUsecase(t)
	err := uc.Delete(context.Background(), "veh_does_not_exist")
	if err == nil {
		t.Fatal("Delete on missing id must fail")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestVehicleUsecase_Delete_EmptyID(t *testing.T) {
	uc, _ := newTestVehicleUsecase(t)
	if err := uc.Delete(context.Background(), "  "); err == nil {
		t.Fatal("Delete with empty id must fail")
	} else if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ListPositions — TASK-018 history endpoint.
// ---------------------------------------------------------------------------

func TestVehicleUsecase_ListPositions_Success(t *testing.T) {
	uc, _, pl := newTestVehicleUsecaseWithPositions(t)
	v, err := uc.Create(context.Background(), CreateVehicleInput{PlateNumber: "OK-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Three positions, oldest-first; the fake reverses to satisfy the
	// repo's DESC contract.
	for i, ts := range []int64{1000, 2000, 3000} {
		pl.add(&domain.Position{
			ID:         int64(i + 1),
			VehicleID:  v.ID,
			Lat:        13.0 + float64(i)*0.1,
			Lng:        100.0 + float64(i)*0.1,
			RecordedAt: ts,
		})
	}
	got, err := uc.ListPositions(context.Background(), v.ID, 0, 0, 0)
	if err != nil {
		t.Fatalf("ListPositions: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 positions, got %d", len(got))
	}
	// DESC order — newest first.
	if got[0].RecordedAt != 3000 || got[2].RecordedAt != 1000 {
		t.Errorf("DESC order broken: %d, %d, %d", got[0].RecordedAt, got[1].RecordedAt, got[2].RecordedAt)
	}
}

func TestVehicleUsecase_ListPositions_EmptyResult(t *testing.T) {
	// Vehicle exists, no positions. Must return non-nil empty slice + nil
	// error so the handler can JSON-encode `[]` without a nil guard.
	uc, _, _ := newTestVehicleUsecaseWithPositions(t)
	v, err := uc.Create(context.Background(), CreateVehicleInput{PlateNumber: "EMPTY-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := uc.ListPositions(context.Background(), v.ID, 0, 0, 100)
	if err != nil {
		t.Fatalf("ListPositions: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 positions, got %d", len(got))
	}
}

func TestVehicleUsecase_ListPositions_VehicleNotFound(t *testing.T) {
	uc, _, _ := newTestVehicleUsecaseWithPositions(t)
	_, err := uc.ListPositions(context.Background(), "veh_does_not_exist", 0, 0, 0)
	if err == nil {
		t.Fatal("expected error for missing vehicle")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestVehicleUsecase_ListPositions_EmptyID(t *testing.T) {
	uc, _, _ := newTestVehicleUsecaseWithPositions(t)
	_, err := uc.ListPositions(context.Background(), "   ", 0, 0, 0)
	if err == nil {
		t.Fatal("expected validation error for empty id")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

func TestVehicleUsecase_ListPositions_FromGreaterThanTo(t *testing.T) {
	uc, _, _ := newTestVehicleUsecaseWithPositions(t)
	v, err := uc.Create(context.Background(), CreateVehicleInput{PlateNumber: "OK-2"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = uc.ListPositions(context.Background(), v.ID, 5000, 1000, 0)
	if err == nil {
		t.Fatal("expected validation error when from > to")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

func TestVehicleUsecase_ListPositions_NegativeLimit(t *testing.T) {
	uc, _, _ := newTestVehicleUsecaseWithPositions(t)
	v, err := uc.Create(context.Background(), CreateVehicleInput{PlateNumber: "OK-3"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = uc.ListPositions(context.Background(), v.ID, 0, 0, -1)
	if err == nil {
		t.Fatal("expected validation error for negative limit")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

func TestVehicleUsecase_ListPositions_NegativeBounds(t *testing.T) {
	uc, _, _ := newTestVehicleUsecaseWithPositions(t)
	v, err := uc.Create(context.Background(), CreateVehicleInput{PlateNumber: "OK-4"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err = uc.ListPositions(context.Background(), v.ID, -1, 0, 0); err == nil || !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("from < 0 should be ErrValidation, got %v", err)
	}
	if _, err = uc.ListPositions(context.Background(), v.ID, 0, -1, 0); err == nil || !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("to < 0 should be ErrValidation, got %v", err)
	}
}

func TestVehicleUsecase_ListPositions_LimitClampedSilently(t *testing.T) {
	// limit > maxHistoryLimit must NOT error; it must clamp to
	// maxHistoryLimit and forward that ceiling to the repo. We assert via
	// the fake's behaviour by seeding (maxHistoryLimit+10) rows and
	// checking the call returns exactly maxHistoryLimit.
	uc, _, pl := newTestVehicleUsecaseWithPositions(t)
	v, err := uc.Create(context.Background(), CreateVehicleInput{PlateNumber: "BIG-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := 0; i < maxHistoryLimit+10; i++ {
		pl.add(&domain.Position{
			ID:         int64(i + 1),
			VehicleID:  v.ID,
			RecordedAt: int64(i + 1),
		})
	}
	got, err := uc.ListPositions(context.Background(), v.ID, 0, 0, maxHistoryLimit+100)
	if err != nil {
		t.Fatalf("ListPositions: %v", err)
	}
	if len(got) != maxHistoryLimit {
		t.Fatalf("expected clamped result of %d rows, got %d", maxHistoryLimit, len(got))
	}
}

func TestVehicleUsecase_ListPositions_DefaultLimit(t *testing.T) {
	// limit == 0 means "apply defaultHistoryLimit" — verify the cap
	// applies even when the fake holds many more rows.
	uc, _, pl := newTestVehicleUsecaseWithPositions(t)
	v, err := uc.Create(context.Background(), CreateVehicleInput{PlateNumber: "DFL-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := 0; i < defaultHistoryLimit+50; i++ {
		pl.add(&domain.Position{
			ID:         int64(i + 1),
			VehicleID:  v.ID,
			RecordedAt: int64(i + 1),
		})
	}
	got, err := uc.ListPositions(context.Background(), v.ID, 0, 0, 0)
	if err != nil {
		t.Fatalf("ListPositions: %v", err)
	}
	if len(got) != defaultHistoryLimit {
		t.Fatalf("expected default limit of %d, got %d", defaultHistoryLimit, len(got))
	}
}

func TestVehicleUsecase_ListPositions_ListerErrorPropagates(t *testing.T) {
	uc, _, pl := newTestVehicleUsecaseWithPositions(t)
	v, err := uc.Create(context.Background(), CreateVehicleInput{PlateNumber: "FAIL-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	pl.errOverride = errors.New("d1 outage")
	_, err = uc.ListPositions(context.Background(), v.ID, 0, 0, 0)
	if err == nil {
		t.Fatal("expected lister error to propagate")
	}
	if err.Error() != "d1 outage" {
		t.Errorf("error: got %q, want %q", err.Error(), "d1 outage")
	}
}
