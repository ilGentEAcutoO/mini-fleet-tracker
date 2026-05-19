// Package d1 contains repositories and tooling that target Cloudflare D1.
//
// The migrator in this file is intentionally storage-agnostic: it talks to an
// Executor interface that both the real D1 HTTP client (pkg/cfclient.D1, added
// in TASK-005) and the in-memory sqlite3 test double in migrator_test.go can
// satisfy. This keeps the Go logic unit-testable without an HTTP round-trip
// while still exercising real SQL parsing and DDL against a SQLite engine.
package d1

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/migrations"
)

// upSuffix marks an "apply" migration. Down migrations live alongside but are
// not run by Apply — they exist as documentation and for manual rollback.
const upSuffix = ".up.sql"

// Executor is the minimal interface the migrator needs from a SQL backend.
// Both the real D1 HTTP client (pkg/cfclient.D1) and the sqlite3 test
// double in migrator_test.go satisfy this contract.
//
// Implementations are expected to handle multi-statement input in Exec;
// the in-process sqlite3 driver does not split statements automatically,
// so the test executor uses splitStatements() defined in this file. The
// D1 HTTP /query endpoint accepts multi-statement bodies natively.
//
// Query was added in TASK-010 to support repositories that need to scan
// multiple rows (e.g. VehicleRepo.List). The narrow contract is identical
// across backends: callers consume the iterator with Next + Scan, must
// honour the deferred error from Err, and must Close exactly once.
type Executor interface {
	Exec(ctx context.Context, sql string, args ...any) error
	QueryRow(ctx context.Context, sql string, args ...any) Row
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
}

// Row is the minimal interface for reading a single result row.
type Row interface {
	Scan(dest ...any) error
}

// Rows is the minimal interface for iterating a multi-row result set. The
// contract mirrors database/sql.Rows in shape so implementations can wrap
// either an *sql.Rows (the sqlite test executor) or an already-materialised
// slice of maps (the D1 HTTP client) without introducing a different
// idiom in caller code.
//
// Usage:
//
//	rows, err := exec.Query(ctx, "SELECT ... FROM t")
//	if err != nil { return err }
//	defer rows.Close()
//	for rows.Next() {
//	    if err := rows.Scan(&a, &b); err != nil { return err }
//	    // ... append to result ...
//	}
//	if err := rows.Err(); err != nil { return err }
type Rows interface {
	// Next advances to the next row. Returns false when no more rows are
	// available or when iteration encountered an error (check Err()).
	Next() bool
	// Scan binds destination pointers to the current row's columns.
	Scan(dest ...any) error
	// Err returns any deferred error from Next; nil on clean termination.
	Err() error
	// Close releases backend resources. Safe to call multiple times.
	Close() error
}

// Migrator applies pending SQL migrations from an embedded filesystem.
// The zero value is not usable; construct with NewMigrator or
// NewMigratorFromFS.
type Migrator struct {
	exec Executor
	fsys fs.FS
}

// NewMigrator returns a Migrator that reads from the embedded migrations.FS.
// This is the standard constructor for production code.
func NewMigrator(exec Executor) *Migrator {
	return &Migrator{exec: exec, fsys: migrations.FS}
}

// NewMigratorFromFS returns a Migrator backed by a caller-supplied filesystem.
// Intended for tests that want to inject a fixture or a deliberately broken
// migration; production code should use NewMigrator.
func NewMigratorFromFS(exec Executor, fsys fs.FS) *Migrator {
	return &Migrator{exec: exec, fsys: fsys}
}

// Apply runs all pending migrations in lexicographical order.
// Migration files are discovered from the embedded migrations.FS;
// files matching "*.up.sql" are eligible. Migrations already recorded
// in schema_migrations are skipped (idempotent).
//
// Errors during a migration leave schema_migrations untouched for that
// version; the operator should fix the SQL and re-run. No "dirty" flag
// is used — atomicity is provided by the executor's own transaction
// semantics, and the demo's blast radius is small enough that a manual
// re-run is acceptable.
func (m *Migrator) Apply(ctx context.Context) error {
	if m == nil || m.exec == nil {
		return errors.New("migrator: nil executor")
	}

	if err := m.ensureTrackingTable(ctx); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	applied, err := m.loadAppliedVersions(ctx)
	if err != nil {
		return fmt.Errorf("load applied versions: %w", err)
	}

	versions, err := m.discoverMigrations()
	if err != nil {
		return fmt.Errorf("discover migrations: %w", err)
	}

	for _, v := range versions {
		if _, ok := applied[v.version]; ok {
			log.Debug().Str("version", v.version).Msg("migration already applied; skipping")
			continue
		}

		body, err := fs.ReadFile(m.fsys, v.filename)
		if err != nil {
			return fmt.Errorf("read %s: %w", v.filename, err)
		}

		if err := m.exec.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", v.version, err)
		}

		now := time.Now().UTC().UnixMilli()
		if err := m.exec.Exec(ctx,
			"INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)",
			v.version, now,
		); err != nil {
			return fmt.Errorf("record %s in schema_migrations: %w", v.version, err)
		}

		log.Info().Str("version", v.version).Int64("applied_at_ms", now).Msg("migration applied")
	}

	return nil
}

// ensureTrackingTable creates schema_migrations if it does not already exist.
// The DDL is intentionally idempotent so first-run and subsequent-run paths
// share the same code.
func (m *Migrator) ensureTrackingTable(ctx context.Context) error {
	const ddl = `CREATE TABLE IF NOT EXISTS schema_migrations (
    version TEXT PRIMARY KEY,
    applied_at INTEGER NOT NULL
)`
	return m.exec.Exec(ctx, ddl)
}

