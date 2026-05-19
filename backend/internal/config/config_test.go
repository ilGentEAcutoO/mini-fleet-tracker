package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearEnv unsets every env var bound by Load so each subtest starts from a
// known-empty baseline. t.Setenv inside subtests will then restore the
// original values at subtest completion.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range allBindings {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unsetenv %s: %v", key, err)
		}
	}
}

// useTempDir chdirs into a fresh temp dir for the duration of the test so
// no stray .env file in the working directory pollutes the run. Restores
// the original cwd via t.Cleanup. Returns the temp dir path for callers
// that need to write fixtures.
func useTempDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	prevWD, wdErr := os.Getwd()
	if wdErr != nil {
		t.Fatalf("getwd: %v", wdErr)
	}
	t.Cleanup(func() {
		if restoreErr := os.Chdir(prevWD); restoreErr != nil {
			t.Fatalf("restore cwd: %v", restoreErr)
		}
	})
	if chErr := os.Chdir(tmp); chErr != nil {
		t.Fatalf("chdir tmp: %v", chErr)
	}
	return tmp
}

func TestLoad_DevDefaults(t *testing.T) {
	clearEnv(t)
	useTempDir(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned error in dev mode: %v", err)
	}

	if got, want := cfg.AppEnv, "development"; got != want {
		t.Errorf("AppEnv = %q, want %q", got, want)
	}
	if got, want := cfg.Port, "8080"; got != want {
		t.Errorf("Port = %q, want %q", got, want)
	}
	if got, want := cfg.LogLevel, "info"; got != want {
		t.Errorf("LogLevel = %q, want %q", got, want)
	}
	if got, want := cfg.CORSOrigin, "http://localhost:3000"; got != want {
		t.Errorf("CORSOrigin = %q, want %q", got, want)
	}
	if !cfg.IsDevelopment() {
		t.Errorf("IsDevelopment() = false, want true")
	}
	// Required vars are allowed to be empty in dev.
	if cfg.JWTSecret != "" {
		t.Errorf("JWTSecret = %q, want empty", cfg.JWTSecret)
	}
}

func TestLoad_EnvOverridesDefaults(t *testing.T) {
	clearEnv(t)
	useTempDir(t)

	t.Setenv("APP_ENV", "development")
	t.Setenv("PORT", "9090")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("CORS_ORIGIN", "https://example.invalid")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got, want := cfg.Port, "9090"; got != want {
		t.Errorf("Port = %q, want %q", got, want)
	}
	if got, want := cfg.LogLevel, "debug"; got != want {
		t.Errorf("LogLevel = %q, want %q", got, want)
	}
	if got, want := cfg.CORSOrigin, "https://example.invalid"; got != want {
		t.Errorf("CORSOrigin = %q, want %q", got, want)
	}
}

func TestLoad_ProdMissingRequired_ReportsAll(t *testing.T) {
	clearEnv(t)
	useTempDir(t)

	t.Setenv("APP_ENV", "production")

	cfg, err := Load()
	if err == nil {
		t.Fatalf("Load succeeded in prod with no required vars; cfg=%+v", cfg)
	}

	msg := err.Error()
	for _, key := range requiredInProd {
		if !strings.Contains(msg, key) {
			t.Errorf("error message missing required key %q\nfull error:\n%s", key, msg)
		}
	}
	// errors.Join wraps each inner error; Unwrap() []error should expose
	// every missing var as a sibling for downstream introspection.
	type multi interface{ Unwrap() []error }
	var m multi
	if !errors.As(err, &m) {
		t.Fatalf("returned error does not expose Unwrap() []error: %T", err)
	}
	// header + one error per required var
	wantCount := len(requiredInProd) + 1
	if got := len(m.Unwrap()); got != wantCount {
		t.Errorf("Unwrap() returned %d errors, want %d", got, wantCount)
	}
}

func TestLoad_ProdAllRequiredPresent(t *testing.T) {
	clearEnv(t)
	useTempDir(t)

	t.Setenv("APP_ENV", "production")
	for _, key := range requiredInProd {
		t.Setenv(key, "value-"+key)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.IsDevelopment() {
		t.Errorf("IsDevelopment() = true, want false for APP_ENV=production")
	}
	if cfg.JWTSecret != "value-JWT_SECRET" {
		t.Errorf("JWTSecret = %q, want %q", cfg.JWTSecret, "value-JWT_SECRET")
	}
	if cfg.DOPublishURL != "value-DO_PUBLISH_URL" {
		t.Errorf("DOPublishURL = %q, want %q", cfg.DOPublishURL, "value-DO_PUBLISH_URL")
	}
}

func TestLoad_DevAllowsEmptyRequired(t *testing.T) {
	clearEnv(t)
	useTempDir(t)

	t.Setenv("APP_ENV", "dev")
	// Intentionally leave every required var empty.

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned error in dev: %v", err)
	}
	if !cfg.IsDevelopment() {
		t.Errorf("IsDevelopment() = false, want true for APP_ENV=dev")
	}
}

func TestLoad_DevReadsDotEnvFile(t *testing.T) {
	clearEnv(t)
	tmp := useTempDir(t)

	dotenv := `APP_ENV=development
PORT=7777
LOG_LEVEL=warn
CORS_ORIGIN=http://localhost:4000
JWT_SECRET=from-dotenv
`
	if err := os.WriteFile(filepath.Join(tmp, ".env"), []byte(dotenv), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Port, "7777"; got != want {
		t.Errorf("Port = %q, want %q (.env not picked up?)", got, want)
	}
	if got, want := cfg.LogLevel, "warn"; got != want {
		t.Errorf("LogLevel = %q, want %q", got, want)
	}
	if got, want := cfg.JWTSecret, "from-dotenv"; got != want {
		t.Errorf("JWTSecret = %q, want %q", got, want)
	}
}

func TestLoad_DevMissingDotEnvIsNotAnError(t *testing.T) {
	clearEnv(t)
	useTempDir(t) // empty dir — no .env present

	if _, err := Load(); err != nil {
		t.Fatalf("dev mode without .env should not error, got: %v", err)
	}
}

func TestLoad_StagingTreatedAsProd(t *testing.T) {
	clearEnv(t)
	useTempDir(t)

	t.Setenv("APP_ENV", "staging")
	if _, err := Load(); err == nil {
		t.Fatalf("staging without required vars should error")
	}
}
