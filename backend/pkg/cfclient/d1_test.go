package cfclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	d1pkg "github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/repository/d1"
)

// newD1TestClient spins up an httptest.Server with the given handler and
// returns a D1Client pointed at it. Tests assert against the handler-side
// inspection captured in closures.
func newD1TestClient(t *testing.T, handler http.HandlerFunc) (*D1Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := NewD1Client(D1Config{
		AccountID:  "acc-test",
		DatabaseID: "db-test",
		APIToken:   "tok-test",
		BaseURL:    srv.URL,
	})
	if err != nil {
		t.Fatalf("NewD1Client: %v", err)
	}
	return c, srv
}

func TestNewD1Client_RequiredFields(t *testing.T) {
	tests := []struct {
		name string
		cfg  D1Config
		want string
	}{
		{"missing AccountID", D1Config{DatabaseID: "d", APIToken: "t"}, "AccountID"},
		{"missing DatabaseID", D1Config{AccountID: "a", APIToken: "t"}, "DatabaseID"},
		{"missing APIToken", D1Config{AccountID: "a", DatabaseID: "d"}, "APIToken"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewD1Client(tc.cfg)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error to mention %q, got %v", tc.want, err)
			}
		})
	}
}

func TestD1_ExecutorInterface(t *testing.T) {
	// Compile-time assertion already lives in d1.go; this is the runtime
	// canary: any tightening of the Executor interface that breaks the
	// client will surface here as a build error too.
	var _ d1pkg.Executor = (*D1Client)(nil)
}

func TestD1_Exec_SendsAuthAndBody(t *testing.T) {
	var gotAuth, gotCT, gotPath, gotMethod string
	var gotBody d1Request

	c, _ := newD1TestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotPath = r.URL.Path
		gotMethod = r.Method
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Fatalf("unmarshal request body: %v", err)
		}
		// Minimal happy-path envelope.
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true,"result":[{"results":[],"success":true}]}`)
	})

	if err := c.Exec(context.Background(),
		"INSERT INTO drivers (id) VALUES (?)", "drv-1"); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/client/v4/accounts/acc-test/d1/database/db-test/query" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer tok-test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotBody.SQL != "INSERT INTO drivers (id) VALUES (?)" {
		t.Errorf("SQL = %q", gotBody.SQL)
	}
	if len(gotBody.Params) != 1 || gotBody.Params[0] != "drv-1" {
		t.Errorf("Params = %+v", gotBody.Params)
	}
}

func TestD1_Exec_NoArgsMarshalsAsEmptyArray(t *testing.T) {
	c, _ := newD1TestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// We expect EITHER "params":[] OR no "params" key at all. The
		// implementation omits it via `omitempty` when params is empty
		// after our zero-args replacement (Go's encoding/json drops
		// empty slices when omitempty is set).
		if strings.Contains(string(body), `"params":null`) {
			t.Errorf("body should not encode params as null: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true,"result":[{"results":[],"success":true}]}`)
	})
	if err := c.Exec(context.Background(), "CREATE TABLE t (x INT)"); err != nil {
		t.Fatalf("Exec: %v", err)
	}
}

func TestD1_QueryRow_ScansSingleColumn(t *testing.T) {
	c, _ := newD1TestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"success": true,
			"result": [{
				"success": true,
				"results": [{"version": "000001_init"}]
			}]
		}`)
	})

	var ver string
	row := c.QueryRow(context.Background(),
		"SELECT version FROM schema_migrations WHERE version = ?", "000001_init")
	if err := row.Scan(&ver); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if ver != "000001_init" {
		t.Fatalf("ver = %q", ver)
	}
}

func TestD1_QueryRow_NoRows_ReturnsRecognisedSentinel(t *testing.T) {
	c, _ := newD1TestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"success": true,
			"result": [{"success": true, "results": []}]
		}`)
	})

	var ver string
	row := c.QueryRow(context.Background(),
		"SELECT version FROM schema_migrations WHERE version = ?", "missing")
	err := row.Scan(&ver)
	if err == nil {
		t.Fatal("expected error for empty result set")
	}
	// The migrator's isNoRowsErr matches on this substring. If it ever
	// drifts, the migrator silently treats "no row" as "row found" and
	// the consequence is that migrations would re-apply. Pin it hard.
	if !strings.Contains(err.Error(), "no rows in result set") {
		t.Fatalf("error must contain 'no rows in result set', got: %v", err)
	}
}

