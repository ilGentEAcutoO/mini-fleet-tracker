package usecase

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
)

// ---------------------------------------------------------------------------
// Hand-rolled mocks. Kept inline so each test case is one screen.
// ---------------------------------------------------------------------------

// memPositionRepo records every Insert call and lets tests inject errors.
// The repo populates p.ID (auto-increment) and p.CreatedAt before
// returning, mirroring the production repo's contract.
//
// GetMostRecentByVehicleBeforeID (added in TASK-020) walks the captured
// inserts in reverse insertion order and returns the first row whose
// vehicle_id matches and whose id is strictly less than excludeID. The
// in-memory order matches the production repo's id-DESC ordering
// because m.nextID is monotonic.
type memPositionRepo struct {
	mu        sync.Mutex
	inserts   []*domain.Position
	nextID    int64
	createdAt int64 // unix-ms; tests can pin this to verify the repo-stamped timestamp
	failWith  error // when non-nil, Insert returns this error and records nothing
	// failPrevWith lets tests inject a non-ErrNotFound error from the
	// transition-detection's previous-position lookup, so the warn-log
	// path is exercisable without forging a real DB error. Defaults to
	// nil; methods route ErrNotFound through the normal "no predecessor"
	// path regardless of this field.
	failPrevWith error
}

func newMemPositionRepo() *memPositionRepo {
	return &memPositionRepo{nextID: 1, createdAt: 1_700_000_000_000}
}

func (m *memPositionRepo) Insert(_ context.Context, p *domain.Position) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWith != nil {
		return m.failWith
	}
	p.ID = m.nextID
	p.CreatedAt = m.createdAt
	m.nextID++
	cp := *p
	m.inserts = append(m.inserts, &cp)
	return nil
}

func (m *memPositionRepo) GetMostRecentByVehicleBeforeID(
	_ context.Context,
	vehicleID string,
	excludeID int64,
) (*domain.Position, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failPrevWith != nil {
		return nil, m.failPrevWith
	}
	// Walk inserts in reverse so we find the largest id < excludeID for
	// the given vehicle. Mirrors the production ORDER BY id DESC LIMIT 1.
	for i := len(m.inserts) - 1; i >= 0; i-- {
		p := m.inserts[i]
		if p.VehicleID != vehicleID {
			continue
		}
		if p.ID >= excludeID {
			continue
		}
		cp := *p
		return &cp, nil
	}
	return nil, fmt.Errorf("previous position for vehicle %s: %w", vehicleID, domain.ErrNotFound)
}

// memVehicleLookup is the in-memory ownership lookup. Tests pre-load
// (vehicleID -> driverID) mappings; absent keys return ErrNotFound.
type memVehicleLookup struct {
	mu     sync.Mutex
	owners map[string]string // vehicleID -> driverID
	failOn map[string]error  // vehicleID -> error (takes precedence)
}

func newMemVehicleLookup() *memVehicleLookup {
	return &memVehicleLookup{owners: map[string]string{}, failOn: map[string]error{}}
}

func (m *memVehicleLookup) Set(vehicleID, driverID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.owners[vehicleID] = driverID
}

func (m *memVehicleLookup) Fail(vehicleID string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failOn[vehicleID] = err
}

func (m *memVehicleLookup) Get(_ context.Context, vehicleID string) (*domain.Vehicle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err, ok := m.failOn[vehicleID]; ok {
		return nil, err
	}
	driverID, ok := m.owners[vehicleID]
	if !ok {
		return nil, fmt.Errorf("vehicle %s: %w", vehicleID, domain.ErrNotFound)
	}
	return &domain.Vehicle{ID: vehicleID, DriverID: driverID}, nil
}

// spyPublisher captures every PublishPositionUpdate / PublishGeofenceAlert
// call and lets tests inject errors to exercise the best-effort
// log-on-failure paths.
//
// Geofence alerts (TASK-020) are captured separately from position
// updates because the two event types have different shapes and most
// tests only care about one or the other — keeping the slices
// separate means assertions stay focused.
type spyPublisher struct {
	mu       sync.Mutex
	calls    []*domain.Position // captured PublishPositionUpdate
	alerts   []capturedAlert    // captured PublishGeofenceAlert
	failWith error              // when non-nil, PublishPositionUpdate returns this error
	// alertFailWith lets tests force a failing geofence publish while
	// keeping the position publish path healthy. Defaults to nil.
	alertFailWith error
}

// capturedAlert is the immutable snapshot of one PublishGeofenceAlert
// call. Plain struct (not a pointer to the in-memory state) so the
// captured value cannot be mutated by later test code.
type capturedAlert struct {
	vehicleID string
	alertType string
	at        int64
}

