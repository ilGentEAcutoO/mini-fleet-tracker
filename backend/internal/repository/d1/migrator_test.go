package d1

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	_ "modernc.org/sqlite"
)

// sqliteExecutor adapts a database/sql.DB to the migrator's Executor
// interface. It exists purely for tests; the production code path uses the
// D1 HTTP client. The implementation mirrors what the real D1 backend does
// natively: split a multi-statement payload on top-level semicolons and exec
// each statement in turn so a single CREATE TABLE ... ; CREATE INDEX ... ;
// migration applies atomically from the migrator's point of view.
type sqliteExecutor struct {
	db *sql.DB
}

func (s *sqliteExecutor) Exec(ctx context.Context, query string, args ...any) error {
	// Single statements with placeholders are passed through verbatim so that
	// the migrator's INSERT INTO schema_migrations (..., ?, ?) call works.
	// Multi-statement payloads (the migration bodies) are split and exec'd
	// without args — our DDL has no placeholders.
	stmts := splitStatements(query)
	if len(stmts) <= 1 {
		_, err := s.db.ExecContext(ctx, query, args...)
		return err
	}
	if len(args) > 0 {
		return fmt.Errorf("sqliteExecutor: refusing to bind args across %d statements", len(stmts))
	}
	for i, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("statement %d/%d: %w", i+1, len(stmts), err)
		}
	}
	return nil
}

func (s *sqliteExecutor) QueryRow(ctx context.Context, query string, args ...any) Row {
	return s.db.QueryRowContext(ctx, query, args...)
}

// Query satisfies the Executor.Query method added in TASK-010. The test
// double wraps the *sql.Rows returned by database/sql so callers see the
// same Rows contract that the production D1 client materialises from JSON.
func (s *sqliteExecutor) Query(ctx context.Context, query string, args ...any) (Rows, error) {
	r, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return &sqliteRows{rows: r}, nil
}

// sqliteRows adapts *sql.Rows to the package's Rows interface. The four
// methods forward verbatim — the only reason for the wrapper is to keep
// the concrete database/sql type from leaking through the interface.
type sqliteRows struct {
	rows *sql.Rows
}

func (r *sqliteRows) Next() bool                  { return r.rows.Next() }
func (r *sqliteRows) Scan(dest ...any) error      { return r.rows.Scan(dest...) }
func (r *sqliteRows) Err() error                  { return r.rows.Err() }
func (r *sqliteRows) Close() error                { return r.rows.Close() }

// newSQLiteExecutor opens a fresh in-memory SQLite database and enables
// foreign-key enforcement so the migrator's REFERENCES clauses are honored.
// The "?_pragma=foreign_keys(1)" DSN switch is specific to modernc.org/sqlite.
func newSQLiteExecutor(t *testing.T) *sqliteExecutor {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:?cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// modernc.org/sqlite with shared cache + multiple connections can race on
	// in-memory tables. Pin to a single connection for deterministic tests.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if cerr := db.Close(); cerr != nil {
			t.Logf("close sqlite: %v", cerr)
		}
	})
	return &sqliteExecutor{db: db}
}

