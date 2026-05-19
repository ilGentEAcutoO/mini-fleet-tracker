package domain_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
)

func TestRole_Valid(t *testing.T) {
	tests := []struct {
		name string
		role domain.Role
		want bool
	}{
		{"driver", domain.RoleDriver, true},
		{"manager", domain.RoleManager, true},
		{"empty", "", false},
		{"admin (not a recognised role)", "admin", false},
		{"capitalised driver", "Driver", false},
		{"trailing space", "driver ", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.role.Valid(); got != tc.want {
				t.Fatalf("Role(%q).Valid() = %v, want %v", string(tc.role), got, tc.want)
			}
		})
	}
}

// TestSentinelErrors_AreDistinct guards against an accidental copy-paste
// that would alias two sentinels to the same value. errors.Is must be able
// to tell them apart so the HTTP layer can map them to different status
// codes.
func TestSentinelErrors_AreDistinct(t *testing.T) {
	sentinels := map[string]error{
		"ErrNotFound":      domain.ErrNotFound,
		"ErrAlreadyExists": domain.ErrAlreadyExists,
		"ErrUnauthorized":  domain.ErrUnauthorized,
		"ErrForbidden":     domain.ErrForbidden,
		"ErrValidation":    domain.ErrValidation,
		"ErrTooMany":       domain.ErrTooMany,
	}
	for nameA, a := range sentinels {
		for nameB, b := range sentinels {
			if nameA == nameB {
				continue
			}
			if errors.Is(a, b) {
				t.Fatalf("%s should not match %s via errors.Is", nameA, nameB)
			}
		}
	}
}

// TestSentinelErrors_AreWrappable verifies the wrapping contract documented
// in errors.go — callers add context with fmt.Errorf("... : %w", sentinel)
// and errors.Is at the boundary recovers the sentinel.
func TestSentinelErrors_AreWrappable(t *testing.T) {
	for _, sentinel := range []error{
		domain.ErrNotFound,
		domain.ErrAlreadyExists,
		domain.ErrUnauthorized,
		domain.ErrForbidden,
		domain.ErrValidation,
		domain.ErrTooMany,
	} {
		wrapped := fmt.Errorf("context: %w", sentinel)
		if !errors.Is(wrapped, sentinel) {
			t.Fatalf("wrapped error should match sentinel %v via errors.Is", sentinel)
		}
	}
}
