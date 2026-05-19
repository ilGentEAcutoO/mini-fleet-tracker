package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/config"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/handler"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/middleware"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/publisher"
	d1repo "github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/repository/d1"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/usecase"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/pkg/cfclient"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/pkg/jwt"
)

// accessTokenTTL is the default JWT lifetime. Pinned at one hour for the
// demo — long enough that drivers do not need to re-auth mid-shift, short
// enough that a stolen cookie is not a perpetual liability. Logout also
// blocklists the JTI, so the practical exposure of a compromised token
// is "time until victim notices and clicks logout", not the full hour.
const accessTokenTTL = time.Hour

// setupApp wires every dependency the API needs and returns a configured
// Fiber app along with a cleanup function the caller must invoke on
// shutdown. Errors here are programmer / operator errors (bad config,
// failed migrations); the binary exits non-zero so the operator sees
// them in their first deploy log line, not after a rolling restart cycle.
//
// The cleanup func is non-nil even on error, so defer-cleanup is safe;
// it is currently a no-op because every dependency we build is either
// reused goroutine-free or has no Close method, but the slot exists so
// future additions (DB pool, message bus client) can register cleanup
// without changing main.go.
func setupApp(cfg *config.Config) (*fiber.App, func(), error) {
	cleanup := func() {}
	if cfg == nil {
		return nil, cleanup, errors.New("setupApp: nil config")
	}

	// --- Infrastructure clients -----------------------------------------
	d1Client, err := cfclient.NewD1Client(cfclient.D1Config{
		AccountID:  cfg.CFAccountID,
		DatabaseID: cfg.D1DatabaseID,
		APIToken:   cfg.CFAPIToken,
	})
	if err != nil {
		return nil, cleanup, fmt.Errorf("setup: d1 client: %w", err)
	}

	kvSessions, err := cfclient.NewKVClient(cfclient.KVConfig{
		AccountID:   cfg.CFAccountID,
		NamespaceID: cfg.KVSessionsNamespaceID,
		APIToken:    cfg.CFAPIToken,
	})
	if err != nil {
		return nil, cleanup, fmt.Errorf("setup: kv sessions: %w", err)
	}

	kvRatelimits, err := cfclient.NewKVClient(cfclient.KVConfig{
		AccountID:   cfg.CFAccountID,
		NamespaceID: cfg.KVRatelimitsNamespaceID,
		APIToken:    cfg.CFAPIToken,
	})
	if err != nil {
		return nil, cleanup, fmt.Errorf("setup: kv ratelimits: %w", err)
	}

	// kvQuotas is not used by the auth layer but the wiring asks the
	// operator to provide it. Wire it now to fail fast if the namespace
	// is missing; later tasks (R2 photo quota) will consume it.
	_, err = cfclient.NewKVClient(cfclient.KVConfig{
		AccountID:   cfg.CFAccountID,
		NamespaceID: cfg.KVQuotasNamespaceID,
		APIToken:    cfg.CFAPIToken,
	})
	if err != nil {
		// Dev environments can have empty KV namespace IDs — only fail in prod.
		if !cfg.IsDevelopment() {
			return nil, cleanup, fmt.Errorf("setup: kv quotas: %w", err)
		}
		log.Warn().Err(err).Msg("kv quotas client not constructed; quotas features disabled in dev")
	}

	// --- Migrations -----------------------------------------------------
	// Run migrations at boot so a fresh D1 database is usable on first
	// request. In production this is a no-op for already-applied versions.
	// Dev environments without CF credentials would explode here — we
	// skip the call in dev so `make run` works on a bare host.
	if !cfg.IsDevelopment() {
		mig := d1repo.NewMigrator(d1Client)
		migCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		migErr := mig.Apply(migCtx)
		cancel()
		if migErr != nil {
			return nil, cleanup, fmt.Errorf("setup: migrate: %w", migErr)
		}
	}

	// --- JWT signer + repos + usecase -----------------------------------
	signer, err := jwt.NewSigner(cfg.JWTSecret, accessTokenTTL)
	if err != nil {
		return nil, cleanup, fmt.Errorf("setup: jwt signer: %w", err)
	}

	driverRepo := d1repo.NewDriverRepo(d1Client)
	authUC, err := usecase.NewAuthUsecase(driverRepo, signer, kvSessions, usecase.IDGeneratorFunc(uuid.NewString))
	if err != nil {
		return nil, cleanup, fmt.Errorf("setup: auth usecase: %w", err)
	}

	// VehicleRepo doubles as the vehicleLookup PositionUsecase needs for
	// the driver-ownership check — its Get method satisfies that contract
	// directly, so we wire the same instance into both usecases.
	vehicleRepo := d1repo.NewVehicleRepo(d1Client)
	vehicleUC, err := usecase.NewVehicleUsecase(vehicleRepo, usecase.IDGeneratorFunc(uuid.NewString))
	if err != nil {
		return nil, cleanup, fmt.Errorf("setup: vehicle usecase: %w", err)
	}

	// PositionUsecase needs an EventPublisher to broadcast position.update
	// events to the FleetHub Durable Object. The production wiring is the
	// HMAC-signed POST handled by cfclient.DurableClient + publisher.New.
	//
	// Dev-environment fallback: when DO_PUBLISH_URL is unset AND we're in
	// development, fall back to NoopPublisher so `go run ./cmd/api` boots
	// without secrets. Positions are still durably persisted to D1; only
	// the WS broadcast is silently dropped. Mirrors the same allow-empty
	// pattern kvQuotas uses just above — fail-fast in prod, degrade
	// gracefully in dev so local hacking does not require a real Worker.
	var positionEventPublisher usecase.EventPublisher
	if cfg.DOPublishURL == "" && cfg.IsDevelopment() {
		log.Warn().Msg("DO_PUBLISH_URL not set; using NoopPublisher (positions will not broadcast to FleetHub)")
		positionEventPublisher = usecase.NoopPublisher{}
	} else {
		var doClient *cfclient.DurableClient
		doClient, err = cfclient.NewDurableClient(cfclient.DurableConfig{
			PublishURL: cfg.DOPublishURL,
			Secret:     cfg.InternalPublishSecret,
		})
		if err != nil {
			return nil, cleanup, fmt.Errorf("setup: durable client: %w", err)
		}
		var fleetPub *publisher.FleetPublisher
		fleetPub, err = publisher.New(doClient)
		if err != nil {
			return nil, cleanup, fmt.Errorf("setup: fleet publisher: %w", err)
		}
		positionEventPublisher = fleetPub
	}

	positionRepo := d1repo.NewPositionRepo(d1Client)
	positionUC, err := usecase.NewPositionUsecase(positionRepo, vehicleRepo, positionEventPublisher)
	if err != nil {
		return nil, cleanup, fmt.Errorf("setup: position usecase: %w", err)
	}

	// --- Rate-limit storage ---------------------------------------------
	rlStorage, err := middleware.NewKVStorage(kvRatelimits)
	if err != nil {
		return nil, cleanup, fmt.Errorf("setup: rate-limit storage: %w", err)
	}

	// --- HTTP layer -----------------------------------------------------
	authHandler, err := handler.NewAuthHandler(authUC, signer, handler.DefaultCookieAttrs(cfg.IsDevelopment()))
	if err != nil {
		return nil, cleanup, fmt.Errorf("setup: auth handler: %w", err)
	}

	vehicleHandler, err := handler.NewVehicleHandler(vehicleUC)
	if err != nil {
		return nil, cleanup, fmt.Errorf("setup: vehicle handler: %w", err)
	}

	positionHandler, err := handler.NewPositionHandler(positionUC)
	if err != nil {
		return nil, cleanup, fmt.Errorf("setup: position handler: %w", err)
	}

	// Healthz checks D1 + KV liveness on every call. We pass kvSessions
	// (not kvQuotas, which is nil-in-dev) because the sessions namespace
	// is always constructed above — choosing the always-built dep keeps
	// the handler usable in every environment without a nil-guard. The
	// dep pings live behind a 2s timeout; failures mark the per-dep
	// status as "fail" but the HTTP response is always 200 so operators
	// can read the granular signal.
	healthzHandler, err := handler.NewHealthzHandler(d1Client, kvSessions, gitCommit, DemoExpiresAt)
	if err != nil {
		return nil, cleanup, fmt.Errorf("setup: healthz handler: %w", err)
	}

	corsMiddleware, err := middleware.CORS(cfg.CORSOrigin)
	if err != nil {
		return nil, cleanup, fmt.Errorf("setup: cors: %w", err)
	}

	globalRL, err := middleware.NewGlobal(middleware.GlobalConfig{Storage: rlStorage})
	if err != nil {
		return nil, cleanup, fmt.Errorf("setup: global rate-limit: %w", err)
	}

	// Per-route rate limits. Login is the easy DoS / brute-force vector,
	// so the bucket is tight (5 attempts per 5 minutes per IP). Healthz
	// gets its own loose-but-non-zero cap (60/min) so a misbehaving
	// monitor cannot single-handedly burn the global budget.
	loginRL, err := middleware.NewPerIP(middleware.PerIPConfig{
		Storage:   rlStorage,
		KeyPrefix: "rl-login",
		Bucket:    middleware.Bucket{Capacity: 5, RefillRate: 5.0 / 300.0}, // 5 tokens per 5 minutes
		TTL:       10 * time.Minute,
	})
	if err != nil {
		return nil, cleanup, fmt.Errorf("setup: login rate-limit: %w", err)
	}

	registerRL, err := middleware.NewPerIP(middleware.PerIPConfig{
		Storage:   rlStorage,
		KeyPrefix: "rl-register",
		Bucket:    middleware.Bucket{Capacity: 5, RefillRate: 5.0 / 600.0}, // 5 per 10 minutes
		TTL:       15 * time.Minute,
	})
	if err != nil {
		return nil, cleanup, fmt.Errorf("setup: register rate-limit: %w", err)
	}

	healthzRL, err := middleware.NewPerIP(middleware.PerIPConfig{
		Storage:   rlStorage,
		KeyPrefix: "rl-healthz",
		Bucket:    middleware.Bucket{Capacity: 60, RefillRate: 1.0},
		TTL:       2 * time.Minute,
	})
	if err != nil {
		return nil, cleanup, fmt.Errorf("setup: healthz rate-limit: %w", err)
	}

	// Per-driver position write cap (60/min/driver per plan). Must run AFTER
	// the auth middleware so the user ID is in c.Locals; NewPerUser falls
	// back to per-IP if no user ID is present, which protects unauth paths
	// but is not the intended use.
	positionRL, err := middleware.NewPerUser(middleware.PerUserConfig{
		Storage:   rlStorage,
		KeyPrefix: "rl-position",
		Bucket:    middleware.Bucket{Capacity: 60, RefillRate: 1.0},
		TTL:       2 * time.Minute,
	})
	if err != nil {
		return nil, cleanup, fmt.Errorf("setup: position rate-limit: %w", err)
	}

	// --- Fiber app + middleware chain -----------------------------------
	app := fiber.New(fiber.Config{
		AppName:               "mini-fleet-tracker-backend",
		DisableStartupMessage: true,
		ReadTimeout:           15 * time.Second,
		WriteTimeout:          15 * time.Second,
		IdleTimeout:           60 * time.Second,
		// Trust the Cloudflare gateway Worker so c.IP() returns the real
		// client IP for rate-limiting / logging. In dev we trust localhost
		// so reverse-proxies for testing work; in prod we trust 0.0.0.0/0
		// because the upstream is the CF Worker — there is no other path
		// for traffic to reach the Container in our deploy topology.
		EnableTrustedProxyCheck: true,
		TrustedProxies:          []string{"0.0.0.0/0", "::/0"},
		ProxyHeader:             "CF-Connecting-IP",
	})

	app.Use(recover.New())
	app.Use(middleware.RequestID())
	app.Use(corsMiddleware)
	app.Use(middleware.Logger())
	app.Use(globalRL)

	// --- Routes ---------------------------------------------------------
	// /healthz: no auth, no CSRF, but tight per-IP cap. The handler
	// pings D1 + KV under a 2s timeout and reports per-dep status
	// alongside the build commit and demo expiry.
	app.Get("/healthz", healthzRL, healthzHandler.Check)

	api := app.Group("/api")
	auth := api.Group("/auth")

	// Public auth endpoints — no auth cookie, no CSRF (CSRF cookie does
	// not exist yet), but per-IP rate-limited.
	auth.Post("/register", registerRL, authHandler.Register)
	auth.Post("/login", loginRL, authHandler.Login)

	// Authed endpoints — auth middleware checks cookie + KV blocklist,
	// then CSRF middleware enforces the double-submit pattern on
	// mutating methods.
	authMW := middleware.NewAuth(signer, authUC)
	csrfMW := middleware.NewCSRF()
	auth.Get("/me", authMW, authHandler.Me)
	auth.Post("/logout", authMW, csrfMW, authHandler.Logout)

	// Vehicle CRUD — manager-only enforcement lives inside the handler so
	// the route table here stays uniform: auth on every method, CSRF on
	// every mutating method, role gate inside the handler.
	vehicles := api.Group("/vehicles", authMW)
	vehicles.Get("/", vehicleHandler.List)
	vehicles.Get("/:id", vehicleHandler.Get)
	vehicles.Post("/", csrfMW, vehicleHandler.Create)
	vehicles.Patch("/:id", csrfMW, vehicleHandler.Update)
	vehicles.Delete("/:id", csrfMW, vehicleHandler.Delete)

	// Position writes — driver-only enforcement in the handler; per-user
	// rate limit on top of the global umbrella. The position usecase
	// publishes a position.update event to the FleetHub DO after a
	// successful save (best-effort; D1 is the source of truth).
	api.Post("/positions", authMW, csrfMW, positionRL, positionHandler.Write)

	return app, cleanup, nil
}
