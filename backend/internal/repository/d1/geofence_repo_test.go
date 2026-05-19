package d1

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
)

// freshSchemaForGeofences seeds an in-memory database migrated to the
// initial schema, with one driver and one vehicle so foreign-key
// constraints on the geofences table are satisfied. Mirrors the pattern
// in freshSchemaForPositions — small intentional duplication keeps each
// test file's setup story self-contained.
func freshSchemaForGeofences(t *testing.T) *sqliteExecutor {
	t.Helper()
	exec := newSQLiteExecutor(t)
	mig := NewMigrator(exec)
	if err := mig.Apply(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ctx := context.Background()
	now := time.Now().UTC().UnixMilli()
	if err := exec.Exec(ctx,
		`INSERT INTO drivers (id, email, password_hash, name, role, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"drv_1", "ada@example.com", "h", "Ada Lovelace", "driver", now, now,
	); err != nil {
		t.Fatalf("seed driver: %v", err)
	}
	if err := exec.Exec(ctx,
		`INSERT INTO vehicles (id, plate_number, model, driver_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"veh_1", "ABC-1234", "Hilux", "drv_1", now, now,
	); err != nil {
		t.Fatalf("seed vehicle: %v", err)
	}
	return exec
}

func sampleFence(vehicleID, id string) *domain.Geofence {
	return &domain.Geofence{
		ID:        id,
		VehicleID: vehicleID,
		CenterLat: 13.7563,
		CenterLng: 100.5018,
		RadiusM:   500,
		CreatedAt: 1_700_000_000_000,
	}
}

func TestGeofenceRepo_Upsert_Insert(t *testing.T) {
	exec := freshSchemaForGeofences(t)
	repo := NewGeofenceRepo(exec)

	g := sampleFence("veh_1", "fence_1")
	if err := repo.Upsert(context.Background(), g); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Verify the row is persisted with the right column values.
	var (
		gotID                 string
		gotVehicleID          string
		gotLat, gotLng        float64
		gotRadius, gotCreated int64
	)
	row := exec.QueryRow(context.Background(),
		`SELECT id, vehicle_id, center_lat, center_lng, radius_m, created_at
		 FROM geofences WHERE vehicle_id = ?`, "veh_1")
	if err := row.Scan(&gotID, &gotVehicleID, &gotLat, &gotLng, &gotRadius, &gotCreated); err != nil {
		t.Fatalf("readback: %v", err)
	}
	if gotID != "fence_1" {
		t.Errorf("ID: got %q, want fence_1", gotID)
	}
	if gotVehicleID != "veh_1" {
		t.Errorf("VehicleID: got %q, want veh_1", gotVehicleID)
	}
	if gotLat != 13.7563 || gotLng != 100.5018 {
		t.Errorf("center: got (%f, %f), want (13.7563, 100.5018)", gotLat, gotLng)
	}
	if gotRadius != 500 {
		t.Errorf("RadiusM: got %d, want 500", gotRadius)
	}
	if gotCreated != 1_700_000_000_000 {
		t.Errorf("CreatedAt: got %d, want 1_700_000_000_000", gotCreated)
	}
}

func TestGeofenceRepo_Upsert_ReplacesExisting(t *testing.T) {
	// The 1:1 invariant: a second Upsert for the same vehicle must
	// replace the prior fence, not add a second row.
	exec := freshSchemaForGeofences(t)
	repo := NewGeofenceRepo(exec)

	g1 := sampleFence("veh_1", "fence_a")
	if err := repo.Upsert(context.Background(), g1); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	g2 := &domain.Geofence{
		ID:        "fence_b",
		VehicleID: "veh_1",
		CenterLat: 14.0,
		CenterLng: 101.0,
		RadiusM:   1000,
		CreatedAt: 1_700_000_000_500,
	}
	if err := repo.Upsert(context.Background(), g2); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	// Exactly one row for the vehicle.
	var count int
	row := exec.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM geofences WHERE vehicle_id = ?`, "veh_1")
	if err := row.Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 row after upsert, got %d", count)
	}

	// The surviving row is the second one.
	got, err := repo.GetByVehicle(context.Background(), "veh_1")
	if err != nil {
		t.Fatalf("GetByVehicle: %v", err)
	}
	if got.ID != "fence_b" {
		t.Errorf("ID: got %q, want fence_b (the replacement)", got.ID)
	}
	if got.RadiusM != 1000 {
		t.Errorf("RadiusM: got %d, want 1000", got.RadiusM)
	}
}

func TestGeofenceRepo_GetByVehicle_Missing(t *testing.T) {
	exec := freshSchemaForGeofences(t)
	repo := NewGeofenceRepo(exec)

	_, err := repo.GetByVehicle(context.Background(), "veh_1")
	if err == nil {
		t.Fatal("expected error for missing fence")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestGeofenceRepo_GetByVehicle_RoundTrip(t *testing.T) {
	// After Upsert the same struct (minus pointer identity) must come
	// back via GetByVehicle — fields lossless across the round-trip.
	exec := freshSchemaForGeofences(t)
	repo := NewGeofenceRepo(exec)

	g := sampleFence("veh_1", "fence_rt")
	if err := repo.Upsert(context.Background(), g); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := repo.GetByVehicle(context.Background(), "veh_1")
	if err != nil {
		t.Fatalf("GetByVehicle: %v", err)
	}
	if got.ID != g.ID || got.VehicleID != g.VehicleID || got.CenterLat != g.CenterLat ||
		got.CenterLng != g.CenterLng || got.RadiusM != g.RadiusM || got.CreatedAt != g.CreatedAt {
		t.Fatalf("round-trip mismatch:\n got: %+v\nwant: %+v", got, g)
	}
}

func TestGeofenceRepo_Delete_Existing(t *testing.T) {
	exec := freshSchemaForGeofences(t)
	repo := NewGeofenceRepo(exec)

	if err := repo.Upsert(context.Background(), sampleFence("veh_1", "fence_1")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := repo.Delete(context.Background(), "veh_1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.GetByVehicle(context.Background(), "veh_1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got: %v", err)
	}
}

func TestGeofenceRepo_Delete_Idempotent(t *testing.T) {
	// Deleting a non-existent fence must succeed — the contract is
	// "ensure no fence exists for this vehicle", not "delete exactly
	// this row".
	exec := freshSchemaForGeofences(t)
	repo := NewGeofenceRepo(exec)

	if err := repo.Delete(context.Background(), "veh_1"); err != nil {
		t.Fatalf("Delete on empty: %v", err)
	}
	// Second call also a no-op.
	if err := repo.Delete(context.Background(), "veh_1"); err != nil {
		t.Fatalf("Delete second time: %v", err)
	}
}

func TestGeofenceRepo_Upsert_UnknownVehicle_FKViolation(t *testing.T) {
	// Foreign-key enforcement is on (newSQLiteExecutor sets it). An
	// upsert pointing at a non-existent vehicle must error.
	exec := freshSchemaForGeofences(t)
	repo := NewGeofenceRepo(exec)

	g := sampleFence("veh_does_not_exist", "fence_orphan")
	err := repo.Upsert(context.Background(), g)
	if err == nil {
		t.Fatal("expected FK violation for unknown vehicle_id")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Fatalf("expected foreign-key error message, got: %v", err)
	}
}

func TestGeofenceRepo_Upsert_CascadeOnVehicleDelete(t *testing.T) {
	// Schema declares ON DELETE CASCADE on geofences(vehicle_id).
	// Deleting the parent vehicle must drop the child fence.
	exec := freshSchemaForGeofences(t)
	repo := NewGeofenceRepo(exec)

	if err := repo.Upsert(context.Background(), sampleFence("veh_1", "fence_1")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := exec.Exec(context.Background(),
		`DELETE FROM vehicles WHERE id = ?`, "veh_1",
	); err != nil {
		t.Fatalf("delete vehicle: %v", err)
	}
	if _, err := repo.GetByVehicle(context.Background(), "veh_1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected cascade-delete to drop fence, got: %v", err)
	}
}

func TestGeofenceRepo_NilExecutor(t *testing.T) {
	// Each method must guard the nil-exec case rather than NPE.
	repo := &GeofenceRepo{}
	if _, err := repo.GetByVehicle(context.Background(), "v"); err == nil {
		t.Error("GetByVehicle on nil-exec repo should error")
	}
	if err := repo.Upsert(context.Background(), sampleFence("v", "f")); err == nil {
		t.Error("Upsert on nil-exec repo should error")
	}
	if err := repo.Delete(context.Background(), "v"); err == nil {
		t.Error("Delete on nil-exec repo should error")
	}
}

func TestGeofenceRepo_Upsert_NilGeofence(t *testing.T) {
	exec := freshSchemaForGeofences(t)
	repo := NewGeofenceRepo(exec)
	if err := repo.Upsert(context.Background(), nil); err == nil {
		t.Error("Upsert(nil) should error")
	}
}
