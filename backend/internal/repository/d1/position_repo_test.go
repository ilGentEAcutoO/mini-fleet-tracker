package d1

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
)

// freshSchemaForPositions sets up the migrated in-memory database and
// seeds one driver + one vehicle so position inserts have a valid FK
// target. Returns the executor for additional assertions.
func freshSchemaForPositions(t *testing.T) *sqliteExecutor {
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

func samplePosition(vehicleID string) *domain.Position {
	return &domain.Position{
		VehicleID:  vehicleID,
		Lat:        13.7563,
		Lng:        100.5018,
		SpeedKmh:   42.5,
		RecordedAt: 1_700_000_000_000,
	}
}

func TestPositionRepo_Insert_Success(t *testing.T) {
	exec := freshSchemaForPositions(t)
	repo := NewPositionRepo(exec)
	// Pin the clock so CreatedAt is deterministic.
	repo.now = func() time.Time { return time.UnixMilli(1_700_000_000_500) }

	p := samplePosition("veh_1")
	if err := repo.Insert(context.Background(), p); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if p.ID == 0 {
		t.Fatal("Insert must populate p.ID from the DB autoincrement")
	}
	if p.CreatedAt != 1_700_000_000_500 {
		t.Fatalf("CreatedAt: got %d, want 1_700_000_000_500", p.CreatedAt)
	}

	// Verify the row is persisted with the right column values.
	var (
		gotLat, gotLng    float64
		gotSpeed          float64
		gotRecorded       int64
		gotCreated        int64
		gotVehicleID      string
	)
	row := exec.QueryRow(context.Background(),
		`SELECT vehicle_id, lat, lng, speed_kmh, recorded_at, created_at
		 FROM positions WHERE id = ?`,
		p.ID)
	if err := row.Scan(&gotVehicleID, &gotLat, &gotLng, &gotSpeed, &gotRecorded, &gotCreated); err != nil {
		t.Fatalf("readback: %v", err)
	}
	if gotVehicleID != "veh_1" {
		t.Errorf("VehicleID: got %q, want veh_1", gotVehicleID)
	}
	if gotLat != 13.7563 || gotLng != 100.5018 {
		t.Errorf("coords: got (%f, %f), want (13.7563, 100.5018)", gotLat, gotLng)
	}
	if gotSpeed != 42.5 {
		t.Errorf("SpeedKmh: got %f, want 42.5", gotSpeed)
	}
	if gotRecorded != 1_700_000_000_000 {
		t.Errorf("RecordedAt: got %d, want 1_700_000_000_000", gotRecorded)
	}
	if gotCreated != 1_700_000_000_500 {
		t.Errorf("CreatedAt: got %d, want 1_700_000_000_500", gotCreated)
	}
}

func TestPositionRepo_Insert_AutoIncrementsID(t *testing.T) {
	exec := freshSchemaForPositions(t)
	repo := NewPositionRepo(exec)

	p1 := samplePosition("veh_1")
	if err := repo.Insert(context.Background(), p1); err != nil {
		t.Fatalf("Insert 1: %v", err)
	}
	p2 := samplePosition("veh_1")
	if err := repo.Insert(context.Background(), p2); err != nil {
		t.Fatalf("Insert 2: %v", err)
	}
	if p2.ID <= p1.ID {
		t.Fatalf("autoincrement broken: p1.ID=%d, p2.ID=%d", p1.ID, p2.ID)
	}
}

func TestPositionRepo_Insert_ZeroSpeedStoredAsNull(t *testing.T) {
	// The repo maps Go zero (0.0) to SQL NULL so the column stays
	// semantically "unset". Verify by reading back via a NULL check.
	exec := freshSchemaForPositions(t)
	repo := NewPositionRepo(exec)

	p := samplePosition("veh_1")
	p.SpeedKmh = 0
	if err := repo.Insert(context.Background(), p); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	var isNull int
	row := exec.QueryRow(context.Background(),
		`SELECT CASE WHEN speed_kmh IS NULL THEN 1 ELSE 0 END FROM positions WHERE id = ?`,
		p.ID)
	if err := row.Scan(&isNull); err != nil {
		t.Fatalf("read NULL check: %v", err)
	}
	if isNull != 1 {
		t.Fatal("zero SpeedKmh should be stored as SQL NULL")
	}
}

func TestPositionRepo_Insert_NonZeroSpeedStored(t *testing.T) {
	exec := freshSchemaForPositions(t)
	repo := NewPositionRepo(exec)

	p := samplePosition("veh_1")
	p.SpeedKmh = 88.0
	if err := repo.Insert(context.Background(), p); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	var got float64
	row := exec.QueryRow(context.Background(),
		`SELECT speed_kmh FROM positions WHERE id = ?`, p.ID)
	if err := row.Scan(&got); err != nil {
		t.Fatalf("readback: %v", err)
	}
	if got != 88.0 {
		t.Fatalf("SpeedKmh: got %f, want 88.0", got)
	}
}

func TestPositionRepo_Insert_FKViolationOnUnknownVehicle(t *testing.T) {
	// Foreign-key enforcement is on (newSQLiteExecutor sets it). An
	// insert pointing at a non-existent vehicle must error.
	exec := freshSchemaForPositions(t)
	repo := NewPositionRepo(exec)

	p := samplePosition("veh_does_not_exist")
	err := repo.Insert(context.Background(), p)
	if err == nil {
		t.Fatal("expected FK violation for unknown vehicle_id")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Fatalf("expected foreign-key error message, got: %v", err)
	}
}

func TestPositionRepo_Insert_NilExecutor(t *testing.T) {
	repo := &PositionRepo{}
	if err := repo.Insert(context.Background(), samplePosition("v")); err == nil {
		t.Fatal("Insert on nil-exec repo should error")
	}
}

func TestPositionRepo_Insert_NilPosition(t *testing.T) {
	exec := freshSchemaForPositions(t)
	repo := NewPositionRepo(exec)
	if err := repo.Insert(context.Background(), nil); err == nil {
		t.Fatal("Insert(nil) should error")
	}
}

// seedPositions inserts n positions with monotonically-increasing
// recorded_at timestamps starting at baseMs (10s apart). Returns the
// position IDs in the order they were inserted (so tests can assert on
// the DESC ordering of the list result).
func seedPositions(t *testing.T, exec *sqliteExecutor, vehicleID string, n int, baseMs int64) []int64 {
	t.Helper()
	repo := NewPositionRepo(exec)
	out := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		p := &domain.Position{
			VehicleID:  vehicleID,
			Lat:        13.7563 + float64(i)*0.001,
			Lng:        100.5018 + float64(i)*0.001,
			SpeedKmh:   float64(40 + i),
			RecordedAt: baseMs + int64(i)*10_000,
		}
		if err := repo.Insert(context.Background(), p); err != nil {
			t.Fatalf("seed insert %d: %v", i, err)
		}
		out = append(out, p.ID)
	}
	return out
}