func newSpyPublisher() *spyPublisher { return &spyPublisher{} }

func (s *spyPublisher) PublishPositionUpdate(_ context.Context, p *domain.Position) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *p
	s.calls = append(s.calls, &cp)
	return s.failWith
}

func (s *spyPublisher) PublishGeofenceAlert(_ context.Context, vehicleID, alertType string, at int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.alerts = append(s.alerts, capturedAlert{
		vehicleID: vehicleID,
		alertType: alertType,
		at:        at,
	})
	return s.alertFailWith
}

// captureLogs reroutes the package-level zerolog Logger to a buffer for
// the duration of the test, and restores the original Logger on cleanup.
// Returns the buffer so the test can inspect what was logged.
//
// Used by the publisher-best-effort test to verify the failure path
// emits a warn-level log instead of failing the request.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	prev := log.Logger
	buf := &bytes.Buffer{}
	log.Logger = zerolog.New(buf).Level(zerolog.WarnLevel)
	t.Cleanup(func() { log.Logger = prev })
	return buf
}

// ---------------------------------------------------------------------------
// Test helper.
// ---------------------------------------------------------------------------

// fixedNow returns a clock that always reports the same time. The
// freshness tests rely on this so the ±5-minute window is checked
// against a known reference, not the wall clock.
func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

// newTestPos constructs a PositionUsecase with in-memory dependencies
// and a fixed clock. Returns the usecase + every mock so tests can both
// drive inputs and inspect captured state.
func newTestPos(t *testing.T, now time.Time) (
	*PositionUsecase,
	*memPositionRepo,
	*memVehicleLookup,
	*spyPublisher,
) {
	t.Helper()
	repo := newMemPositionRepo()
	lookup := newMemVehicleLookup()
	pub := newSpyPublisher()
	uc, err := NewPositionUsecase(repo, lookup, pub)
	if err != nil {
		t.Fatalf("NewPositionUsecase: %v", err)
	}
	uc.now = fixedNow(now)
	return uc, repo, lookup, pub
}

// validInput returns a sensible default WritePositionInput pinned to
// recordedAt = now. Tests override individual fields to probe one
// validation branch at a time.
func validInput(vehicleID string, recordedAt int64) WritePositionInput {
	return WritePositionInput{
		VehicleID:  vehicleID,
		Lat:        13.7563, // Bangkok
		Lng:        100.5018,
		SpeedKmh:   42.5,
		RecordedAt: recordedAt,
	}
}

// ---------------------------------------------------------------------------
// Construction.
// ---------------------------------------------------------------------------

