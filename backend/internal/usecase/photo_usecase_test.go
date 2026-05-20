package usecase

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
)

// ---------------------------------------------------------------------------
// Hand-rolled mocks. Kept inline so each test case is one screen.
// ---------------------------------------------------------------------------

// memQuotaStore is an in-memory KV stand-in. Mirrors the cfclient.KVClient
// contract: Get returns (nil, false, nil) on missing key; Put with non-zero
// ttl records the ttl (tests can assert on it).
type memQuotaStore struct {
	mu       sync.Mutex
	data     map[string][]byte
	ttls     map[string]time.Duration
	getErr   error
	putErr   error
	putCalls int
	getCalls int
}

func newMemQuotaStore() *memQuotaStore {
	return &memQuotaStore{
		data: map[string][]byte{},
		ttls: map[string]time.Duration{},
	}
}

func (m *memQuotaStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCalls++
	if m.getErr != nil {
		return nil, false, m.getErr
	}
	v, ok := m.data[key]
	if !ok {
		return nil, false, nil
	}
	cp := make([]byte, len(v))
	copy(cp, v)
	return cp, true, nil
}

func (m *memQuotaStore) Put(_ context.Context, key string, value []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.putCalls++
	if m.putErr != nil {
		return m.putErr
	}
	cp := make([]byte, len(value))
	copy(cp, value)
	m.data[key] = cp
	m.ttls[key] = ttl
	return nil
}

// seedCount preloads the counter so a test can start from a non-zero
// quota state without going through PresignUpload.
func (m *memQuotaStore) seedCount(key string, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = []byte(strconv.Itoa(n))
	m.ttls[key] = quotaTTL
}

// recordedPresign captures every call to PresignPutObject so the test
// can assert on the key/ttl/contentLengthMax triple the usecase sent.
type recordedPresign struct {
	key              string
	ttl              time.Duration
	contentLengthMax int64
}

// memPresigner is an in-memory R2 stand-in. Returns deterministic URLs
// so tests can assert on the response shape.
type memPresigner struct {
	mu             sync.Mutex
	putCalls       []recordedPresign
	getCalls       []string
	listCalls      []string
	listReturn     []string
	listErr        error
	presignPutErr  error
	presignGetErr  error
	presignGetErrs map[string]error
}

func newMemPresigner() *memPresigner {
	return &memPresigner{presignGetErrs: map[string]error{}}
}

func (m *memPresigner) PresignPutObject(
	_ context.Context, key string, ttl time.Duration, contentLengthMax int64,
) (*url.URL, http.Header, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.putCalls = append(m.putCalls, recordedPresign{key: key, ttl: ttl, contentLengthMax: contentLengthMax})
	if m.presignPutErr != nil {
		return nil, nil, m.presignPutErr
	}
	u, _ := url.Parse(fmt.Sprintf("https://r2.test/mft/%s?X-Amz-Signature=fake", key))
	hdr := http.Header{}
	if contentLengthMax > 0 {
		hdr.Set("Content-Length", strconv.FormatInt(contentLengthMax, 10))
	}
	return u, hdr, nil
}

func (m *memPresigner) PresignGetObject(_ context.Context, key string, _ time.Duration) (*url.URL, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCalls = append(m.getCalls, key)
	if err, ok := m.presignGetErrs[key]; ok {
		return nil, err
	}
	if m.presignGetErr != nil {
		return nil, m.presignGetErr
	}
	u, _ := url.Parse(fmt.Sprintf("https://r2.test/mft/%s?X-Amz-Signature=fakeget", key))
	return u, nil
}

func (m *memPresigner) ListObjects(_ context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listCalls = append(m.listCalls, prefix)
	if m.listErr != nil {
		return nil, m.listErr
	}
	out := make([]string, len(m.listReturn))
	copy(out, m.listReturn)
	return out, nil
}

// memVehicleExistence is an in-memory vehicleExistenceLookup. Tests
// register IDs that should "exist"; absent IDs return ErrNotFound.
type memVehicleExistence struct {
	mu     sync.Mutex
	owners map[string]string
	failOn map[string]error
}

func newMemVehicleExistence() *memVehicleExistence {
	return &memVehicleExistence{owners: map[string]string{}, failOn: map[string]error{}}
}

