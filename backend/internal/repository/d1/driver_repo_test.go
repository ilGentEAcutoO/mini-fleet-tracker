package d1

import (
	"context"
	"errors"
	"testing"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
)

// freshSchemaExecutor returns an in-memory sqlite executor with the live
// migrations applied. Tests for the driver repo all share the same setup,
// so we keep this helper instead of duplicating Apply() in every test.
func freshSchemaExecutor(t *testing.T) *sqliteExecutor {
	t.Helper()
	exec := newSQLiteExecutor(t)
	mig := NewMigrator(exec)
	if err := mig.Apply(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return exec
}

func sampleDriver(id, email string) *domain.Driver {
	return &domain.Driver{
		ID:           id,
		Email:        email,
		PasswordHash: "$argon2id$v=19$m=65536,t=3,p=2$saltsaltsaltsalt$hashhashhashhashhashhashhashhash",
		Name:         "Sample Person",
		Role:         domain.RoleDriver,
		CreatedAt:    1_700_000_000_000,
		UpdatedAt:    1_700_000_000_000,
	}
}

func TestDriverRepo_Create_Success(t *testing.T) {
	exec := freshSchemaExecutor(t)
	repo := NewDriverRepo(exec)

	d := sampleDriver("drv_01", "ada@example.com")
	if err := repo.Create(context.Background(), d); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The row should now be retrievable via GetByEmail with identical fields.
	got, err := repo.GetByEmail(context.Background(), "ada@example.com")
	if err != nil {
		t.Fatalf("GetByEmail: %v", err)
	}
	if got.ID != d.ID || got.Email != d.Email || got.PasswordHash != d.PasswordHash ||
		got.Name != d.Name || got.Role != d.Role || got.CreatedAt != d.CreatedAt || got.UpdatedAt != d.UpdatedAt {
		t.Fatalf("roundtrip drift: got %+v, want %+v", got, d)
	}
}

func TestDriverRepo_Create_DuplicateEmailMappedToErrAlreadyExists(t *testing.T) {
	exec := freshSchemaExecutor(t)
	repo := NewDriverRepo(exec)

	first := sampleDriver("drv_01", "ada@example.com")
	if err := repo.Create(context.Background(), first); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	// Different ID, same email — must collide on the UNIQUE index.
	second := sampleDriver("drv_02", "ada@example.com")
	err := repo.Create(context.Background(), second)
	if err == nil {
		t.Fatal("expected second Create with duplicate email to fail")
	}
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists, got: %v", err)
	}
}

func TestDriverRepo_GetByEmail_NotFound(t *testing.T) {
	exec := freshSchemaExecutor(t)
	repo := NewDriverRepo(exec)

	_, err := repo.GetByEmail(context.Background(), "missing@example.com")
	if err == nil {
		t.Fatal("expected error on missing email")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestDriverRepo_GetByID_NotFound(t *testing.T) {
	exec := freshSchemaExecutor(t)
	repo := NewDriverRepo(exec)

	_, err := repo.GetByID(context.Background(), "drv_does_not_exist")
	if err == nil {
		t.Fatal("expected error on missing id")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestDriverRepo_GetByID_Success(t *testing.T) {
	exec := freshSchemaExecutor(t)
	repo := NewDriverRepo(exec)

	d := sampleDriver("drv_42", "linus@example.com")
	d.Role = domain.RoleManager
	d.Name = "Linus Torvalds"
	if err := repo.Create(context.Background(), d); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.GetByID(context.Background(), "drv_42")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Role != domain.RoleManager {
		t.Fatalf("Role: want manager, got %q", got.Role)
	}
	if got.Name != "Linus Torvalds" {
		t.Fatalf("Name: got %q", got.Name)
	}
}

// TestIsUniqueViolation isolates the brittle string-match. If a future
// SQLite/D1 release changes the wording, the only required edit is in
// isUniqueViolation — and this test will tell you which strings still
// trip the match.
func TestIsUniqueViolation(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated error", errors.New("network down"), false},
		{"canonical sqlite/D1 message",
			errors.New("constraint failed: UNIQUE constraint failed: drivers.email (2067)"), true},
		{"generic UNIQUE on email", errors.New("UNIQUE constraint failed: some_table.email"), true},
		{"UNIQUE on a different column", errors.New("UNIQUE constraint failed: drivers.id"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUniqueViolation(tc.err); got != tc.want {
				t.Fatalf("isUniqueViolation(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestDriverRepo_NilExecutor guards against using the zero value of the
// struct — a constructor mistake that would otherwise surface as a nil
// pointer panic deep in the request path.
func TestDriverRepo_NilExecutor(t *testing.T) {
	repo := &DriverRepo{}
	if err := repo.Create(context.Background(), sampleDriver("x", "y@y")); err == nil {
		t.Fatal("Create on nil-exec repo should error")
	}
	if _, err := repo.GetByEmail(context.Background(), "a@b"); err == nil {
		t.Fatal("GetByEmail on nil-exec repo should error")
	}
	if _, err := repo.GetByID(context.Background(), "x"); err == nil {
		t.Fatal("GetByID on nil-exec repo should error")
	}
}
