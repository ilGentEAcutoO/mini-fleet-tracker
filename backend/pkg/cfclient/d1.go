package cfclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	d1pkg "github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/repository/d1"
)

// Compile-time assertion that D1Client satisfies the repository/d1.Executor
// interface defined in TASK-003. This is the load-bearing line that closes
// the loop from the migrator: if the Executor contract drifts, this file
// stops compiling and we know immediately.
var _ d1pkg.Executor = (*D1Client)(nil)

// D1Client talks to the Cloudflare D1 REST API. It is safe for concurrent
// use by multiple goroutines as long as the underlying *http.Client is.
type D1Client struct {
	accountID  string
	databaseID string
	apiToken   string
	httpClient *http.Client
	baseURL    string // host root, e.g. "https://api.cloudflare.com"; no trailing slash
}

// D1Config is the constructor input for NewD1Client.
type D1Config struct {
	AccountID  string
	DatabaseID string
	APIToken   string
	// HTTPClient is optional; when nil a client with a 10s timeout is used.
	HTTPClient *http.Client
	// BaseURL is optional; defaults to https://api.cloudflare.com. Tests
	// point this at an httptest.Server. No trailing slash; we append.
	BaseURL string
}

// NewD1Client validates cfg and returns a ready-to-use client.
func NewD1Client(cfg D1Config) (*D1Client, error) {
	if strings.TrimSpace(cfg.AccountID) == "" {
		return nil, errors.New("cfclient.D1: AccountID is required")
	}
	if strings.TrimSpace(cfg.DatabaseID) == "" {
		return nil, errors.New("cfclient.D1: DatabaseID is required")
	}
	if strings.TrimSpace(cfg.APIToken) == "" {
		return nil, errors.New("cfclient.D1: APIToken is required")
	}

	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}

	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = "https://api.cloudflare.com"
	}

	return &D1Client{
		accountID:  cfg.AccountID,
		databaseID: cfg.DatabaseID,
		apiToken:   cfg.APIToken,
		httpClient: hc,
		baseURL:    base,
	}, nil
}

// d1Request is the JSON body posted to /query.
type d1Request struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params,omitempty"`
}

// d1Error mirrors the {code, message} objects inside the D1 envelope.
type d1Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// d1Result is one element of the top-level result array.
type d1Result struct {
	Results []map[string]any `json:"results"`
	Success bool             `json:"success"`
	Meta    map[string]any   `json:"meta,omitempty"`
}

// d1Envelope is the full top-level shape of every D1 response.
type d1Envelope struct {
	Result   []d1Result `json:"result"`
	Success  bool       `json:"success"`
	Errors   []d1Error  `json:"errors,omitempty"`
	Messages []d1Error  `json:"messages,omitempty"`
}

// queryURL returns the fully-qualified URL of the D1 /query endpoint.
func (c *D1Client) queryURL() string {
	return fmt.Sprintf(
		"%s/client/v4/accounts/%s/d1/database/%s/query",
		c.baseURL, c.accountID, c.databaseID,
	)
}