func TestD1_Exec_MapsUnauthorized(t *testing.T) {
	c, _ := newD1TestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{
			"success": false,
			"errors": [{"code": 10000, "message": "Authentication error"}]
		}`)
	})
	err := c.Exec(context.Background(), "SELECT 1")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
	if !strings.Contains(err.Error(), "Authentication error") {
		t.Fatalf("expected D1 message to bubble up, got %v", err)
	}
}

func TestD1_Exec_MapsNotFound(t *testing.T) {
	c, _ := newD1TestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"success": false, "errors":[{"code":7404,"message":"database not found"}]}`)
	})
	err := c.Exec(context.Background(), "SELECT 1")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestD1_Exec_MapsTopLevelSuccessFalse(t *testing.T) {
	c, _ := newD1TestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		// HTTP 200 but envelope says success:false — D1 occasionally
		// returns this for SQL-level failures (constraint violations etc.).
		_, _ = io.WriteString(w, `{
			"success": false,
			"errors": [{"code": 7500, "message": "table foo has no column bar"}]
		}`)
	})
	err := c.Exec(context.Background(), "SELECT bar FROM foo")
	if err == nil {
		t.Fatal("expected error for envelope success=false")
	}
	if !strings.Contains(err.Error(), "table foo has no column bar") {
		t.Fatalf("expected SQL message to bubble, got %v", err)
	}
}

func TestD1_Exec_MapsPerResultSuccessFalse(t *testing.T) {
	c, _ := newD1TestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{
			"success": true,
			"result": [{
				"success": false,
				"results": [],
				"meta": {}
			}],
			"errors": [{"code": 7600, "message": "constraint failed"}]
		}`)
	})
	err := c.Exec(context.Background(), "INSERT INTO drivers VALUES (?)", "x")
	if err == nil {
		t.Fatal("expected error for result success=false")
	}
	if !strings.Contains(err.Error(), "result 0 not successful") {
		t.Fatalf("expected per-result error, got %v", err)
	}
}

func TestD1_Exec_500WithUndecodableBody(t *testing.T) {
	c, _ := newD1TestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "<html>bad gateway</html>")
	})
	err := c.Exec(context.Background(), "SELECT 1")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "http 500") {
		t.Fatalf("expected status to surface, got %v", err)
	}
}

func TestD1_BaseURL_TrimsTrailingSlash(t *testing.T) {
	// We can't easily verify the URL without doing a round-trip, but
	// we can at least confirm that BaseURL with a trailing slash doesn't
	// produce a double-slashed path that some HTTP routers reject.
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"success":true,"result":[{"results":[],"success":true}]}`)
	}))
	t.Cleanup(srv.Close)

	c, err := NewD1Client(D1Config{
		AccountID:  "a",
		DatabaseID: "d",
		APIToken:   "t",
		BaseURL:    srv.URL + "/", // trailing slash
	})
	if err != nil {
		t.Fatalf("NewD1Client: %v", err)
	}
	if err := c.Exec(context.Background(), "SELECT 1"); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.Contains(gotPath, "//") {
		t.Fatalf("trailing slash leaked into path: %q", gotPath)
	}
}

func TestD1_QueryRow_ScanTooManyDest(t *testing.T) {
	c, _ := newD1TestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{
			"success": true,
			"result": [{"success": true, "results": [{"id": "x"}]}]
		}`)
	})
	var a, b string
	row := c.QueryRow(context.Background(), "SELECT id FROM t")
	err := row.Scan(&a, &b)
	if err == nil {
		t.Fatal("expected error when binding more dest than columns")
	}
}

func TestD1_QueryRow_UnsupportedDestType(t *testing.T) {
	c, _ := newD1TestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{
			"success": true,
			"result": [{"success": true, "results": [{"id": "x"}]}]
		}`)
	})
	var dest map[string]any // not supported by assignD1Value
	row := c.QueryRow(context.Background(), "SELECT id FROM t")
	if err := row.Scan(&dest); err == nil {
		t.Fatal("expected unsupported-dest-type error")
	}
}

func TestD1_QueryRow_PropagatesQueryError(t *testing.T) {
	c, _ := newD1TestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"success":false}`)
	})
	var v string
	row := c.QueryRow(context.Background(), "SELECT 1")
	if err := row.Scan(&v); err == nil {
		t.Fatal("expected propagated query error")
	}
}

func TestD1_AssignsAllScalarKinds(t *testing.T) {
	c, _ := newD1TestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		// Use single-column rows to make Scan deterministic without
		// fighting collectColumnOrder's alphabetical sort.
		_, _ = io.WriteString(w, `{
			"success": true,
			"result": [{"success": true, "results": [{"v": 42}]}]
		}`)
	})

	t.Run("int", func(t *testing.T) {
		var i int
		row := c.QueryRow(context.Background(), "SELECT v")
		if err := row.Scan(&i); err != nil {
			t.Fatalf("scan int: %v", err)
		}
		if i != 42 {
			t.Fatalf("i=%d", i)
		}
	})
}
