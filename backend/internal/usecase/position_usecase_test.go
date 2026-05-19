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
type memPositionRepo struct {
	mu        sync.Mutex
	inserts   []*domain.Position
	nextID    int64
	createdAt int64 // unix-ms; tests can pin this to verify the repo-stamped timestamp
	failWith  error // when non-nil, Insert returns this error and records nothing
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

// spyPublisher captures every PublishPositionUpdate call and lets tests
// inject errors to exercise the best-effort log-on-failure path.
type spyPublisher struct {
	mu       sync.Mutex
	calls    []*domain.Position
	failWith error // when non-nil, PublishPositionUpdate returns this error
}

func newSpyPublisher() *spyPublisher { return &spyPublisher{} }

func (s *spyPublisher) PublishPositionUpdate(_ context.Context, p *domain.Position) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *p
	s.calls = append(s.calls, &cp)
	return s.failWith
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
