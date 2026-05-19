// Package main seeds a fresh database with demo data:
//
//   - 1 manager  (manager@demo.local / SeedPassword!1)
//   - 1 driver   (driver@demo.local  / SeedPassword!1)
//   - 3 vehicles (DEMO-001, DEMO-002, DEMO-003, manager-owned)
//   - driver is assigned to DEMO-001
//
// The script is idempotent: entities that already exist are kept as-is
// and logged as "already present" rather than re-created. That makes
// `make seed` safe to run repeatedly against the same database — for
// the public demo we want a one-button reset that operators can fire
// without first wiping the schema.
//
// Reuses the production auth + vehicle usecases (rather than poking
// the repos directly) so the seeded entities go through the exact same
// validation, hashing, and ID-generation paths that real traffic does.
// If those rules ever change, the seed picks up the change for free.
//
// Example:
//
//	make seed
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/config"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
	d1repo "github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/repository/d1"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/usecase"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/pkg/cfclient"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/pkg/jwt"
)

// seedPassword is the plaintext password used for BOTH demo accounts.
// It satisfies the auth usecase's minimum-length policy and matches
// the credentials printed at the bottom of the seed run so an operator
// or reviewer can copy-paste straight into the login form.
//
// Safety note: this is a public-demo seed, not a production helper.
// The plaintext is checked into source on purpose so the README and
// CV reviewer can use the same credentials without coordinating a
// secrets handoff. Anyone with these credentials can write positions
// for DEMO-001 — that is the intended demo behaviour.
const seedPassword = "SeedPassword!1"

// seedTimeout caps the total wall-clock budget of one `make seed` run.
// All work happens inside one ctx so a hung CF dependency cannot wedge
// CI for minutes.
const seedTimeout = 30 * time.Second

// platePlan is the static list of vehicles the seed creates. Pulled
// out as a top-level var so the README can grep for the canonical
// list and the test (if any) can assert against the same constants.
//
// Index 0 is the driver-assigned vehicle. The remaining rows stay
// unassigned so the manager UI has both states to render against.
var platePlan = []struct {
	Plate    string
	Model    string
	Assigned bool
}{
	{"DEMO-001", "Toyota Hilux", true},
	{"DEMO-002", "Isuzu D-Max", false},
	{"DEMO-003", "Ford Ranger", false},
}

