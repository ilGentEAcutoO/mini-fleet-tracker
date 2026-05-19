package d1

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
)

// DriverRepo is the D1-backed implementation of the driver storage
// interface declared at the consumer site (internal/usecase). It depends
// only on the narrow Executor contract — the in-memory sqlite3 test
// double used in the migrator tests satisfies it just as well as the
// real pkg/cfclient.D1Client does in production.
type DriverRepo struct {
	exec Executor
}

// NewDriverRepo constructs a repository bound to exec. exec is captured
// verbatim; callers are responsible for its lifecycle and concurrency
// guarantees (both real implementations are safe for shared use).
func NewDriverRepo(exec Executor) *DriverRepo {
	return &DriverRepo{exec: exec}
}

// driverColumnList is the canonical SELECT projection for the drivers
// table, ordered so it matches the alphabetical scan order produced by
// the D1 client's QueryRow. The migrator's in-memory sqlite test double
// returns columns in declared order, but our production D1 client
// alphabetises them (see pkg/cfclient.d1Row.Scan); aligning the SELECT
// list with the alphabetical order means a single Scan binding works
// for both backends.
const driverColumnList = "created_at, email, id, name, password_hash, role, updated_at"

// scanDriver reads a Row in the column order documented by driverColumnList.
// Pulled out so Create-after-Insert lookups and GetBy* paths share the
// same destination layout.
func scanDriver(row Row, d *domain.Driver) error {
	var roleStr string
	if err := row.Scan(
		&d.CreatedAt,
		&d.Email,
		&d.ID,
		&d.Name,
		&d.PasswordHash,
		&roleStr,
		&d.UpdatedAt,
	); err != nil {
		return err
	}
	d.Role = domain.Role(roleStr)
	return nil
}

// Create inserts a new driver row. The caller supplies an already-hashed
// password — the repository never sees plaintext.
//
// D1/SQLite returns an error whose text contains
//
//	UNIQUE constraint failed: drivers.email
//
// when the email column's UNIQUE index is violated. We translate that
// magic substring into domain.ErrAlreadyExists so the usecase layer can
// errors.Is the result without importing storage-specific symbols. The
// match is deliberately conservative (one well-known substring, no
// regex) and is exercised by the matching unit test.
func (r *DriverRepo) Create(ctx context.Context, d *domain.Driver) error {
	if r == nil || r.exec == nil {
		return errors.New("driver repo: nil executor")
	}
	if d == nil {
		return errors.New("driver repo: nil driver")
	}

	err := r.exec.Exec(ctx,
		`INSERT INTO drivers (id, email, password_hash, name, role, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.Email, d.PasswordHash, d.Name, string(d.Role), d.CreatedAt, d.UpdatedAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("driver email %s: %w", d.Email, domain.ErrAlreadyExists)
		}
		return fmt.Errorf("driver repo: insert: %w", err)
	}
	return nil
}

// GetByEmail returns the driver row with the given email, or
// domain.ErrNotFound when there is no such row. Used by Login.
func (r *DriverRepo) GetByEmail(ctx context.Context, email string) (*domain.Driver, error) {
	if r == nil || r.exec == nil {
		return nil, errors.New("driver repo: nil executor")
	}
	row := r.exec.QueryRow(ctx,
		`SELECT `+driverColumnList+` FROM drivers WHERE email = ?`,
		email,
	)
	var d domain.Driver
	if err := scanDriver(row, &d); err != nil {
		if isNoRowsErr(err) {
			return nil, fmt.Errorf("driver email %s: %w", email, domain.ErrNotFound)
		}
		return nil, fmt.Errorf("driver repo: get-by-email: %w", err)
	}
	return &d, nil
}

// GetByID returns the driver row with the given UUID, or
// domain.ErrNotFound. Used by /api/auth/me.
func (r *DriverRepo) GetByID(ctx context.Context, id string) (*domain.Driver, error) {
	if r == nil || r.exec == nil {
		return nil, errors.New("driver repo: nil executor")
	}
	row := r.exec.QueryRow(ctx,
		`SELECT `+driverColumnList+` FROM drivers WHERE id = ?`,
		id,
	)
	var d domain.Driver
	if err := scanDriver(row, &d); err != nil {
		if isNoRowsErr(err) {
			return nil, fmt.Errorf("driver id %s: %w", id, domain.ErrNotFound)
		}
		return nil, fmt.Errorf("driver repo: get-by-id: %w", err)
	}
	return &d, nil
}

// isUniqueViolation reports whether err looks like a SQLite UNIQUE
// constraint failure on the drivers.email column. Matching by error text
// is brittle in principle — SQLite versions have varied the wording — but
// modernc.org/sqlite (which we use in tests) and D1 (which uses libsqlite
// under the hood) both emit the canonical
//
//	UNIQUE constraint failed: drivers.email
//
// message today. If a future SQLite release changes the wording, the
// matching unit test in driver_repo_test.go will fail loudly and the
// substring set below is the single place to update.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// Accept both the exact column-qualified message and a more permissive
	// fallback in case D1 ever rewrites the error envelope.
	if strings.Contains(msg, "UNIQUE constraint failed: drivers.email") {
		return true
	}
	if strings.Contains(msg, "UNIQUE constraint failed") && strings.Contains(msg, "email") {
		return true
	}
	return false
}