func TestNewPositionUsecase_RejectsNilDeps(t *testing.T) {
	repo := newMemPositionRepo()
	lookup := newMemVehicleLookup()
	pub := newSpyPublisher()

	cases := []struct {
		name string
		r    positionRepo
		v    vehicleLookup
		p    EventPublisher
	}{
		{"nil positions", nil, lookup, pub},
		{"nil vehicles", repo, nil, pub},
		{"nil publisher", repo, lookup, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewPositionUsecase(tc.r, tc.v, tc.p); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestNewPositionUsecase_NoopPublisherAccepted(t *testing.T) {
	// The wiring code in bootstrap.go will pass NoopPublisher{} until
	// TASK-014 lands. Make sure that path constructs cleanly.
	repo := newMemPositionRepo()
	lookup := newMemVehicleLookup()
	if _, err := NewPositionUsecase(repo, lookup, NoopPublisher{}); err != nil {
		t.Fatalf("NoopPublisher should construct: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Validation branches.
// ---------------------------------------------------------------------------

func TestWrite_VehicleIDRequired(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	uc, _, lookup, _ := newTestPos(t, now)
	lookup.Set("veh_1", "drv_1")

	in := validInput("", now.UnixMilli())
	_, err := uc.Write(context.Background(), "drv_1", in)
	if err == nil {
		t.Fatal("empty VehicleID must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

func TestWrite_LatBelowMinimum(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	uc, _, lookup, _ := newTestPos(t, now)
	lookup.Set("veh_1", "drv_1")

	in := validInput("veh_1", now.UnixMilli())
	in.Lat = -90.0001
	_, err := uc.Write(context.Background(), "drv_1", in)
	if err == nil {
		t.Fatal("lat < -90 must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

func TestWrite_LatAboveMaximum(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	uc, _, lookup, _ := newTestPos(t, now)
	lookup.Set("veh_1", "drv_1")

	in := validInput("veh_1", now.UnixMilli())
	in.Lat = 90.0001
	_, err := uc.Write(context.Background(), "drv_1", in)
	if err == nil {
		t.Fatal("lat > 90 must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

func TestWrite_LngBelowMinimum(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	uc, _, lookup, _ := newTestPos(t, now)
	lookup.Set("veh_1", "drv_1")

	in := validInput("veh_1", now.UnixMilli())
	in.Lng = -180.0001
	_, err := uc.Write(context.Background(), "drv_1", in)
	if err == nil {
		t.Fatal("lng < -180 must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

func TestWrite_LngAboveMaximum(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	uc, _, lookup, _ := newTestPos(t, now)
	lookup.Set("veh_1", "drv_1")

	in := validInput("veh_1", now.UnixMilli())
	in.Lng = 180.0001
	_, err := uc.Write(context.Background(), "drv_1", in)
	if err == nil {
		t.Fatal("lng > 180 must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

func TestWrite_SpeedKmhNegative(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	uc, _, lookup, _ := newTestPos(t, now)
	lookup.Set("veh_1", "drv_1")

	in := validInput("veh_1", now.UnixMilli())
	in.SpeedKmh = -0.1
	_, err := uc.Write(context.Background(), "drv_1", in)
	if err == nil {
		t.Fatal("negative speed must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

func TestWrite_SpeedKmhAboveMaximum(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	uc, _, lookup, _ := newTestPos(t, now)
	lookup.Set("veh_1", "drv_1")

	in := validInput("veh_1", now.UnixMilli())
	in.SpeedKmh = 500.0001
	_, err := uc.Write(context.Background(), "drv_1", in)
	if err == nil {
		t.Fatal("speed > 500 must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

func TestWrite_SpeedKmhZero_AllowedAsUnset(t *testing.T) {
	// Zero is the "unset" sentinel; it must NOT trip the range check.
	now := time.Unix(1_700_000_000, 0)
	uc, _, lookup, _ := newTestPos(t, now)
	lookup.Set("veh_1", "drv_1")

	in := validInput("veh_1", now.UnixMilli())
	in.SpeedKmh = 0
	p, err := uc.Write(context.Background(), "drv_1", in)
	if err != nil {
		t.Fatalf("speed=0 should be accepted as unset: %v", err)
	}
	if p.SpeedKmh != 0 {
		t.Fatalf("zero speed should pass through unchanged, got %f", p.SpeedKmh)
	}
}

func TestWrite_RecordedAtTooFarInFuture(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	uc, _, lookup, _ := newTestPos(t, now)
	lookup.Set("veh_1", "drv_1")

	// 6 minutes in the future = past the +5 minute window.
	future := now.Add(6 * time.Minute).UnixMilli()
	in := validInput("veh_1", future)
	_, err := uc.Write(context.Background(), "drv_1", in)
	if err == nil {
		t.Fatal("recorded_at too far in the future must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

func TestWrite_RecordedAtTooOld(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	uc, _, lookup, _ := newTestPos(t, now)
	lookup.Set("veh_1", "drv_1")

	// 6 minutes in the past = past the -5 minute window.
	old := now.Add(-6 * time.Minute).UnixMilli()
	in := validInput("veh_1", old)
	_, err := uc.Write(context.Background(), "drv_1", in)
	if err == nil {
		t.Fatal("recorded_at too old must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

func TestWrite_RecordedAtAtWindowEdge_Accepted(t *testing.T) {
	// Exactly at the ±5min boundary should be accepted (the gate is
	// strict "Before"/"After", not "Before-or-equal"/"After-or-equal").
	now := time.Unix(1_700_000_000, 0)
	uc, _, lookup, _ := newTestPos(t, now)
	lookup.Set("veh_1", "drv_1")

	atEdge := now.Add(5 * time.Minute).UnixMilli()
	in := validInput("veh_1", atEdge)
	if _, err := uc.Write(context.Background(), "drv_1", in); err != nil {
		t.Fatalf("recorded_at at +5min edge should be accepted: %v", err)
	}

	atEdgeBack := now.Add(-5 * time.Minute).UnixMilli()
	in.RecordedAt = atEdgeBack
	if _, err := uc.Write(context.Background(), "drv_1", in); err != nil {
		t.Fatalf("recorded_at at -5min edge should be accepted: %v", err)
	}
}

func TestWrite_EmptyDriverID(t *testing.T) {
	// Defensive: the handler should never call with an empty driver ID,
	// but if it did we want a 400, not a panic.
	now := time.Unix(1_700_000_000, 0)
	uc, _, _, _ := newTestPos(t, now)

	in := validInput("veh_1", now.UnixMilli())
	_, err := uc.Write(context.Background(), "   ", in)
	if err == nil {
		t.Fatal("empty driverID must fail")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Ownership branches.
// ---------------------------------------------------------------------------

func TestWrite_VehicleNotFound(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	uc, _, _, _ := newTestPos(t, now)
	// Note: no Set() — the lookup will report ErrNotFound.

	in := validInput("veh_does_not_exist", now.UnixMilli())
	_, err := uc.Write(context.Background(), "drv_1", in)
	if err == nil {
		t.Fatal("missing vehicle must fail")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestWrite_DriverNotOwner(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	uc, _, lookup, _ := newTestPos(t, now)
	lookup.Set("veh_1", "drv_owner") // owned by someone else

	in := validInput("veh_1", now.UnixMilli())
	_, err := uc.Write(context.Background(), "drv_intruder", in)
	if err == nil {
		t.Fatal("driver-not-owner must fail")
	}
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got: %v", err)
	}
}

func TestWrite_VehicleLookupReturnsInfraError(t *testing.T) {
	// A non-ErrNotFound error from the lookup must surface as a wrapped
	// internal error (handler maps to 500), not be silently swallowed.
	now := time.Unix(1_700_000_000, 0)
	uc, _, lookup, _ := newTestPos(t, now)
	lookup.Fail("veh_1", errors.New("D1 timeout"))

	in := validInput("veh_1", now.UnixMilli())
	_, err := uc.Write(context.Background(), "drv_1", in)
	if err == nil {
		t.Fatal("infra error must propagate")
	}
	if errors.Is(err, domain.ErrForbidden) || errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrValidation) {
		t.Fatalf("infra error must not be classified as a domain error: %v", err)
	}
}

func TestWrite_VehicleLookupNilNil(t *testing.T) {
	// Defensive: a misbehaving repo that returns (nil, nil) must surface
	// as ErrNotFound, not NPE.
	now := time.Unix(1_700_000_000, 0)
	repo := newMemPositionRepo()
	lookup := &nilNilLookup{}
	pub := newSpyPublisher()
	uc, err := NewPositionUsecase(repo, lookup, pub)
	if err != nil {
		t.Fatalf("NewPositionUsecase: %v", err)
	}
	uc.now = fixedNow(now)

	in := validInput("veh_x", now.UnixMilli())
	_, err = uc.Write(context.Background(), "drv_1", in)
	if err == nil {
		t.Fatal("nil-nil lookup must fail")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

// nilNilLookup is a deliberately-misbehaving lookup used by the
// defensive-NPE test above.
type nilNilLookup struct{}

func (nilNilLookup) Get(_ context.Context, _ string) (*domain.Vehicle, error) { return nil, nil }

// ---------------------------------------------------------------------------
// Repo / Publisher behaviour.
// ---------------------------------------------------------------------------

func TestWrite_RepoInsertErrorPropagates(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	uc, repo, lookup, _ := newTestPos(t, now)
	lookup.Set("veh_1", "drv_1")
	repo.failWith = errors.New("D1 unique constraint failed")

	in := validInput("veh_1", now.UnixMilli())
	_, err := uc.Write(context.Background(), "drv_1", in)
	if err == nil {
		t.Fatal("repo insert error must propagate")
	}
	if !strings.Contains(err.Error(), "D1 unique constraint failed") {
		t.Fatalf("expected wrapped repo error, got: %v", err)
	}
}

func TestWrite_PublisherErrorDoesNotFailRequest(t *testing.T) {
	// The publish step is best-effort: a publisher failure is logged
	// at warn level but does NOT fail the request. The position is
	// already durably written; the WS push is a notification convenience.
	now := time.Unix(1_700_000_000, 0)
	uc, _, lookup, pub := newTestPos(t, now)
	lookup.Set("veh_1", "drv_1")
	pub.failWith = errors.New("DO unreachable")

	// Capture warn-level logs so we can verify the failure path emits
	// a structured warning instead of silently swallowing the error.
	buf := captureLogs(t)

	in := validInput("veh_1", now.UnixMilli())
	p, err := uc.Write(context.Background(), "drv_1", in)
	if err != nil {
		t.Fatalf("publisher failure should not fail request: %v", err)
	}
	if p == nil || p.ID == 0 {
		t.Fatalf("position must be returned with ID populated even on publish failure: %+v", p)
	}

	// Verify the publisher WAS called (publish-shape check) and the
	// failure WAS logged.
	if len(pub.calls) != 1 {
		t.Fatalf("expected exactly 1 publisher call, got %d", len(pub.calls))
	}
	if pub.calls[0].VehicleID != "veh_1" {
		t.Errorf("publisher saw wrong vehicle: %+v", pub.calls[0])
	}
	logged := buf.String()
	if !strings.Contains(logged, "DO unreachable") {
		t.Errorf("expected publish failure log to mention the underlying error, got: %s", logged)
	}
	if !strings.Contains(logged, `"level":"warn"`) {
		t.Errorf("expected warn-level log entry, got: %s", logged)
	}
}

func TestWrite_PublisherCalledAfterPersist(t *testing.T) {
	// On the happy path the publisher MUST receive the persisted Position
	// (with ID populated), not the pre-insert value. This is the
	// contract TASK-014 relies on — the publisher should always see the
	// final DB-assigned ID.
	now := time.Unix(1_700_000_000, 0)
	uc, repo, lookup, pub := newTestPos(t, now)
	lookup.Set("veh_1", "drv_1")

	in := validInput("veh_1", now.UnixMilli())
	p, err := uc.Write(context.Background(), "drv_1", in)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(repo.inserts) != 1 {
		t.Fatalf("expected exactly 1 insert, got %d", len(repo.inserts))
	}
	if len(pub.calls) != 1 {
		t.Fatalf("expected exactly 1 publisher call, got %d", len(pub.calls))
	}
	if pub.calls[0].ID == 0 {
		t.Fatal("publisher must see the DB-assigned ID, not zero")
	}
	if pub.calls[0].ID != p.ID {
		t.Fatalf("publisher saw ID=%d but Write returned ID=%d", pub.calls[0].ID, p.ID)
	}
}

// ---------------------------------------------------------------------------
// Success — comprehensive field-by-field check.
// ---------------------------------------------------------------------------

func TestWrite_Success_AllFieldsPopulated(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	uc, repo, lookup, pub := newTestPos(t, now)
	lookup.Set("veh_1", "drv_1")

	in := WritePositionInput{
		VehicleID:  "veh_1",
		Lat:        13.7563,
		Lng:        100.5018,
		SpeedKmh:   42.5,
		RecordedAt: now.UnixMilli(),
	}
	p, err := uc.Write(context.Background(), "drv_1", in)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Echoed fields.
	if p.VehicleID != "veh_1" {
		t.Errorf("VehicleID: got %q, want veh_1", p.VehicleID)
	}
	if p.Lat != 13.7563 {
		t.Errorf("Lat: got %f, want 13.7563", p.Lat)
	}
	if p.Lng != 100.5018 {
		t.Errorf("Lng: got %f, want 100.5018", p.Lng)
	}
	if p.SpeedKmh != 42.5 {
		t.Errorf("SpeedKmh: got %f, want 42.5", p.SpeedKmh)
	}
	if p.RecordedAt != now.UnixMilli() {
		t.Errorf("RecordedAt: got %d, want %d", p.RecordedAt, now.UnixMilli())
	}

	// Populated by the repo.
	if p.ID == 0 {
		t.Error("ID must be populated by the repo")
	}
	if p.CreatedAt == 0 {
		t.Error("CreatedAt must be populated by the repo")
	}
	if p.CreatedAt != repo.createdAt {
		t.Errorf("CreatedAt: got %d, want %d (repo's stamp)", p.CreatedAt, repo.createdAt)
	}

	// The publisher saw the final, populated position.
	if len(pub.calls) != 1 {
		t.Fatalf("expected 1 publisher call, got %d", len(pub.calls))
	}
	if pub.calls[0].CreatedAt != p.CreatedAt {
		t.Errorf("publisher saw CreatedAt=%d, want %d", pub.calls[0].CreatedAt, p.CreatedAt)
	}
}

func TestWrite_NoopPublisher_RealConfigPath(t *testing.T) {
	// The production wiring uses NoopPublisher{} until TASK-014. Make
	// sure the happy path produces no spurious error and the
	// position is returned correctly even when no real broadcast occurs.
	now := time.Unix(1_700_000_000, 0)
	repo := newMemPositionRepo()
	lookup := newMemVehicleLookup()
	lookup.Set("veh_1", "drv_1")
	uc, err := NewPositionUsecase(repo, lookup, NoopPublisher{})
	if err != nil {
		t.Fatalf("NewPositionUsecase: %v", err)
	}
	uc.now = fixedNow(now)

	in := validInput("veh_1", now.UnixMilli())
	p, err := uc.Write(context.Background(), "drv_1", in)
	if err != nil {
		t.Fatalf("Write with NoopPublisher: %v", err)
	}
	if p == nil || p.ID == 0 {
		t.Fatalf("position must be returned with ID populated, got %+v", p)
	}
}

// ---------------------------------------------------------------------------
// TASK-020 — geofence transition detection. The Write path consults the
// fences lookup AFTER the position is durably inserted; if the
// inside/outside state changed across the previous reading and the
// current one, a geofence.alert is emitted via the publisher.
//
// Six cases cover the matrix:
//
//   - no fence configured → no alert
//   - prev inside,  curr inside  → no alert
//   - prev outside, curr outside → no alert
//   - prev inside,  curr outside → alert with type=exit
//   - prev outside, curr inside  → alert with type=enter
//   - no previous position (first ever) → no alert
//
// Plus the negative-path cases:
//   - fences-lookup nil-nil → no alert, no panic
//   - alert-publish failure → request still succeeds, warning logged
// ---------------------------------------------------------------------------

// memGeofenceLookup is the in-memory fence lookup used by the
// transition-detection tests. Tests pre-load (vehicleID -> fence)
// mappings; absent keys return ErrNotFound (matching the "no fence
// configured" semantic).
type memGeofenceLookup struct {
	mu       sync.Mutex
	fences   map[string]*domain.Geofence
	failWith error // when non-nil, GetByVehicle returns this error for any key
}

func newMemGeofenceLookup() *memGeofenceLookup {
	return &memGeofenceLookup{fences: map[string]*domain.Geofence{}}
}

func (m *memGeofenceLookup) Set(g *domain.Geofence) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fences[g.VehicleID] = g
}

func (m *memGeofenceLookup) GetByVehicle(_ context.Context, vehicleID string) (*domain.Geofence, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWith != nil {
		return nil, m.failWith
	}
	g, ok := m.fences[vehicleID]
	if !ok {
		return nil, fmt.Errorf("fence for vehicle %s: %w", vehicleID, domain.ErrNotFound)
	}
	cp := *g
	return &cp, nil
}

// bangkokFence returns a 500-m fence centred on the Wat Pho area. The
// inside/outside test points below are chosen to be unambiguously
// inside/outside this fence by a wide margin so the test does not flake
// on floating-point edge cases.
func bangkokFence(vehicleID string) *domain.Geofence {
	return &domain.Geofence{
		ID:        "fence_test",
		VehicleID: vehicleID,
		CenterLat: 13.7464,
		CenterLng: 100.4929,
		RadiusM:   500,
		CreatedAt: 1_700_000_000_000,
	}
}

// insidePoint and outsidePoint are coordinates clearly inside / outside
// the bangkokFence. Distance is on the order of ~10 m for inside and
// ~10 km for outside, so both classifications are unambiguous.
var (
	insidePoint  = struct{ lat, lng float64 }{lat: 13.7464, lng: 100.4929} // dead centre
	outsidePoint = struct{ lat, lng float64 }{lat: 13.8000, lng: 100.6000} // ~12 km NE
)

// newTestPosWithFences wires a PositionUsecase with the fence lookup
// option enabled. Returns the usecase + every mock so tests can drive
// inputs and inspect the captured alerts.
func newTestPosWithFences(t *testing.T, now time.Time) (
	*PositionUsecase,
	*memPositionRepo,
	*memVehicleLookup,
	*memGeofenceLookup,
	*spyPublisher,
) {
	t.Helper()
	repo := newMemPositionRepo()
	vehicles := newMemVehicleLookup()
	fences := newMemGeofenceLookup()
	pub := newSpyPublisher()
	uc, err := NewPositionUsecase(repo, vehicles, pub, WithGeofences(fences))
	if err != nil {
		t.Fatalf("NewPositionUsecase: %v", err)
	}
	uc.now = fixedNow(now)
	return uc, repo, vehicles, fences, pub
}

// writeAt is a tiny helper that wraps the usecase Write call with a
// pinned lat/lng, leaving the speed/freshness fields to their defaults.
// Tests use it to push the next reading at one of the canonical
// inside/outside coordinates.
func writeAt(
	t *testing.T,
	uc *PositionUsecase,
	vehicleID, driverID string,
	lat, lng float64,
	recordedAt int64,
) *domain.Position {
	t.Helper()
	p, err := uc.Write(context.Background(), driverID, WritePositionInput{
		VehicleID:  vehicleID,
		Lat:        lat,
		Lng:        lng,
		SpeedKmh:   25.0,
		RecordedAt: recordedAt,
	})
	if err != nil {
		t.Fatalf("Write(%f,%f): %v", lat, lng, err)
	}
	return p
}

func TestWrite_Geofence_NoFenceConfigured(t *testing.T) {
	// No fence Set() on the lookup → GetByVehicle returns ErrNotFound
	// → silently skipped. The position write still succeeds and only
	// the position.update event is emitted.
	now := time.Unix(1_700_000_000, 0)
	uc, _, vehicles, _, pub := newTestPosWithFences(t, now)
	vehicles.Set("veh_1", "drv_1")

	writeAt(t, uc, "veh_1", "drv_1", insidePoint.lat, insidePoint.lng, now.UnixMilli())
	writeAt(t, uc, "veh_1", "drv_1", outsidePoint.lat, outsidePoint.lng, now.UnixMilli())

	if len(pub.alerts) != 0 {
		t.Fatalf("no fence configured: expected 0 alerts, got %d (%+v)", len(pub.alerts), pub.alerts)
	}
	if len(pub.calls) != 2 {
		t.Fatalf("expected 2 position.update publishes, got %d", len(pub.calls))
	}
}

func TestWrite_Geofence_NoPreviousPosition(t *testing.T) {
	// First-ever position for the vehicle: GetMostRecentByVehicleBeforeID
	// returns ErrNotFound → no transition to compare → no alert.
	now := time.Unix(1_700_000_000, 0)
	uc, _, vehicles, fences, pub := newTestPosWithFences(t, now)
	vehicles.Set("veh_1", "drv_1")
	fences.Set(bangkokFence("veh_1"))

	// Even though the single reading is inside the fence, the first
	// reading has nothing to transition FROM, so no alert.
	writeAt(t, uc, "veh_1", "drv_1", insidePoint.lat, insidePoint.lng, now.UnixMilli())

	if len(pub.alerts) != 0 {
		t.Fatalf("first-ever position: expected 0 alerts, got %d (%+v)", len(pub.alerts), pub.alerts)
	}
}

func TestWrite_Geofence_BothInside_NoAlert(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	uc, _, vehicles, fences, pub := newTestPosWithFences(t, now)
	vehicles.Set("veh_1", "drv_1")
	fences.Set(bangkokFence("veh_1"))

	writeAt(t, uc, "veh_1", "drv_1", insidePoint.lat, insidePoint.lng, now.UnixMilli())
	// Slightly different inside point, still well within the fence.
	writeAt(t, uc, "veh_1", "drv_1", insidePoint.lat+0.0001, insidePoint.lng+0.0001, now.UnixMilli())

	if len(pub.alerts) != 0 {
		t.Fatalf("both-inside: expected 0 alerts, got %d (%+v)", len(pub.alerts), pub.alerts)
	}
}

func TestWrite_Geofence_BothOutside_NoAlert(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	uc, _, vehicles, fences, pub := newTestPosWithFences(t, now)
	vehicles.Set("veh_1", "drv_1")
	fences.Set(bangkokFence("veh_1"))

	writeAt(t, uc, "veh_1", "drv_1", outsidePoint.lat, outsidePoint.lng, now.UnixMilli())
	writeAt(t, uc, "veh_1", "drv_1", outsidePoint.lat+0.001, outsidePoint.lng+0.001, now.UnixMilli())

	if len(pub.alerts) != 0 {
		t.Fatalf("both-outside: expected 0 alerts, got %d (%+v)", len(pub.alerts), pub.alerts)
	}
}

func TestWrite_Geofence_ExitTransition(t *testing.T) {
	// Previous inside, current outside → alert with type=exit.
	now := time.Unix(1_700_000_000, 0)
	uc, _, vehicles, fences, pub := newTestPosWithFences(t, now)
	vehicles.Set("veh_1", "drv_1")
	fences.Set(bangkokFence("veh_1"))

	writeAt(t, uc, "veh_1", "drv_1", insidePoint.lat, insidePoint.lng, now.UnixMilli())
	current := writeAt(t, uc, "veh_1", "drv_1", outsidePoint.lat, outsidePoint.lng, now.UnixMilli())

	if len(pub.alerts) != 1 {
		t.Fatalf("expected exactly 1 alert, got %d (%+v)", len(pub.alerts), pub.alerts)
	}
	alert := pub.alerts[0]
	if alert.alertType != "exit" {
		t.Errorf("alert_type: got %q, want exit", alert.alertType)
	}
	if alert.vehicleID != "veh_1" {
		t.Errorf("vehicle_id: got %q, want veh_1", alert.vehicleID)
	}
	if alert.at != current.RecordedAt {
		t.Errorf("at: got %d, want %d (current.RecordedAt)", alert.at, current.RecordedAt)
	}
}

func TestWrite_Geofence_EnterTransition(t *testing.T) {
	// Previous outside, current inside → alert with type=enter.
	now := time.Unix(1_700_000_000, 0)
	uc, _, vehicles, fences, pub := newTestPosWithFences(t, now)
	vehicles.Set("veh_1", "drv_1")
	fences.Set(bangkokFence("veh_1"))

	writeAt(t, uc, "veh_1", "drv_1", outsidePoint.lat, outsidePoint.lng, now.UnixMilli())
	current := writeAt(t, uc, "veh_1", "drv_1", insidePoint.lat, insidePoint.lng, now.UnixMilli())

	if len(pub.alerts) != 1 {
		t.Fatalf("expected exactly 1 alert, got %d (%+v)", len(pub.alerts), pub.alerts)
	}
	alert := pub.alerts[0]
	if alert.alertType != "enter" {
		t.Errorf("alert_type: got %q, want enter", alert.alertType)
	}
	if alert.vehicleID != "veh_1" {
		t.Errorf("vehicle_id: got %q, want veh_1", alert.vehicleID)
	}
	if alert.at != current.RecordedAt {
		t.Errorf("at: got %d, want %d (current.RecordedAt)", alert.at, current.RecordedAt)
	}
}

func TestWrite_Geofence_NilFencesLookup_NoAlert(t *testing.T) {
	// Usecase wired without WithGeofences → transition-detection
	// silently skipped. This is the backwards-compatibility path so
	// existing callers (and the rest of the suite above) keep working.
	now := time.Unix(1_700_000_000, 0)
	repo := newMemPositionRepo()
	vehicles := newMemVehicleLookup()
	vehicles.Set("veh_1", "drv_1")
	pub := newSpyPublisher()
	uc, err := NewPositionUsecase(repo, vehicles, pub) // no WithGeofences
	if err != nil {
		t.Fatalf("NewPositionUsecase: %v", err)
	}
	uc.now = fixedNow(now)

	writeAt(t, uc, "veh_1", "drv_1", insidePoint.lat, insidePoint.lng, now.UnixMilli())
	writeAt(t, uc, "veh_1", "drv_1", outsidePoint.lat, outsidePoint.lng, now.UnixMilli())

	if len(pub.alerts) != 0 {
		t.Fatalf("nil-fences-lookup: expected 0 alerts, got %d", len(pub.alerts))
	}
}

func TestWrite_Geofence_AlertPublishFailureDoesNotFailRequest(t *testing.T) {
	// Best-effort: an alert publish failure must be logged at warn
	// level but the request still succeeds. The position is already
	// durably written; the alert is a notification convenience.
	now := time.Unix(1_700_000_000, 0)
	uc, _, vehicles, fences, pub := newTestPosWithFences(t, now)
	vehicles.Set("veh_1", "drv_1")
	fences.Set(bangkokFence("veh_1"))
	pub.alertFailWith = errors.New("DO unreachable")

	buf := captureLogs(t)

	writeAt(t, uc, "veh_1", "drv_1", insidePoint.lat, insidePoint.lng, now.UnixMilli())
	current, err := uc.Write(context.Background(), "drv_1", WritePositionInput{
		VehicleID:  "veh_1",
		Lat:        outsidePoint.lat,
		Lng:        outsidePoint.lng,
		SpeedKmh:   30,
		RecordedAt: now.UnixMilli(),
	})
	if err != nil {
		t.Fatalf("Write must succeed even when alert publish fails: %v", err)
	}
	if current == nil || current.ID == 0 {
		t.Fatal("position must be returned with ID populated")
	}
	if len(pub.alerts) != 1 {
		t.Fatalf("publisher should still be called for the exit transition, got %d alerts", len(pub.alerts))
	}
	logged := buf.String()
	if !strings.Contains(logged, "DO unreachable") {
		t.Errorf("expected warn log to mention underlying error, got: %s", logged)
	}
}

func TestWrite_Geofence_FenceLookupInfraError_LogsAndSkips(t *testing.T) {
	// A non-ErrNotFound error from the fence lookup must be logged at
	// warn level and the request still succeeds — same best-effort
	// contract as the alert publish.
	now := time.Unix(1_700_000_000, 0)
	uc, _, vehicles, fences, pub := newTestPosWithFences(t, now)
	vehicles.Set("veh_1", "drv_1")
	fences.failWith = errors.New("D1 timeout in fence lookup")

	buf := captureLogs(t)

	in := validInput("veh_1", now.UnixMilli())
	if _, err := uc.Write(context.Background(), "drv_1", in); err != nil {
		t.Fatalf("fence-lookup failure should not fail request: %v", err)
	}
	if len(pub.alerts) != 0 {
		t.Fatalf("expected 0 alerts on fence-lookup failure, got %d", len(pub.alerts))
	}
	logged := buf.String()
	if !strings.Contains(logged, "D1 timeout in fence lookup") {
		t.Errorf("expected warn log to mention underlying error, got: %s", logged)
	}
}