func TestPositionRepo_ListByVehicleAndRange_EmptySliceWhenNoMatch(t *testing.T) {
	// A vehicle with no positions should return an empty (non-nil) slice.
	exec := freshSchemaForPositions(t)
	repo := NewPositionRepo(exec)
	got, err := repo.ListByVehicleAndRange(context.Background(), "veh_1", 0, 0, 100)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil empty slice, got nil")
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 rows, got %d", len(got))
	}
}

func TestPositionRepo_ListByVehicleAndRange_OrdersDescByRecordedAt(t *testing.T) {
	// Insert in ascending order; expect the result in descending order.
	exec := freshSchemaForPositions(t)
	ids := seedPositions(t, exec, "veh_1", 5, 1_700_000_000_000)
	// ids are in insert order (ascending recorded_at); reverse-expected.
	wantOrder := []int64{ids[4], ids[3], ids[2], ids[1], ids[0]}

	repo := NewPositionRepo(exec)
	got, err := repo.ListByVehicleAndRange(context.Background(), "veh_1", 0, 0, 100)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("expected 5 rows, got %d", len(got))
	}
	for i, p := range got {
		if p.ID != wantOrder[i] {
			t.Errorf("position %d: got ID=%d, want %d", i, p.ID, wantOrder[i])
		}
	}
}

func TestPositionRepo_ListByVehicleAndRange_AppliesFromBound(t *testing.T) {
	exec := freshSchemaForPositions(t)
	base := int64(1_700_000_000_000)
	_ = seedPositions(t, exec, "veh_1", 5, base) // recorded_at: base, base+10k, +20k, +30k, +40k

	repo := NewPositionRepo(exec)
	// From base+20k onwards = 3 rows (recorded_at == 20k, 30k, 40k).
	got, err := repo.ListByVehicleAndRange(context.Background(), "veh_1", base+20_000, 0, 100)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(got))
	}
	for _, p := range got {
		if p.RecordedAt < base+20_000 {
			t.Errorf("position recordedAt %d below lower bound %d", p.RecordedAt, base+20_000)
		}
	}
}

