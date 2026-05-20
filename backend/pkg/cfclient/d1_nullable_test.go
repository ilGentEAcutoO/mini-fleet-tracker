package cfclient

import (
	"context"
	"io"
	"net/http"
	"testing"
)

// TestAssignD1ValueNullable pins the **string / **int64 / **float64
// support paths in assignD1Value. The vehicle repository (and any future
// repo with nullable primitive columns) relies on this idiom — see
// backend/internal/repository/d1/vehicle_repo.go:40-42 for the original
// motivation — so Scan can distinguish SQL NULL from the zero value of
// the underlying type.
//
// Each subtest stubs a two-row D1 envelope where the first row carries a
// concrete value and the second row carries SQL NULL (encoded as JSON
// null after envelope decoding). Both rows are scanned through the same
// **T target; the assertions confirm:
//
//   - non-null value → *T is non-nil and equals the source value
//   - null value     → *T is nil
//
// If the underlying assignD1Value lacks a **T case, the test fails at
// the first Scan with "unsupported dest type **T" — that's the exact
// production failure that drove this fix.
func TestAssignD1ValueNullable(t *testing.T) {
	t.Run("**string", func(t *testing.T) {
		c, _ := newD1TestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{
				"success": true,
				"result": [{
					"success": true,
					"results": [
						{"v": "hello"},
						{"v": null}
					]
				}]
			}`)
		})

		rows, err := c.Query(context.Background(), "SELECT v FROM t")
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		defer func() { _ = rows.Close() }()

		// Row 1: concrete string.
		if !rows.Next() {
			t.Fatal("expected first row")
		}
		var s *string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("Scan **string (non-null): %v", err)
		}
		if s == nil {
			t.Fatal("**string scan of non-null returned nil pointer")
		}
		if *s != "hello" {
			t.Fatalf("**string value = %q, want %q", *s, "hello")
		}

		// Row 2: SQL NULL.
		if !rows.Next() {
			t.Fatal("expected second row")
		}
		s = nil // reset so a failure to write nil is visible
		// Make s point at a value first so we can detect that Scan
		// actually wrote nil rather than leaving it untouched.
		dummy := "untouched"
		s = &dummy
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("Scan **string (null): %v", err)
		}
		if s != nil {
			t.Fatalf("**string scan of NULL should set pointer to nil, got %q", *s)
		}
	})

	t.Run("**int64", func(t *testing.T) {
		c, _ := newD1TestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{
				"success": true,
				"result": [{
					"success": true,
					"results": [
						{"v": 42},
						{"v": null}
					]
				}]
			}`)
		})

		rows, err := c.Query(context.Background(), "SELECT v FROM t")
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		defer func() { _ = rows.Close() }()

		if !rows.Next() {
			t.Fatal("expected first row")
		}
		var i *int64
		if err := rows.Scan(&i); err != nil {
			t.Fatalf("Scan **int64 (non-null): %v", err)
		}
		if i == nil {
			t.Fatal("**int64 scan of non-null returned nil pointer")
		}
		if *i != 42 {
			t.Fatalf("**int64 value = %d, want 42", *i)
		}

		if !rows.Next() {
			t.Fatal("expected second row")
		}
		dummy := int64(99)
		i = &dummy
		if err := rows.Scan(&i); err != nil {
			t.Fatalf("Scan **int64 (null): %v", err)
		}
		if i != nil {
			t.Fatalf("**int64 scan of NULL should set pointer to nil, got %d", *i)
		}
	})

	t.Run("**float64", func(t *testing.T) {
		c, _ := newD1TestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{
				"success": true,
				"result": [{
					"success": true,
					"results": [
						{"v": 3.14},
						{"v": null}
					]
				}]
			}`)
		})

		rows, err := c.Query(context.Background(), "SELECT v FROM t")
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		defer func() { _ = rows.Close() }()

		if !rows.Next() {
			t.Fatal("expected first row")
		}
		var f *float64
		if err := rows.Scan(&f); err != nil {
			t.Fatalf("Scan **float64 (non-null): %v", err)
		}
		if f == nil {
			t.Fatal("**float64 scan of non-null returned nil pointer")
		}
		if *f != 3.14 {
			t.Fatalf("**float64 value = %v, want 3.14", *f)
		}

		if !rows.Next() {
			t.Fatal("expected second row")
		}
		dummy := 2.71
		f = &dummy
		if err := rows.Scan(&f); err != nil {
			t.Fatalf("Scan **float64 (null): %v", err)
		}
		if f != nil {
			t.Fatalf("**float64 scan of NULL should set pointer to nil, got %v", *f)
		}
	})
}

// TestAssignD1ValueNullable_QueryRow mirrors the multi-row test through
// QueryRow + d1Row.Scan to confirm both Scan paths share the same
// destination type set. scanVehicle (single-row helper) calls QueryRow,
// so this is the path the production Get(:id) handler tripped over.
func TestAssignD1ValueNullable_QueryRow(t *testing.T) {
	t.Run("**string non-null", func(t *testing.T) {
		c, _ := newD1TestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{
				"success": true,
				"result": [{"success": true, "results": [{"v": "world"}]}]
			}`)
		})
		var s *string
		row := c.QueryRow(context.Background(), "SELECT v FROM t WHERE id = ?", "x")
		if err := row.Scan(&s); err != nil {
			t.Fatalf("QueryRow Scan **string: %v", err)
		}
		if s == nil || *s != "world" {
			t.Fatalf("got %v, want pointer to 'world'", s)
		}
	})

	t.Run("**string null", func(t *testing.T) {
		c, _ := newD1TestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{
				"success": true,
				"result": [{"success": true, "results": [{"v": null}]}]
			}`)
		})
		dummy := "untouched"
		s := &dummy
		row := c.QueryRow(context.Background(), "SELECT v FROM t WHERE id = ?", "x")
		if err := row.Scan(&s); err != nil {
			t.Fatalf("QueryRow Scan **string null: %v", err)
		}
		if s != nil {
			t.Fatalf("**string null should yield nil, got %q", *s)
		}
	})
}
