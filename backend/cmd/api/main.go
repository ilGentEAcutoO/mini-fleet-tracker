// Package main is the entry point for the Mini Fleet Tracker HTTP API.
//
// Bootstrap responsibilities:
//   - parse the demo expiration timestamp (cost-protection layer 2)
//   - configure zerolog for the current environment
//   - wire a minimal Fiber app with /healthz
//   - listen on $PORT and shut down cleanly on SIGINT/SIGTERM
//
// Business handlers, repositories, and middlewares are added by later tasks.
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	fiberzerolog "github.com/gofiber/contrib/fiberzerolog"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// DemoExpiresAt is the hard cut-off for the public Zero Friction demo.
// After this instant the API short-circuits to 410 Gone; the gateway Worker
// and Durable Object enforce the same fence so cost cannot run away.
// See TASK-030 for the full revival workflow.
const DemoExpiresAt = "2026-05-31T23:59:59+07:00"

// gitCommit is stamped at build time via -ldflags "-X main.gitCommit=..."
// Falls back to the GIT_COMMIT env var, then to "dev".
var gitCommit = "dev"

// demoExpiresAt holds the parsed DemoExpiresAt constant. Populated in main().
var demoExpiresAt time.Time

func main() {
	configureLogger()

	var err error
	demoExpiresAt, err = time.Parse(time.RFC3339, DemoExpiresAt)
	if err != nil {
		// A malformed constant is a programmer error — fail loud.
		log.Fatal().Err(err).Str("value", DemoExpiresAt).Msg("invalid DemoExpiresAt constant")
	}
	if time.Now().After(demoExpiresAt) {
		log.Warn().Time("demo_expires_at", demoExpiresAt).Msg("demo window has already closed; API will refuse traffic in expiration middleware")
	}

	if envCommit := strings.TrimSpace(os.Getenv("GIT_COMMIT")); envCommit != "" && gitCommit == "dev" {
		gitCommit = envCommit
	}

	app := fiber.New(fiber.Config{
		AppName:               "mini-fleet-tracker-backend",
		DisableStartupMessage: true,
		ReadTimeout:           15 * time.Second,
		WriteTimeout:          15 * time.Second,
		IdleTimeout:           60 * time.Second,
	})

	app.Use(recover.New())
	app.Use(fiberzerolog.New(fiberzerolog.Config{
		Logger: &log.Logger,
		Fields: []string{"latency", "status", "method", "url", "ip", "ua"},
	}))

	app.Get("/healthz", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"status":          "ok",
			"commit":          gitCommit,
			"demo_expires_at": DemoExpiresAt,
		})
	})

	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8080"
	}

	// Listen in a goroutine so the main goroutine can block on the signal
	// channel. Errors from Listen are normal on shutdown (ErrServerClosed);
	// only fatal on unexpected failure.
	listenErr := make(chan error, 1)
	go func() {
		log.Info().Str("addr", ":"+port).Str("commit", gitCommit).Time("demo_expires_at", demoExpiresAt).Msg("listening")
		if err := app.Listen(":" + port); err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenErr <- err
		}
		close(listenErr)
	}()

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-shutdown:
		log.Info().Str("signal", sig.String()).Msg("shutting down")
	case err, ok := <-listenErr:
		if ok && err != nil {
			log.Error().Err(err).Msg("listener failed")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := app.ShutdownWithContext(ctx); err != nil {
		log.Error().Err(err).Msg("shutdown error")
		os.Exit(1)
	}
	log.Info().Msg("shutdown complete")
}

// configureLogger picks pretty console output for APP_ENV=development and JSON
// elsewhere, then applies the global level from LOG_LEVEL (default info).
func configureLogger() {
	zerolog.TimeFieldFormat = time.RFC3339

	env := strings.ToLower(strings.TrimSpace(os.Getenv("APP_ENV")))
	if env == "" {
		env = "development"
	}

	if env == "development" || env == "dev" {
		log.Logger = zerolog.New(zerolog.ConsoleWriter{
			Out:        os.Stderr,
			TimeFormat: time.RFC3339,
		}).With().Timestamp().Logger()
	} else {
		log.Logger = zerolog.New(os.Stderr).With().Timestamp().Str("env", env).Logger()
	}

	levelStr := strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL")))
	if levelStr == "" {
		levelStr = "info"
	}
	lvl, err := zerolog.ParseLevel(levelStr)
	if err != nil {
		log.Warn().Str("requested", levelStr).Msg("invalid LOG_LEVEL; falling back to info")
		lvl = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(lvl)
}