func TestPositionRepo_ListByVehicleAndRange_AppliesToBound(t *testing.T) {
	exec := freshSchemaForPositions(t)
	base := int64(1_700_000_000_000)
	_ = seedPositions(t, exec, "veh_1", 5, base)

	repo := NewPositionRepo(exec)
	// Up to base+20k = 3 rows (recorded_at == 0, 10k, 20k).
	got, err := repo.ListByVehicleAndRange(context.Background(), "veh_1", 0, base+20_000, 100)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(got))
	}
	for _, p := range got {
		if p.RecordedAt > base+20_000 {
			t.Errorf("position recordedAt %d above upper bound %d", p.RecordedAt, base+20_000)
		}
	}
}

func TestPositionRepo_ListByVehicleAndRange_AppliesBothBounds(t *testing.T) {
	exec := freshSchemaForPositions(t)
	base := int64(1_700_000_000_000)
	_ = seedPositions(t, exec, "veh_1", 5, base)

	repo := NewPositionRepo(exec)
	// [base+10k, base+30k] = 3 rows.
	got, err := repo.ListByVehicleAndRange(context.Background(), "veh_1", base+10_000, base+30_000, 100)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(got))
	}
}

func TestPositionRepo_ListByVehicleAndRange_AppliesLimit(t *testing.T) {
	exec := freshSchemaForPositions(t)
	_ = seedPositions(t, exec, "veh_1", 5, 1_700_000_000_000)

	repo := NewPositionRepo(exec)
	got, err := repo.ListByVehicleAndRange(context.Background(), "veh_1", 0, 0, 2)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("limit=2 should return 2 rows, got %d", len(got))
	}
}

func TestPositionRepo_ListByVehicleAndRange_NonPositiveLimitMeansNoLimit(t *testing.T) {
	// limit <= 0 means "no row cap" — the caller decides at the HTTP
	// layer.
	exec := freshSchemaForPositions(t)
	_ = seedPositions(t, exec, "veh_1", 5, 1_700_000_000_000)

	repo := NewPositionRepo(exec)
	for _, lim := range []int{0, -1} {
		got, err := repo.ListByVehicleAndRange(context.Background(), "veh_1", 0, 0, lim)
		if err != nil {
			t.Fatalf("List(limit=%d): %v", lim, err)
		}
		if len(got) != 5 {
			t.Fatalf("limit=%d should return all 5 rows, got %d", lim, len(got))
		}
	}
}

func TestPositionRepo_ListByVehicleAndRange_FiltersByVehicleID(t *testing.T) {
	// Insert one vehicle; verify the list filter does NOT spill rows
	// from a sibling vehicle.
	exec := freshSchemaForPositions(t)
	// Add a second vehicle in the schema.
	ctx := context.Background()
	now := time.Now().UTC().UnixMilli()
	if err := exec.Exec(ctx,
		`INSERT INTO vehicles (id, plate_number, model, driver_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"veh_2", "XYZ-9999", "Sedan", "drv_1", now, now,
	); err != nil {
		t.Fatalf("seed second vehicle: %v", err)
	}

	_ = seedPositions(t, exec, "veh_1", 3, 1_700_000_000_000)
	_ = seedPositions(t, exec, "veh_2", 7, 1_700_000_000_000)

	repo := NewPositionRepo(exec)
	got, err := repo.ListByVehicleAndRange(context.Background(), "veh_1", 0, 0, 100)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 rows for veh_1, got %d", len(got))
	}
	for _, p := range got {
		if p.VehicleID != "veh_1" {
			t.Errorf("list leaked sibling vehicle row: %+v", p)
		}
	}
}

func TestPositionRepo_ListByVehicleAndRange_HandlesNullSpeedKmh(t *testing.T) {
	// A position inserted with SpeedKmh=0 stores SQL NULL; the list
	// scan must surface that back as Go zero (0.0), not error out.
	exec := freshSchemaForPositions(t)
	repo := NewPositionRepo(exec)

	zero := samplePosition("veh_1")
	zero.SpeedKmh = 0
	if err := repo.Insert(context.Background(), zero); err != nil {
		t.Fatalf("Insert (null speed): %v", err)
	}

	got, err := repo.ListByVehicleAndRange(context.Background(), "veh_1", 0, 0, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 row, got %d", len(got))
	}
	if got[0].SpeedKmh != 0 {
		t.Errorf("null speed must scan back as zero, got %f", got[0].SpeedKmh)
	}
}

func TestPositionRepo_ListByVehicleAndRange_NilExecutor(t *testing.T) {
	repo := &PositionRepo{}
	_, err := repo.ListByVehicleAndRange(context.Background(), "v", 0, 0, 0)
	if err == nil {
		t.Fatal("ListByVehicleAndRange on nil-exec repo should error")
	}
}