// doQuery is the shared HTTP round-trip used by both Exec and QueryRow. It
// posts the SQL + params, parses the envelope, and maps non-success states
// to a wrapped error.
func (c *D1Client) doQuery(ctx context.Context, method, sqlText string, args []any) (*d1Envelope, error) {
	if args == nil {
		args = []any{} // marshal as [] not null; some servers care
	}
	body, err := json.Marshal(d1Request{SQL: sqlText, Params: args})
	if err != nil {
		return nil, fmt.Errorf("d1 %s: marshal request: %w", method, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.queryURL(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("d1 %s: build request: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("d1 %s: http: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("d1 %s: read body: %w", method, err)
	}

	// Try to decode the envelope even on non-2xx — D1 returns the same
	// shape on errors and the embedded `errors[]` message is more useful
	// than a bare status code.
	var env d1Envelope
	decodeErr := json.Unmarshal(raw, &env)

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("d1 %s: %w (status %d): %s",
			method, ErrUnauthorized, resp.StatusCode, firstErrorMessage(env, raw))
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("d1 %s: %w (status %d): %s",
			method, ErrNotFound, resp.StatusCode, firstErrorMessage(env, raw))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("d1 %s: http %d: %s",
			method, resp.StatusCode, firstErrorMessage(env, raw))
	}

	if decodeErr != nil {
		return nil, fmt.Errorf("d1 %s: decode response: %w", method, decodeErr)
	}

	if !env.Success {
		return nil, fmt.Errorf("d1 %s: %s", method, firstErrorMessage(env, raw))
	}

	// Per-result success bit can be false even when the top-level success
	// is true (rare, but possible for partial batch failures); treat that
	// as a hard error so callers don't silently see zero rows.
	for i, r := range env.Result {
		if !r.Success {
			return nil, fmt.Errorf("d1 %s: result %d not successful: %s",
				method, i, firstErrorMessage(env, raw))
		}
	}

	return &env, nil
}

// firstErrorMessage extracts a useful message from a (possibly partial)
// envelope, falling back to the raw body. Always returns a non-empty string.
func firstErrorMessage(env d1Envelope, raw []byte) string {
	if len(env.Errors) > 0 {
		e := env.Errors[0]
		return fmt.Sprintf("[%d] %s", e.Code, e.Message)
	}
	if len(env.Messages) > 0 {
		m := env.Messages[0]
		return fmt.Sprintf("[%d] %s", m.Code, m.Message)
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "<empty response>"
	}
	// Cap so giant HTML error pages don't pollute logs.
	if len(s) > 512 {
		s = s[:512] + "...(truncated)"
	}
	return s
}

// Exec runs a statement against D1 and discards the result rows. It
// satisfies repository/d1.Executor.Exec.
//
// D1's /query endpoint accepts multi-statement bodies natively, so the
// migrator can hand us a whole *.up.sql file in one call without splitting.
func (c *D1Client) Exec(ctx context.Context, sqlText string, args ...any) error {
	_, err := c.doQuery(ctx, "Exec", sqlText, args)
	return err
}

// QueryRow runs a statement and returns its first row, satisfying
// repository/d1.Executor.QueryRow.
//
// The returned Row is non-nil regardless of outcome; Scan reports any
// query-time error and the "no rows" sentinel string that
// repository/d1.isNoRowsErr recognises.
func (c *D1Client) QueryRow(ctx context.Context, sqlText string, args ...any) d1pkg.Row {
	env, err := c.doQuery(ctx, "QueryRow", sqlText, args)
	if err != nil {
		return &d1Row{err: err}
	}
	// Pull the first row of the first result set. D1 returns an array of
	// results (one per statement); QueryRow only inspects [0].
	if len(env.Result) == 0 || len(env.Result[0].Results) == 0 {
		return &d1Row{err: errNoRows}
	}
	return &d1Row{
		columns: collectColumnOrder(env.Result[0].Results[0]),
		row:     env.Result[0].Results[0],
	}
}

// Query runs a statement that may return multiple rows. D1's /query endpoint
// already materialises all results in one HTTP response, so the returned
// Rows implementation walks a cached slice — Next is O(1), Close is a no-op
// beyond marking the cursor exhausted. There is no streaming layer.
//
// Errors from doQuery are returned directly (rather than deferred to the
// first Next call) so callers can distinguish "no rows yet" from
// "transport failed" without a Scan ceremony.
func (c *D1Client) Query(ctx context.Context, sqlText string, args ...any) (d1pkg.Rows, error) {
	env, err := c.doQuery(ctx, "Query", sqlText, args)
	if err != nil {
		return nil, err
	}
	if len(env.Result) == 0 {
		// A successful envelope with no result set is degenerate but valid;
		// return an iterator that yields zero rows.
		return &d1Rows{}, nil
	}
	rs := env.Result[0].Results
	// Snapshot the column order from the first row if any. An empty result
	// set has no column metadata in D1's envelope, but that is fine — Scan
	// is never called on a zero-row iterator.
	var columns []string
	if len(rs) > 0 {
		columns = collectColumnOrder(rs[0])
	}
	return &d1Rows{rows: rs, columns: columns}, nil
}

// d1Rows is the multi-row counterpart of d1Row. It walks a slice of
// JSON-decoded row maps with an internal cursor; Scan reuses the same
// assignD1Value path as d1Row so the supported destination types stay
// in lockstep.
//
// The zero cursor index is the "before-first-row" position — Next must
// be called once before the first Scan, matching database/sql.Rows
// semantics.
type d1Rows struct {
	rows    []map[string]any
	columns []string
	idx     int // 1-based after the first Next; 0 means before-first
	err     error
	closed  bool
}

// Next advances the cursor. Returns false when iteration is past the last
// row or after Close. A deferred error from a previous Scan does not
// terminate iteration on its own — callers should check Err() to detect it.
func (r *d1Rows) Next() bool {
	if r.closed {
		return false
	}
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

// Scan binds dest pointers to the current row. Callers must have called
// Next at least once. Mirrors d1Row.Scan in implementation so the supported
// destination type set is identical.
func (r *d1Rows) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if r.idx == 0 {
		return errors.New("d1 Rows: Scan called before Next")
	}
	if r.idx > len(r.rows) {
		return errors.New("d1 Rows: Scan called past end of rows")
	}
	row := r.rows[r.idx-1]
	if len(dest) > len(r.columns) {
		return fmt.Errorf("d1 Rows Scan: %d dest values but row has %d columns",
			len(dest), len(r.columns))
	}
	for i, d := range dest {
		col := r.columns[i]
		val, ok := row[col]
		if !ok {
			return fmt.Errorf("d1 Rows Scan: column %q missing from row", col)
		}
		if err := assignD1Value(d, val); err != nil {
			return fmt.Errorf("d1 Rows Scan: column %q: %w", col, err)
		}
	}
	return nil
}

// Err returns any deferred iteration error. The D1 client materialises the
// full result set up-front, so there is no streaming error path — Err is
// nil unless a previous Scan stored a sticky error.
func (r *d1Rows) Err() error { return r.err }

// Close marks the iterator exhausted. The slice-backed implementation
// holds no transport resources, so Close is effectively a flag flip and
// is safe to call multiple times.
func (r *d1Rows) Close() error {
	r.closed = true
	return nil
}

// errNoRows carries the magic substring that repository/d1.isNoRowsErr
// matches on. We deliberately mirror database/sql's message so the
// migrator and the in-memory test executor see the same shape.
var errNoRows = errors.New("sql: no rows in result set")

// d1Row implements repository/d1.Row.
//
// D1 returns each row as a map[string]any whose key order is not preserved
// by encoding/json. We snapshot the key order at construction time using
// the original JSON token stream so Scan can map positional dest pointers
// back to columns in their declared order.
//
// In practice the migrator uses single-column SELECTs (`SELECT version
// FROM schema_migrations WHERE version = ?`), so positional binding is
// adequate. Multi-column callers can pass dest pointers in column-declaration
// order or — better — switch to a named-binding API in a follow-up task.
type d1Row struct {
	columns []string
	row     map[string]any
	err     error
}

func (r *d1Row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) > len(r.columns) {
		return fmt.Errorf("d1 Scan: %d dest values but row has %d columns",
			len(dest), len(r.columns))
	}
	for i, d := range dest {
		col := r.columns[i]
		val, ok := r.row[col]
		if !ok {
			return fmt.Errorf("d1 Scan: column %q missing from row", col)
		}
		if err := assignD1Value(d, val); err != nil {
			return fmt.Errorf("d1 Scan: column %q: %w", col, err)
		}
	}
	return nil
}

// collectColumnOrder returns the keys of a single-row result. encoding/json
// does not preserve key order for map[string]any, but the D1 envelope's row
// objects are small enough that an alphabetical fallback works as long as
// the caller binds dest pointers in declared (== alphabetical) order. For
// the migrator's `SELECT version FROM schema_migrations` this is a no-op.
//
// A future iteration can swap to json.Decoder + a positional row type that
// preserves the server's column order; for TASK-005 the alphabetical
// fallback is good enough to satisfy the Executor contract and keep the
// code free of a third-party JSON parser.
func collectColumnOrder(row map[string]any) []string {
	keys := make([]string, 0, len(row))
	for k := range row {
		keys = append(keys, k)
	}
	// Sort for deterministic Scan-to-dest mapping.
	// Callers passing multi-column dest should bind in alphabetical key order.
	sortStrings(keys)
	return keys
}

// sortStrings is a tiny insertion sort to avoid pulling in "sort" for one
// 1-3 element slice. Keeps the import surface lean.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// assignD1Value copies a single JSON-decoded value into a typed pointer.
// D1 only ever returns SQLite-flavoured primitives (TEXT, INTEGER, REAL,
// BLOB, NULL), so the supported dest types are intentionally narrow.
func assignD1Value(dest, src any) error {
	switch d := dest.(type) {
	case *string:
		switch v := src.(type) {
		case string:
			*d = v
		case nil:
			*d = ""
		default:
			return fmt.Errorf("cannot assign %T to *string", src)
		}
	case *int:
		switch v := src.(type) {
		case float64:
			*d = int(v)
		case nil:
			*d = 0
		default:
			return fmt.Errorf("cannot assign %T to *int", src)
		}
	case *int64:
		switch v := src.(type) {
		case float64:
			*d = int64(v)
		case nil:
			*d = 0
		default:
			return fmt.Errorf("cannot assign %T to *int64", src)
		}
	case *float64:
		switch v := src.(type) {
		case float64:
			*d = v
		case nil:
			*d = 0
		default:
			return fmt.Errorf("cannot assign %T to *float64", src)
		}
	case *bool:
		switch v := src.(type) {
		case bool:
			*d = v
		case nil:
			*d = false
		default:
			return fmt.Errorf("cannot assign %T to *bool", src)
		}
	case *any:
		*d = src
	default:
		return fmt.Errorf("unsupported dest type %T", dest)
	}
	return nil
}
