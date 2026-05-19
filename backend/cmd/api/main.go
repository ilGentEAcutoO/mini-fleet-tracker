// Package main is the entry point for the Mini Fleet Tracker HTTP API.
//
// Bootstrap responsibilities:
//   - parse the demo expiration timestamp (cost-protection layer 2)
//   - configure zerolog for the current environment
//   - load config, build CF clients + repos + usecases + handlers, and
//     compose them into the Fiber app via setupApp in bootstrap.go
//   - listen on $PORT and shut down cleanly on SIGINT/SIGTERM
//
// All wiring lives in bootstrap.go so this file stays under ~100 lines and
// is easy to scan for the boot sequence at a glance.
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

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/config"
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

	cfg, err := config.Load()
	if err != nil {
		log.Fatal().Err(err).Msg("config load failed")
	}

	app, cleanup, err := setupApp(cfg)
	if err != nil {
		log.Fatal().Err(err).Msg("setup failed")
	}
	defer cleanup()

	port := strings.TrimSpace(cfg.Port)
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