func main() {
	// Pretty console output for a CLI tool — JSON-on-stderr is the
	// production logging shape but a one-shot seed is meant to be
	// read by a human, not a log pipeline.
	log.Logger = zerolog.New(zerolog.ConsoleWriter{
		Out:        os.Stderr,
		TimeFormat: time.RFC3339,
	}).With().Timestamp().Logger()

	cfg, err := config.Load()
	if err != nil {
		log.Fatal().Err(err).Msg("config load")
	}

	d1Client, err := cfclient.NewD1Client(cfclient.D1Config{
		AccountID:  cfg.CFAccountID,
		DatabaseID: cfg.D1DatabaseID,
		APIToken:   cfg.CFAPIToken,
	})
	if err != nil {
		log.Fatal().Err(err).Msg("d1 client")
	}

	// We need a KVClient to satisfy the AuthUsecase constructor's
	// blocklist contract, even though the seed never calls Logout.
	// The sessions namespace is the safe choice — always populated
	// in prod and the only one with a strict ID in dev.
	kvSessions, err := cfclient.NewKVClient(cfclient.KVConfig{
		AccountID:   cfg.CFAccountID,
		NamespaceID: cfg.KVSessionsNamespaceID,
		APIToken:    cfg.CFAPIToken,
	})
	if err != nil {
		log.Fatal().Err(err).Msg("kv client")
	}

	// JWT signer is also required by the auth usecase. The token TTL
	// is irrelevant for the seed (we never issue a token), but the
	// signer still needs a positive value to construct successfully.
	signer, err := jwt.NewSigner(cfg.JWTSecret, time.Hour)
	if err != nil {
		log.Fatal().Err(err).Msg("jwt signer")
	}

	ctx, cancel := context.WithTimeout(context.Background(), seedTimeout)
	defer cancel()

	// Apply migrations so we don't seed against a missing schema. The
	// migrator is a no-op for already-applied versions, so this stays
	// idempotent end-to-end.
	mig := d1repo.NewMigrator(d1Client)
	if err = mig.Apply(ctx); err != nil {
		log.Fatal().Err(err).Msg("migrate")
	}

	// --- Users ---------------------------------------------------------
	driverRepo := d1repo.NewDriverRepo(d1Client)
	authUC, err := usecase.NewAuthUsecase(
		driverRepo,
		signer,
		kvSessions,
		usecase.IDGeneratorFunc(uuid.NewString),
	)
	if err != nil {
		log.Fatal().Err(err).Msg("auth usecase")
	}

	manager, err := registerIfMissing(ctx, authUC, driverRepo,
		"manager@demo.local", seedPassword, "Demo Manager", domain.RoleManager)
	if err != nil {
		log.Fatal().Err(err).Msg("seed manager")
	}
	log.Info().Str("id", manager.ID).Str("email", manager.Email).Str("role", "manager").Msg("manager ready")

	driver, err := registerIfMissing(ctx, authUC, driverRepo,
		"driver@demo.local", seedPassword, "Demo Driver", domain.RoleDriver)
	if err != nil {
		log.Fatal().Err(err).Msg("seed driver")
	}
	log.Info().Str("id", driver.ID).Str("email", driver.Email).Str("role", "driver").Msg("driver ready")

	// --- Vehicles ------------------------------------------------------
	vehicleRepo := d1repo.NewVehicleRepo(d1Client)
	vehicleUC, err := usecase.NewVehicleUsecase(vehicleRepo, usecase.IDGeneratorFunc(uuid.NewString))
	if err != nil {
		log.Fatal().Err(err).Msg("vehicle usecase")
	}

	for _, p := range platePlan {
		assignedTo := ""
		if p.Assigned {
			assignedTo = driver.ID
		}
		if err = ensureVehicle(ctx, vehicleUC, p.Plate, p.Model, assignedTo); err != nil {
			log.Fatal().Err(err).Str("plate", p.Plate).Msg("seed vehicle")
		}
		log.Info().Str("plate", p.Plate).Str("model", p.Model).Bool("assigned", p.Assigned).Msg("vehicle ready")
	}

	log.Info().Msg("seed complete")
	fmt.Println()
	fmt.Println("Demo credentials:")
	fmt.Printf("  Manager:  manager@demo.local / %s\n", seedPassword)
	fmt.Printf("  Driver:   driver@demo.local  / %s  (assigned to DEMO-001)\n", seedPassword)
}

// registerIfMissing fetches the driver by email first and, if present,
// returns it without touching the database. This is the cheaper of the
// two idempotency strategies (the alternative is calling Register and
// swallowing ErrAlreadyExists — that path still pays the ~100ms argon2
// hash cost on every re-run, which adds up when the seed is part of a
// rapid local dev loop).
//
// On a fresh database, GetByEmail returns ErrNotFound; we fall through
// to Register. Any other error from the repository bubbles up so the
// caller can fail fast.
func registerIfMissing(
	ctx context.Context,
	uc *usecase.AuthUsecase,
	repo *d1repo.DriverRepo,
	email, password, name string,
	role domain.Role,
) (*domain.Driver, error) {
	existing, err := repo.GetByEmail(ctx, email)
	if err == nil && existing != nil {
		return existing, nil
	}
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return nil, fmt.Errorf("lookup %s: %w", email, err)
	}
	return uc.Register(ctx, email, password, name, role)
}

// ensureVehicle creates a vehicle and treats domain.ErrAlreadyExists
// (the canonical "this plate is taken" outcome) as success. Any other
// error bubbles up. We accept that "already exists" silently skips
// the driver-assignment update — for the demo this is fine because
// the only re-run path is during local iteration where the assignment
// is already correct.
func ensureVehicle(
	ctx context.Context,
	uc *usecase.VehicleUsecase,
	plate, model, driverID string,
) error {
	_, err := uc.Create(ctx, usecase.CreateVehicleInput{
		PlateNumber: plate,
		Model:       model,
		DriverID:    driverID,
	})
	if err == nil {
		return nil
	}
	if errors.Is(err, domain.ErrAlreadyExists) {
		return nil
	}
	return err
}