func TestMigrator_Apply_FreshDatabase(t *testing.T) {
	exec := newSQLiteExecutor(t)
	mig := NewMigrator(exec)

	if err := mig.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// All four domain tables exist.
	for _, table := range []string{"drivers", "vehicles", "positions", "geofences", "schema_migrations"} {
		var name string
		row := exec.QueryRow(context.Background(),
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", table)
		if err := row.Scan(&name); err != nil {
			t.Fatalf("expected table %q to exist: %v", table, err)
		}
		if name != table {
			t.Fatalf("expected table %q, got %q", table, name)
		}
	}

	// The positions index exists too.
	var idx string
	row := exec.QueryRow(context.Background(),
		"SELECT name FROM sqlite_master WHERE type='index' AND name=?", "idx_positions_vehicle_time")
	if err := row.Scan(&idx); err != nil {
		t.Fatalf("expected idx_positions_vehicle_time index: %v", err)
	}

	// schema_migrations records exactly one row for the initial migration.
	var version string
	var appliedAt int64
	row = exec.QueryRow(context.Background(),
		"SELECT version, applied_at FROM schema_migrations")
	if err := row.Scan(&version, &appliedAt); err != nil {
		t.Fatalf("scan schema_migrations: %v", err)
	}
	if version != "000001_init" {
		t.Fatalf("expected version 000001_init, got %q", version)
	}
	// applied_at should be a recent unix-ms timestamp; allow generous slack
	// so a slow CI host does not flake the test.
	now := time.Now().UTC().UnixMilli()
	if appliedAt <= 0 || appliedAt > now+1_000 || appliedAt < now-60_000 {
		t.Fatalf("applied_at out of expected range: got %d, now=%d", appliedAt, now)
	}
}

func TestMigrator_Apply_Idempotent(t *testing.T) {
	exec := newSQLiteExecutor(t)
	mig := NewMigrator(exec)

	if err := mig.Apply(context.Background()); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	countBefore := countMigrations(t, exec)

	if err := mig.Apply(context.Background()); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	countAfter := countMigrations(t, exec)

	if countBefore != 1 || countAfter != 1 {
		t.Fatalf("expected exactly 1 row in schema_migrations across both runs, got before=%d after=%d",
			countBefore, countAfter)
	}
}

func TestMigrator_Apply_SQLCompilesCorrectly(t *testing.T) {
	exec := newSQLiteExecutor(t)
	mig := NewMigrator(exec)
	if err := mig.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	ctx := context.Background()
	now := time.Now().UTC().UnixMilli()

	// 1) Insert a manager driver — exercises the STRICT type rules and the
	// role CHECK constraint.
	if err := exec.Exec(ctx,
		`INSERT INTO drivers (id, email, password_hash, name, role, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"drv_01", "ada@example.com", "hash$argon2", "Ada Lovelace", "manager", now, now,
	); err != nil {
		t.Fatalf("insert driver: %v", err)
	}

	// 2) Inserting an invalid role must be rejected by the CHECK constraint.
	err := exec.Exec(ctx,
		`INSERT INTO drivers (id, email, password_hash, name, role, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"drv_02", "bad@example.com", "h", "Bad Role", "admin", now, now,
	)
	if err == nil {
		t.Fatal("expected CHECK constraint to reject role='admin'")
	}

	// 3) Insert a vehicle owned by the driver, then a position, then verify
	// the foreign-key cascade by deleting the vehicle.
	if err := exec.Exec(ctx,
		`INSERT INTO vehicles (id, plate_number, model, driver_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"veh_01", "ABC-1234", "Toyota Hilux", "drv_01", now, now,
	); err != nil {
		t.Fatalf("insert vehicle: %v", err)
	}

	if err := exec.Exec(ctx,
		`INSERT INTO positions (vehicle_id, lat, lng, speed_kmh, recorded_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"veh_01", 13.7563, 100.5018, 42.5, now, now,
	); err != nil {
		t.Fatalf("insert position: %v", err)
	}

	if err := exec.Exec(ctx, "DELETE FROM vehicles WHERE id = ?", "veh_01"); err != nil {
		t.Fatalf("delete vehicle: %v", err)
	}

	var remaining int
	row := exec.QueryRow(ctx, "SELECT COUNT(*) FROM positions WHERE vehicle_id = ?", "veh_01")
	if err := row.Scan(&remaining); err != nil {
		t.Fatalf("count positions: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("expected ON DELETE CASCADE to remove positions, got %d remaining", remaining)
	}
}

func TestMigrator_Apply_ErrorPropagation(t *testing.T) {
	exec := newSQLiteExecutor(t)

	brokenFS := fstest.MapFS{
		"999999_broken.up.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE ::: not valid sql"),
		},
	}
	mig := NewMigratorFromFS(exec, brokenFS)

	err := mig.Apply(context.Background())
	if err == nil {
		t.Fatal("expected Apply to fail on invalid SQL, got nil")
	}
	if !strings.Contains(err.Error(), "apply 999999_broken") {
		t.Fatalf("expected error to wrap the migration version, got: %v", err)
	}

	// schema_migrations exists (ensureTrackingTable runs first) but must NOT
	// have a row for the broken migration.
	var version string
	row := exec.QueryRow(context.Background(),
		"SELECT version FROM schema_migrations WHERE version = ?", "999999_broken")
	scanErr := row.Scan(&version)
	if scanErr == nil {
		t.Fatalf("schema_migrations should not record a failed migration; got version=%q", version)
	}
	if !errors.Is(scanErr, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows for missing version, got: %v", scanErr)
	}
}

func TestMigrator_Apply_NilExecutor(t *testing.T) {
	mig := &Migrator{}
	if err := mig.Apply(context.Background()); err == nil {
		t.Fatal("expected error from nil executor")
	}
}

func TestMigrator_DiscoverMigrations_SortsLexicographically(t *testing.T) {
	fsys := fstest.MapFS{
		"000003_third.up.sql":  &fstest.MapFile{Data: []byte("SELECT 1;")},
		"000001_first.up.sql":  &fstest.MapFile{Data: []byte("SELECT 1;")},
		"000002_second.up.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
		"000004_unused.down.sql": &fstest.MapFile{
			Data: []byte("DROP TABLE x;"),
		},
		"README.txt": &fstest.MapFile{Data: []byte("ignored")},
	}
	mig := NewMigratorFromFS(nil, fsys)
	got, err := mig.discoverMigrations()
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	wantVersions := []string{"000001_first", "000002_second", "000003_third"}
	if len(got) != len(wantVersions) {
		t.Fatalf("expected %d migrations, got %d (%v)", len(wantVersions), len(got), got)
	}
	for i, v := range wantVersions {
		if got[i].version != v {
			t.Fatalf("position %d: want %q, got %q", i, v, got[i].version)
		}
	}
}

func TestSplitStatements(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "two simple statements",
			in:   "CREATE TABLE a (x INT); CREATE TABLE b (y INT);",
			want: []string{"CREATE TABLE a (x INT)", "CREATE TABLE b (y INT)"},
		},
		{
			name: "trailing whitespace and empty statement",
			in:   "SELECT 1;  ;  SELECT 2;",
			want: []string{"SELECT 1", "SELECT 2"},
		},
		{
			name: "semicolon inside single-quoted literal is preserved",
			in:   "INSERT INTO t VALUES ('a;b'); SELECT 2;",
			want: []string{"INSERT INTO t VALUES ('a;b')", "SELECT 2"},
		},
		{
			name: "pure comment statement is dropped",
			in:   "-- a comment\n;\nSELECT 1;",
			want: []string{"SELECT 1"},
		},
		{
			name: "semicolon inside line comment is not a split",
			in:   "-- this; comment has a semicolon\nSELECT 1; SELECT 2;",
			want: []string{"-- this; comment has a semicolon\nSELECT 1", "SELECT 2"},
		},
		{
			name: "semicolon inside block comment is not a split",
			in:   "/* a; b; c */ SELECT 1; SELECT 2;",
			want: []string{"/* a; b; c */ SELECT 1", "SELECT 2"},
		},
		{
			name: "doubled single quote escape inside literal",
			in:   "INSERT INTO t VALUES ('it''s; fine'); SELECT 2;",
			want: []string{"INSERT INTO t VALUES ('it''s; fine')", "SELECT 2"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := splitStatements(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len mismatch: want %d %v, got %d %v", len(tc.want), tc.want, len(got), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("statement %d: want %q, got %q", i, tc.want[i], got[i])
				}
			}
		})
	}
}

func countMigrations(t *testing.T, exec *sqliteExecutor) int {
	t.Helper()
	var n int
	row := exec.QueryRow(context.Background(), "SELECT COUNT(*) FROM schema_migrations")
	if err := row.Scan(&n); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	return n
}

