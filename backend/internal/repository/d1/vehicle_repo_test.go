package d1

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
)

// sampleVehicle returns a vehicle in a state typical of a fresh Create —
// timestamps are populated, IDs are caller-supplied, optional fields can
// be overridden by tests as needed.
func sampleVehicle(id, plate string) *domain.Vehicle {
	now := time.Now().UTC().UnixMilli()
	return &domain.Vehicle{
		ID:          id,
		PlateNumber: plate,
		Model:       "Toyota Hilux",
		DriverID:    "", // unassigned by default; tests can fill in when they need an FK
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

func TestVehicleRepo_Create_RoundTrip(t *testing.T) {
	exec := freshSchemaExecutor(t)
	repo := NewVehicleRepo(exec)

	v := sampleVehicle("veh_01", "ABC-1234")
	if err := repo.Create(context.Background(), v); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.Get(context.Background(), "veh_01")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != v.ID || got.PlateNumber != v.PlateNumber || got.Model != v.Model ||
		got.DriverID != v.DriverID || got.CreatedAt != v.CreatedAt || got.UpdatedAt != v.UpdatedAt {
		t.Fatalf("roundtrip drift: got %+v, want %+v", got, v)
	}
}

func TestVehicleRepo_Create_DuplicatePlateMappedToErrAlreadyExists(t *testing.T) {
	exec := freshSchemaExecutor(t)
	repo := NewVehicleRepo(exec)

	first := sampleVehicle("veh_01", "ABC-1234")
	if err := repo.Create(context.Background(), first); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	second := sampleVehicle("veh_02", "ABC-1234")
	err := repo.Create(context.Background(), second)
	if err == nil {
		t.Fatal("expected duplicate plate to fail")
	}
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists, got: %v", err)
	}
}

func TestVehicleRepo_Get_NotFound(t *testing.T) {
	exec := freshSchemaExecutor(t)
	repo := NewVehicleRepo(exec)

	_, err := repo.Get(context.Background(), "veh_does_not_exist")
	if err == nil {
		t.Fatal("expected error on missing id")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestVehicleRepo_List_EmptyReturnsEmptySlice(t *testing.T) {
	exec := freshSchemaExecutor(t)
	repo := NewVehicleRepo(exec)

	got, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 vehicles, got %d", len(got))
	}
}

func TestVehicleRepo_List_OrdersByCreatedAtDesc(t *testing.T) {
	exec := freshSchemaExecutor(t)
	repo := NewVehicleRepo(exec)

	// Three vehicles with distinct created_at timestamps so the ordering
	// is unambiguous regardless of how SQLite would break ties.
	a := sampleVehicle("veh_a", "AAA-0001")
	a.CreatedAt = 1_700_000_000_000
	a.UpdatedAt = a.CreatedAt
	b := sampleVehicle("veh_b", "BBB-0002")
	b.CreatedAt = 1_700_000_001_000
	b.UpdatedAt = b.CreatedAt
	c := sampleVehicle("veh_c", "CCC-0003")
	c.CreatedAt = 1_700_000_002_000
	c.UpdatedAt = c.CreatedAt

	// Insert in non-sorted order to make sure List's ORDER BY does the
	// work, not the insertion order.
	for _, v := range []*domain.Vehicle{b, a, c} {
		if err := repo.Create(context.Background(), v); err != nil {
			t.Fatalf("Create %s: %v", v.ID, err)
		}
	}

	got, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 vehicles, got %d", len(got))
	}
	wantOrder := []string{"veh_c", "veh_b", "veh_a"}
	for i, want := range wantOrder {
		if got[i].ID != want {
			t.Fatalf("position %d: got %q, want %q (full list: %+v)", i, got[i].ID, want, idsOf(got))
		}
	}
}

func TestVehicleRepo_Update_ReflectsChanges(t *testing.T) {
	exec := freshSchemaExecutor(t)
	repo := NewVehicleRepo(exec)

	v := sampleVehicle("veh_01", "ABC-1234")
	if err := repo.Create(context.Background(), v); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Mutate plate, model, driver_id; bump updated_at.
	v.PlateNumber = "XYZ-9999"
	v.Model = "Ford Ranger"
	v.DriverID = "" // remains unassigned — exercises the NULL write path
	v.UpdatedAt = v.CreatedAt + 5_000
	if err := repo.Update(context.Background(), v); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := repo.Get(context.Background(), "veh_01")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.PlateNumber != "XYZ-9999" {
		t.Errorf("plate not updated: %q", got.PlateNumber)
	}
	if got.Model != "Ford Ranger" {
		t.Errorf("model not updated: %q", got.Model)
	}
	if got.UpdatedAt != v.UpdatedAt {
		t.Errorf("updated_at not bumped: got %d, want %d", got.UpdatedAt, v.UpdatedAt)
	}
}

func TestVehicleRepo_Update_NotFound(t *testing.T) {
	exec := freshSchemaExecutor(t)
	repo := NewVehicleRepo(exec)

	v := sampleVehicle("veh_does_not_exist", "ZZZ-0000")
	err := repo.Update(context.Background(), v)
	if err == nil {
		t.Fatal("expected error on missing id")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestVehicleRepo_Update_DuplicatePlate(t *testing.T) {
	exec := freshSchemaExecutor(t)
	repo := NewVehicleRepo(exec)

	a := sampleVehicle("veh_a", "AAA-0001")
	b := sampleVehicle("veh_b", "BBB-0002")
	if err := repo.Create(context.Background(), a); err != nil {
		t.Fatalf("Create a: %v", err)
	}
	if err := repo.Create(context.Background(), b); err != nil {
		t.Fatalf("Create b: %v", err)
	}

	// Try to rename b onto a's plate — must collide.
	b.PlateNumber = "AAA-0001"
	err := repo.Update(context.Background(), b)
	if err == nil {
		t.Fatal("expected duplicate-plate Update to fail")
	}
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists, got: %v", err)
	}
}

func TestVehicleRepo_Delete_Success(t *testing.T) {
	exec := freshSchemaExecutor(t)
	repo := NewVehicleRepo(exec)

	v := sampleVehicle("veh_01", "ABC-1234")
	if err := repo.Create(context.Background(), v); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.Delete(context.Background(), "veh_01"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := repo.Get(context.Background(), "veh_01")
	if err == nil {
		t.Fatal("expected Get after Delete to fail")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after Delete, got: %v", err)
	}
}

func TestVehicleRepo_Delete_NotFound(t *testing.T) {
	exec := freshSchemaExecutor(t)
	repo := NewVehicleRepo(exec)

	err := repo.Delete(context.Background(), "veh_does_not_exist")
	if err == nil {
		t.Fatal("expected error on missing id")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestVehicleRepo_NilExecutor(t *testing.T) {
	repo := &VehicleRepo{}
	if err := repo.Create(context.Background(), sampleVehicle("x", "y")); err == nil {
		t.Error("Create on nil-exec repo should error")
	}
	if _, err := repo.Get(context.Background(), "x"); err == nil {
		t.Error("Get on nil-exec repo should error")
	}
	if _, err := repo.List(context.Background()); err == nil {
		t.Error("List on nil-exec repo should error")
	}
	if err := repo.Update(context.Background(), sampleVehicle("x", "y")); err == nil {
		t.Error("Update on nil-exec repo should error")
	}
	if err := repo.Delete(context.Background(), "x"); err == nil {
		t.Error("Delete on nil-exec repo should error")
	}
}

func TestVehicleRepo_NullableFields(t *testing.T) {
	// Vehicles can be created with empty Model and empty DriverID — these
	// must round-trip through SQL NULL and return as empty strings.
	exec := freshSchemaExecutor(t)
	repo := NewVehicleRepo(exec)

	v := sampleVehicle("veh_01", "ABC-1234")
	v.Model = ""    // explicit empty — repository must persist NULL
	v.DriverID = "" // unassigned — repository must persist NULL
	if err := repo.Create(context.Background(), v); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := repo.Get(context.Background(), "veh_01")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Model != "" {
		t.Errorf("Model should round-trip as empty string, got %q", got.Model)
	}
	if got.DriverID != "" {
		t.Errorf("DriverID should round-trip as empty string, got %q", got.DriverID)
	}
}

// TestIsVehiclePlateUnique pins the brittle string-match. If the SQLite/D1
// error wording ever changes, this test surfaces the drift loudly.
func TestIsVehiclePlateUnique(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("network down"), false},
		{"canonical sqlite/D1 message",
			errors.New("constraint failed: UNIQUE constraint failed: vehicles.plate_number (2067)"), true},
		{"generic UNIQUE on plate_number",
			errors.New("UNIQUE constraint failed: some_table.plate_number"), true},
		{"UNIQUE on a different column", errors.New("UNIQUE constraint failed: vehicles.id"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isVehiclePlateUnique(tc.err); got != tc.want {
				t.Fatalf("isVehiclePlateUnique(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// idsOf is a debug helper for test failure messages — keeps the assertion
// output readable when List ordering goes wrong.
func idsOf(vs []*domain.Vehicle) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.ID)
	}
	return out
}