// loadAppliedVersions returns the set of versions already recorded in
// schema_migrations. The membership map gives O(1) "already applied?" lookup.
func (m *Migrator) loadAppliedVersions(ctx context.Context) (map[string]struct{}, error) {
	// We cannot stream rows through the narrow Executor interface, so the
	// migrator instead asks the tracking table whether each candidate version
	// exists. discoverMigrations runs first to bound the set; here we just
	// pre-seed an empty map and let Apply consult QueryRow per candidate.
	//
	// In practice the candidate set is tiny (one migration today, low double
	// digits over the demo's lifetime), so N round-trips is fine and keeps
	// the Executor interface from needing a streaming Query method.
	versions, err := m.discoverMigrations()
	if err != nil {
		return nil, err
	}

	applied := make(map[string]struct{}, len(versions))
	for _, v := range versions {
		var found string
		row := m.exec.QueryRow(ctx,
			"SELECT version FROM schema_migrations WHERE version = ?",
			v.version,
		)
		err := row.Scan(&found)
		switch {
		case err == nil:
			applied[v.version] = struct{}{}
		case isNoRowsErr(err):
			// not applied yet; nothing to record
		default:
			return nil, fmt.Errorf("check version %s: %w", v.version, err)
		}
	}
	return applied, nil
}

// isNoRowsErr returns true when the underlying executor reports an empty
// result set. We match by error text to avoid leaking a database/sql import
// into the production package — callers may swap in a non-sql executor.
func isNoRowsErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no rows") || strings.Contains(msg, "sql: no rows in result set")
}

// migrationFile pairs a filename with its parsed version string.
type migrationFile struct {
	version  string // e.g. "000001_init"
	filename string // e.g. "000001_init.up.sql"
}

// discoverMigrations walks m.fsys and returns every *.up.sql file sorted
// lexicographically. Sort order matches filename order, which is why the
// project uses zero-padded numeric prefixes.
func (m *Migrator) discoverMigrations() ([]migrationFile, error) {
	entries, err := fs.ReadDir(m.fsys, ".")
	if err != nil {
		return nil, err
	}

	out := make([]migrationFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, upSuffix) {
			continue
		}
		out = append(out, migrationFile{
			version:  strings.TrimSuffix(name, upSuffix),
			filename: name,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// splitStatements breaks a multi-statement SQL string into individual,
// non-empty trimmed statements. It is a small, deliberately scoped scanner —
// not a full SQL parser — that recognises just enough syntax to avoid the
// classic pitfalls in our migration files:
//
//   - semicolons inside single- or double-quoted string literals are skipped
//   - "--" line comments are consumed to end-of-line (so a semicolon inside
//     a comment never triggers a split)
//   - "/* ... */" block comments are consumed (no semicolon escape there
//     either)
//
// The helper is package-local because the in-process sqlite3 test executor
// needs the same splitting logic that the D1 HTTP executor relies on the
// server to perform.
func splitStatements(sql string) []string {
	var (
		out      []string
		buf      strings.Builder
		inSingle bool
		inDouble bool
	)

	flush := func() {
		stmt := strings.TrimSpace(buf.String())
		buf.Reset()
		if stmt == "" {
			return
		}
		// Drop statements that are entirely SQL line comments or whitespace.
		onlyComments := true
		for _, line := range strings.Split(stmt, "\n") {
			t := strings.TrimSpace(line)
			if t == "" || strings.HasPrefix(t, "--") {
				continue
			}
			onlyComments = false
			break
		}
		if onlyComments {
			return
		}
		out = append(out, stmt)
	}

	i := 0
	for i < len(sql) {
		ch := sql[i]

		// Inside a quoted literal: only the matching quote (and a doubled
		// quote-escape) matters. SQLite uses '' to escape ' inside literals;
		// the same goes for "" inside double-quoted identifiers. The scanner
		// stays inside the literal otherwise.
		if inSingle {
			buf.WriteByte(ch)
			if ch == '\'' {
				if i+1 < len(sql) && sql[i+1] == '\'' {
					buf.WriteByte(sql[i+1])
					i += 2
					continue
				}
				inSingle = false
			}
			i++
			continue
		}
		if inDouble {
			buf.WriteByte(ch)
			if ch == '"' {
				if i+1 < len(sql) && sql[i+1] == '"' {
					buf.WriteByte(sql[i+1])
					i += 2
					continue
				}
				inDouble = false
			}
			i++
			continue
		}

		// Outside literals: detect comment openers BEFORE writing the char so
		// the comment body is preserved in the buffer (SQLite tolerates it)
		// but its semicolons never reach the splitter.
		if ch == '-' && i+1 < len(sql) && sql[i+1] == '-' {
			// Line comment: copy verbatim up to and including the newline.
			for i < len(sql) && sql[i] != '\n' {
				buf.WriteByte(sql[i])
				i++
			}
			continue
		}
		if ch == '/' && i+1 < len(sql) && sql[i+1] == '*' {
			// Block comment: copy verbatim through the closing */.
			buf.WriteByte(sql[i])
			buf.WriteByte(sql[i+1])
			i += 2
			for i < len(sql) {
				if sql[i] == '*' && i+1 < len(sql) && sql[i+1] == '/' {
					buf.WriteByte(sql[i])
					buf.WriteByte(sql[i+1])
					i += 2
					break
				}
				buf.WriteByte(sql[i])
				i++
			}
			continue
		}

		switch ch {
		case '\'':
			inSingle = true
			buf.WriteByte(ch)
		case '"':
			inDouble = true
			buf.WriteByte(ch)
		case ';':
			flush()
		default:
			buf.WriteByte(ch)
		}
		i++
	}
	flush()
	return out
}