func (m *memVehicleExistence) set(id, driverID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.owners[id] = driverID
}

func (m *memVehicleExistence) Get(_ context.Context, id string) (*domain.Vehicle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err, ok := m.failOn[id]; ok {
		return nil, err
	}
	d, ok := m.owners[id]
	if !ok {
		return nil, fmt.Errorf("vehicle %s: %w", id, domain.ErrNotFound)
	}
	return &domain.Vehicle{ID: id, DriverID: d}, nil
}

// counterIDs is the deterministic IDGenerator used by photo tests. Each
// NewID call advances a counter so the generated object keys are
// predictable across the table-driven cases.
type counterPhotoIDs struct {
	mu  sync.Mutex
	n   int
	pfx string
}

func newCounterPhotoIDs(pfx string) *counterPhotoIDs {
	return &counterPhotoIDs{pfx: pfx}
}

func (c *counterPhotoIDs) NewID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return fmt.Sprintf("%s%03d", c.pfx, c.n)
}

// ---------------------------------------------------------------------------
// Construction.
// ---------------------------------------------------------------------------

func TestNewPhotoUsecase_RequiredDeps(t *testing.T) {
	pres := newMemPresigner()
	qs := newMemQuotaStore()
	vex := newMemVehicleExistence()
	ids := newCounterPhotoIDs("ph_")

	tests := []struct {
		name string
		make func() (*PhotoUsecase, error)
		want string
	}{
		{"nil presigner", func() (*PhotoUsecase, error) { return NewPhotoUsecase(nil, qs, vex, ids) }, "presigner"},
		{"nil quotas", func() (*PhotoUsecase, error) { return NewPhotoUsecase(pres, nil, vex, ids) }, "quotas"},
		{"nil vehicles", func() (*PhotoUsecase, error) { return NewPhotoUsecase(pres, qs, nil, ids) }, "vehicles"},
		{"nil ids", func() (*PhotoUsecase, error) { return NewPhotoUsecase(pres, qs, vex, nil) }, "id generator"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.make()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// PresignUpload — quota table.
// ---------------------------------------------------------------------------

func TestPhotoUsecase_PresignUpload_Quota(t *testing.T) {
	fixedNow := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	const vehID = "veh-001"
	const userID = "drv-001"

	tests := []struct {
		name          string
		seeded        int    // pre-existing quota count
		filename      string // raw filename input
		wantStatus    string // "ok" | "quota" | "notfound"
		wantRemaining int
	}{
		{"first upload of the day", 0, "front_view.jpg", "ok", 2},
		{"second upload of the day", 1, "side.png", "ok", 1},
		{"third (and final) upload of the day", 2, "back view.jpg", "ok", 0},
		{"fourth upload is over quota", 3, "fourth.jpg", "quota", 0},
		{"quota one past the cap stays rejected", 4, "x.jpg", "quota", 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pres := newMemPresigner()
			qs := newMemQuotaStore()
			vex := newMemVehicleExistence()
			vex.set(vehID, userID)
			ids := newCounterPhotoIDs("ph_")
			uc, err := NewPhotoUsecase(pres, qs, vex, ids)
			if err != nil {
				t.Fatalf("NewPhotoUsecase: %v", err)
			}
			uc.now = func() time.Time { return fixedNow }

			if tc.seeded > 0 {
				qs.seedCount(quotaKeyFor(vehID, fixedNow), tc.seeded)
			}

			out, err := uc.PresignUpload(context.Background(), userID, vehID, tc.filename)

			switch tc.wantStatus {
			case "ok":
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if out == nil {
					t.Fatal("expected output")
				}
				if out.URL == "" {
					t.Error("output URL should be non-empty")
				}
				if out.Method != http.MethodPut {
					t.Errorf("method = %q, want PUT", out.Method)
				}
				if out.ContentLengthMax != MaxPhotoBytes {
					t.Errorf("content_length_max = %d, want %d", out.ContentLengthMax, MaxPhotoBytes)
				}
				if out.QuotaRemaining != tc.wantRemaining {
					t.Errorf("quota_remaining = %d, want %d", out.QuotaRemaining, tc.wantRemaining)
				}
				if !strings.HasPrefix(out.Key, fmt.Sprintf("vehicles/%s/", vehID)) {
					t.Errorf("key prefix wrong: %q", out.Key)
				}
				// Filename sanitisation: spaces must have been replaced
				// with underscores; the original extension preserved.
				if strings.Contains(out.Key, " ") {
					t.Errorf("key should not contain raw spaces: %q", out.Key)
				}
				// The presigner should have been called with the chosen key.
				if len(pres.putCalls) != 1 {
					t.Errorf("PresignPutObject call count = %d, want 1", len(pres.putCalls))
				}
				if pres.putCalls[0].contentLengthMax != MaxPhotoBytes {
					t.Errorf("PresignPutObject contentLengthMax = %d, want %d",
						pres.putCalls[0].contentLengthMax, MaxPhotoBytes)
				}
				if pres.putCalls[0].ttl != PresignPutTTL {
					t.Errorf("PresignPutObject ttl = %s, want %s",
						pres.putCalls[0].ttl, PresignPutTTL)
				}
				// Quota was incremented.
				if qs.putCalls != 1 {
					t.Errorf("quota Put call count = %d, want 1", qs.putCalls)
				}
				written := string(qs.data[quotaKeyFor(vehID, fixedNow)])
				wantWritten := strconv.Itoa(tc.seeded + 1)
				if written != wantWritten {
					t.Errorf("quota value written = %q, want %q", written, wantWritten)
				}
			case "quota":
				if err == nil {
					t.Fatal("expected ErrTooMany")
				}
				if !errors.Is(err, domain.ErrTooMany) {
					t.Errorf("err = %v, want errors.Is ErrTooMany", err)
				}
				if out != nil {
					t.Errorf("output should be nil on quota error, got %+v", out)
				}
				// Presigner must NOT have been called.
				if len(pres.putCalls) != 0 {
					t.Errorf("presigner should not be called on quota-exceeded: %v", pres.putCalls)
				}
				// Quota must NOT have been written.
				if qs.putCalls != 0 {
					t.Errorf("quota should not increment on rejection: %d puts", qs.putCalls)
				}
			default:
				t.Fatalf("unknown wantStatus %q", tc.wantStatus)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// PresignUpload — validation branches.
// ---------------------------------------------------------------------------

func TestPhotoUsecase_PresignUpload_VehicleNotFound(t *testing.T) {
	pres := newMemPresigner()
	qs := newMemQuotaStore()
	vex := newMemVehicleExistence() // empty — every lookup returns ErrNotFound
	ids := newCounterPhotoIDs("ph_")
	uc, _ := NewPhotoUsecase(pres, qs, vex, ids)

	_, err := uc.PresignUpload(context.Background(), "drv-001", "missing", "x.jpg")
	if err == nil {
		t.Fatal("expected error for missing vehicle")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want errors.Is ErrNotFound", err)
	}
	if len(pres.putCalls) != 0 {
		t.Errorf("presigner should not be called when vehicle is missing")
	}
	if qs.getCalls != 0 {
		t.Errorf("quota Get should not be called when vehicle is missing: %d", qs.getCalls)
	}
}

func TestPhotoUsecase_PresignUpload_ValidationBranches(t *testing.T) {
	const vehID = "veh-001"
	const userID = "drv-001"

	tests := []struct {
		name      string
		userID    string
		vehicleID string
		filename  string
		wantStr   string
	}{
		{"empty vehicle id", userID, "", "x.jpg", "vehicle_id"},
		{"whitespace vehicle id", userID, "   ", "x.jpg", "vehicle_id"},
		{"empty user id", "", vehID, "x.jpg", "user_id"},
		{"empty filename", userID, vehID, "", "filename"},
		{"whitespace filename", userID, vehID, "   ", "filename"},
		{"filename too long", userID, vehID, strings.Repeat("a", maxFilenameLen+1) + ".jpg", "too long"},
		{"path traversal filename produces only basename", userID, vehID, "../../etc/passwd", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pres := newMemPresigner()
			qs := newMemQuotaStore()
			vex := newMemVehicleExistence()
			vex.set(vehID, userID)
			ids := newCounterPhotoIDs("ph_")
			uc, _ := NewPhotoUsecase(pres, qs, vex, ids)

			out, err := uc.PresignUpload(context.Background(), tc.userID, tc.vehicleID, tc.filename)

			// "" wantStr means the call should succeed (path traversal
			// reduces to a safe basename, not an error).
			if tc.wantStr == "" {
				if err != nil {
					t.Fatalf("expected success after sanitisation, got: %v", err)
				}
				// passwd is the basename of "../../etc/passwd"; ensure it
				// is now the suffix of the object key (a literal `passwd`
				// at the end, with no path-traversal markers anywhere).
				if !strings.HasSuffix(out.Key, "-passwd") && !strings.HasSuffix(out.Key, "/passwd") {
					t.Errorf("sanitised key = %q, expected to end with -passwd or /passwd", out.Key)
				}
				if strings.Contains(out.Key, "..") {
					t.Errorf("key escapes prefix: %q", out.Key)
				}
				// Vehicle prefix must remain rooted at vehicles/<id>/.
				if !strings.HasPrefix(out.Key, "vehicles/veh-001/") {
					t.Errorf("key prefix lost: %q", out.Key)
				}
				return
			}

			if err == nil {
				t.Fatalf("expected validation error for %q", tc.name)
			}
			if !errors.Is(err, domain.ErrValidation) {
				t.Errorf("err = %v, want errors.Is ErrValidation", err)
			}
			if !strings.Contains(err.Error(), tc.wantStr) {
				t.Errorf("err msg %q should contain %q", err.Error(), tc.wantStr)
			}
		})
	}
}

// TestPhotoUsecase_PresignUpload_QuotaIncrementFailureIsFailClosed pins
// TASK-052 / security review M2. Before, a Put failure logged a warn
// and STILL returned the URL — meaning a KV outage let an attacker
// burst far past the daily-3 cap because every retry re-minted a fresh
// presigned URL while the counter stayed stuck. The new contract:
// writeQuota error → return domain.ErrUnavailable so the handler
// surfaces 503 + Retry-After and the client retries instead of bursting
// uploads through an uncounted gap.
//
// The presigned URL itself is minted before the write — that's fine
// because it never reaches the client when we return an error, and
// PresignPutObject doesn't reserve R2 capacity (it's pure SigV4
// cryptography). A future Option B (atomic FleetQuota DO) lands the
// increment + sign in one RPC; until then fail-CLOSED is the correct
// trade for a 5MB-per-upload, cost-bounded demo.
func TestPhotoUsecase_PresignUpload_QuotaIncrementFailureIsFailClosed(t *testing.T) {
	pres := newMemPresigner()
	qs := newMemQuotaStore()
	qs.putErr = errors.New("kv put 500")
	vex := newMemVehicleExistence()
	vex.set("veh-001", "drv-001")
	ids := newCounterPhotoIDs("ph_")
	uc, _ := NewPhotoUsecase(pres, qs, vex, ids)

	buf := captureLogs(t)
	out, err := uc.PresignUpload(context.Background(), "drv-001", "veh-001", "ok.jpg")
	if err == nil {
		t.Fatal("expected fail-closed error on writeQuota failure")
	}
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("err = %v, want errors.Is(domain.ErrUnavailable)", err)
	}
	if out != nil {
		t.Errorf("expected nil output when fail-closed; got %+v", out)
	}
	// Warn log still fires so operators see the KV health drift.
	if !strings.Contains(buf.String(), "quota write failed") && !strings.Contains(buf.String(), "kv put 500") {
		t.Errorf("expected warn log mentioning the storage failure; got %q", buf.String())
	}
}

// readQuotaFailureRejectsRequest verifies that the more dangerous
// branch — KV Get failure — DOES fail the request. We refuse to issue
// presigns when we cannot tell whether the quota has been hit, so a KV
// outage cannot be used as a "let me upload unlimited photos" gap.
func TestPhotoUsecase_PresignUpload_QuotaReadFailureFails(t *testing.T) {
	pres := newMemPresigner()
	qs := newMemQuotaStore()
	qs.getErr = errors.New("kv get 500")
	vex := newMemVehicleExistence()
	vex.set("veh-001", "drv-001")
	ids := newCounterPhotoIDs("ph_")
	uc, _ := NewPhotoUsecase(pres, qs, vex, ids)

	_, err := uc.PresignUpload(context.Background(), "drv-001", "veh-001", "x.jpg")
	if err == nil {
		t.Fatal("expected error on KV read failure")
	}
	if !strings.Contains(err.Error(), "quota read") {
		t.Errorf("err = %v, want mention of quota read", err)
	}
	if len(pres.putCalls) != 0 {
		t.Errorf("presigner should not be called when quota read fails")
	}
}

// corruptedQuotaValueIsAnError — if the counter is not a valid integer
// we refuse to count rather than reset (otherwise an attacker who can
// inject garbage into KV gets a free reset).
func TestPhotoUsecase_PresignUpload_CorruptedQuotaCounterFails(t *testing.T) {
	pres := newMemPresigner()
	qs := newMemQuotaStore()
	fixedNow := time.Now().UTC()
	qs.data[quotaKeyFor("veh-001", fixedNow)] = []byte("garbage-not-a-number")
	vex := newMemVehicleExistence()
	vex.set("veh-001", "drv-001")
	ids := newCounterPhotoIDs("ph_")
	uc, _ := NewPhotoUsecase(pres, qs, vex, ids)
	uc.now = func() time.Time { return fixedNow }

	_, err := uc.PresignUpload(context.Background(), "drv-001", "veh-001", "x.jpg")
	if err == nil {
		t.Fatal("expected error on corrupted quota counter")
	}
	if !strings.Contains(err.Error(), "parse quota counter") && !strings.Contains(err.Error(), "quota read") {
		t.Errorf("err = %v, want mention of parse failure", err)
	}
}

func TestPhotoUsecase_PresignUpload_PresignerFailureSurfaces(t *testing.T) {
	pres := newMemPresigner()
	pres.presignPutErr = errors.New("r2 broke")
	qs := newMemQuotaStore()
	vex := newMemVehicleExistence()
	vex.set("veh-001", "drv-001")
	ids := newCounterPhotoIDs("ph_")
	uc, _ := NewPhotoUsecase(pres, qs, vex, ids)

	_, err := uc.PresignUpload(context.Background(), "drv-001", "veh-001", "x.jpg")
	if err == nil {
		t.Fatal("expected error when presigner fails")
	}
	if !strings.Contains(err.Error(), "PresignPutObject") {
		t.Errorf("err = %v, want PresignPutObject wrap", err)
	}
	// The quota MUST NOT be incremented if the presign failed — otherwise
	// transient R2 errors waste the user's daily allowance.
	if qs.putCalls != 0 {
		t.Errorf("quota should not increment when presign fails: %d", qs.putCalls)
	}
}

// ---------------------------------------------------------------------------
// PresignUpload — quota key format and TTL.
// ---------------------------------------------------------------------------

func TestQuotaKeyFor_FormatIsStableAcrossTimezones(t *testing.T) {
	// 23:30 in UTC+8 is 15:30 UTC — same calendar day. The key MUST
	// use the UTC date so the boundary is unambiguous.
	loc8, _ := time.LoadLocation("Asia/Bangkok")
	t1 := time.Date(2026, 5, 19, 23, 30, 0, 0, loc8) // 2026-05-19 15:30 UTC
	want := "quota:veh-001:2026-05-19"
	if got := quotaKeyFor("veh-001", t1); got != want {
		t.Errorf("quotaKeyFor = %q, want %q", got, want)
	}
	// 06:00 in UTC+8 is 22:00 UTC the previous day — different UTC day.
	t2 := time.Date(2026, 5, 20, 6, 0, 0, 0, loc8) // 2026-05-19 22:00 UTC
	want2 := "quota:veh-001:2026-05-19"
	if got := quotaKeyFor("veh-001", t2); got != want2 {
		t.Errorf("quotaKeyFor = %q, want %q", got, want2)
	}
}

func TestPhotoUsecase_PresignUpload_QuotaTTL(t *testing.T) {
	pres := newMemPresigner()
	qs := newMemQuotaStore()
	vex := newMemVehicleExistence()
	vex.set("veh-001", "drv-001")
	ids := newCounterPhotoIDs("ph_")
	uc, _ := NewPhotoUsecase(pres, qs, vex, ids)

	_, err := uc.PresignUpload(context.Background(), "drv-001", "veh-001", "x.jpg")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	for k, ttl := range qs.ttls {
		if ttl != quotaTTL {
			t.Errorf("ttl for key %q = %s, want %s", k, ttl, quotaTTL)
		}
	}
}

// ---------------------------------------------------------------------------
// List.
// ---------------------------------------------------------------------------

func TestPhotoUsecase_List_HappyPath(t *testing.T) {
	pres := newMemPresigner()
	pres.listReturn = []string{
		"vehicles/veh-001/a.jpg",
		"vehicles/veh-001/b.jpg",
	}
	qs := newMemQuotaStore()
	vex := newMemVehicleExistence()
	vex.set("veh-001", "drv-001")
	ids := newCounterPhotoIDs("ph_")
	uc, _ := NewPhotoUsecase(pres, qs, vex, ids)

	entries, err := uc.List(context.Background(), "veh-001")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
	if entries[0].Key != "vehicles/veh-001/a.jpg" || entries[1].Key != "vehicles/veh-001/b.jpg" {
		t.Errorf("entry keys = %v", entries)
	}
	for i, e := range entries {
		if e.URL == "" {
			t.Errorf("entry[%d] URL empty", i)
		}
		if e.ExpiresAt == 0 {
			t.Errorf("entry[%d] ExpiresAt zero", i)
		}
	}
	// ListObjects should be called with the per-vehicle prefix.
	if len(pres.listCalls) != 1 || pres.listCalls[0] != "vehicles/veh-001/" {
		t.Errorf("list calls = %v, want one with prefix vehicles/veh-001/", pres.listCalls)
	}
}

func TestPhotoUsecase_List_Empty(t *testing.T) {
	pres := newMemPresigner()
	// no listReturn → ListObjects returns empty slice
	qs := newMemQuotaStore()
	vex := newMemVehicleExistence()
	vex.set("veh-001", "drv-001")
	ids := newCounterPhotoIDs("ph_")
	uc, _ := NewPhotoUsecase(pres, qs, vex, ids)

	entries, err := uc.List(context.Background(), "veh-001")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if entries == nil {
		t.Error("expected non-nil empty slice")
	}
	if len(entries) != 0 {
		t.Errorf("len = %d, want 0", len(entries))
	}
}

func TestPhotoUsecase_List_VehicleNotFound(t *testing.T) {
	pres := newMemPresigner()
	qs := newMemQuotaStore()
	vex := newMemVehicleExistence()
	ids := newCounterPhotoIDs("ph_")
	uc, _ := NewPhotoUsecase(pres, qs, vex, ids)

	_, err := uc.List(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected ErrNotFound")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want errors.Is ErrNotFound", err)
	}
	if len(pres.listCalls) != 0 {
		t.Errorf("ListObjects should not be called for missing vehicle")
	}
}

func TestPhotoUsecase_List_BlankVehicleIDIsValidationError(t *testing.T) {
	pres := newMemPresigner()
	qs := newMemQuotaStore()
	vex := newMemVehicleExistence()
	ids := newCounterPhotoIDs("ph_")
	uc, _ := NewPhotoUsecase(pres, qs, vex, ids)

	_, err := uc.List(context.Background(), "   ")
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

func TestPhotoUsecase_List_ListErrorSurfaces(t *testing.T) {
	pres := newMemPresigner()
	pres.listErr = errors.New("r2 boom")
	qs := newMemQuotaStore()
	vex := newMemVehicleExistence()
	vex.set("veh-001", "drv-001")
	ids := newCounterPhotoIDs("ph_")
	uc, _ := NewPhotoUsecase(pres, qs, vex, ids)

	_, err := uc.List(context.Background(), "veh-001")
	if err == nil {
		t.Fatal("expected error on ListObjects failure")
	}
	if !strings.Contains(err.Error(), "list objects") {
		t.Errorf("err = %v, want 'list objects' wrap", err)
	}
}

func TestPhotoUsecase_List_PresignGetFailureSkipsEntry(t *testing.T) {
	pres := newMemPresigner()
	pres.listReturn = []string{
		"vehicles/veh-001/a.jpg",
		"vehicles/veh-001/b.jpg",
		"vehicles/veh-001/c.jpg",
	}
	// Fail the middle one to verify the other two still come through.
	pres.presignGetErrs["vehicles/veh-001/b.jpg"] = errors.New("transient")
	qs := newMemQuotaStore()
	vex := newMemVehicleExistence()
	vex.set("veh-001", "drv-001")
	ids := newCounterPhotoIDs("ph_")
	uc, _ := NewPhotoUsecase(pres, qs, vex, ids)

	buf := captureLogs(t)
	entries, err := uc.List(context.Background(), "veh-001")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2 (skipped 1)", len(entries))
	}
	for _, e := range entries {
		if e.Key == "vehicles/veh-001/b.jpg" {
			t.Errorf("failed key should have been skipped: %+v", e)
		}
	}
	if !strings.Contains(buf.String(), "presign GET failed") {
		t.Errorf("expected warn log on presign skip, got %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// Filename sanitisation — direct unit table (cheap to enumerate the
// edge cases without setting up the whole usecase).
// ---------------------------------------------------------------------------

func TestSanitiseFilename(t *testing.T) {
	tests := []struct {
		in      string
		wantOK  bool
		wantOut string // only when wantOK
	}{
		{"front_view.jpg", true, "front_view.jpg"},
		{"front view.jpg", true, "front_view.jpg"},
		{"weird/name.jpg", true, "name.jpg"},
		{"../../etc/passwd", true, "passwd"},
		{"💥.png", true, "_.png"},
		{"x.jpg ", true, "x.jpg"}, // trimmed
		{"  ", false, ""},
		{"", false, ""},
		{".", false, ""},
		{"...", false, ""}, // trims to empty
		{strings.Repeat("a", maxFilenameLen) + ".jpg", false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			out, err := sanitiseFilename(tc.in)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("unexpected err for %q: %v", tc.in, err)
				}
				if out != tc.wantOut {
					t.Errorf("sanitise(%q) = %q, want %q", tc.in, out, tc.wantOut)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error for %q (got %q)", tc.in, out)
			}
			if !errors.Is(err, domain.ErrValidation) {
				t.Errorf("err = %v, want ErrValidation", err)
			}
		})
	}
}

// TestSanitiseFilename_RejectsWindowsReservedNames exercises the guard
// added by TASK-060 (security review L3). Windows reserved device names
// (CON, PRN, AUX, NUL, COM1-9, LPT1-9 — case-insensitive — with or
// without extension) must be rejected with ErrValidation so a future
// Windows-hosted consumer of these R2 objects cannot synthesise a name
// that shadows a device. The check happens after the existing
// sanitisation so the regex compares the canonical short name.
func TestSanitiseFilename_RejectsWindowsReservedNames(t *testing.T) {
	reserved := []string{
		"con", "CON", "Con",
		"prn", "PRN",
		"aux", "AUX",
		"nul", "NUL",
		"com1", "COM1", "com2", "com9",
		"lpt1", "LPT1", "lpt2", "lpt9",
		"con.jpg", "CON.JPG", "Con.txt",
		"prn.png", "aux.log",
		"com1.bin", "LPT5.dat",
	}
	for _, name := range reserved {
		t.Run(name, func(t *testing.T) {
			out, err := sanitiseFilename(name)
			if err == nil {
				t.Fatalf("sanitiseFilename(%q) = %q, want ErrValidation (reserved Windows name)", name, out)
			}
			if !errors.Is(err, domain.ErrValidation) {
				t.Errorf("sanitiseFilename(%q) err = %v, want ErrValidation", name, err)
			}
		})
	}
}

// TestSanitiseFilename_AllowsLookalikes confirms the regex is strict —
// names that LOOK like reserved devices but aren't (CON_, CONS, COM,
// LPT0, LPT10, README) must keep working. The reserved set is exactly
// CON/PRN/AUX/NUL and COM[1-9]/LPT[1-9] — nothing else.
func TestSanitiseFilename_AllowsLookalikes(t *testing.T) {
	ok := []string{
		"console.log",
		"prnted.jpg",
		"auxiliary.png",
		"null.dat",
		"com.png",  // no digit
		"com0.bin", // 0 is not 1-9
		"com10.bin",
		"lpt.png",
		"lpt0.dat",
		"lpt10.txt",
		"readme.md",
		"con_.jpg",
		"cons.txt",
	}
	for _, name := range ok {
		t.Run(name, func(t *testing.T) {
			out, err := sanitiseFilename(name)
			if err != nil {
				t.Fatalf("sanitiseFilename(%q) err = %v, want nil (not reserved)", name, err)
			}
			if out == "" {
				t.Fatalf("sanitiseFilename(%q) returned empty string", name)
			}
		})
	}
}
