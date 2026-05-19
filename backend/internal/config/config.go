// Package config centralizes runtime configuration for the Mini Fleet
// Tracker backend.
//
// Behaviour summary:
//
//   - Values come from environment variables. A `.env` file in the working
//     directory is read only when APP_ENV=development|dev (operator
//     convenience for `go run ./cmd/api`).
//   - A small set of fields have safe defaults so the dev binary boots
//     without any env at all (APP_ENV, PORT, LOG_LEVEL, CORS_ORIGIN).
//   - In any non-development environment, a strict required-var check
//     runs. Every missing required var is reported in one error
//     (aggregated with errors.Join) so operators see the whole gap in one
//     log line rather than fixing them one at a time.
//
// Callers should treat Config as immutable after Load returns.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
)

// Config mirrors backend/.env.example. Field tags are used by Viper to bind
// the environment variable names to struct fields via mapstructure.
type Config struct {
	// App
	AppEnv     string `mapstructure:"APP_ENV"`
	Port       string `mapstructure:"PORT"`
	LogLevel   string `mapstructure:"LOG_LEVEL"`
	CORSOrigin string `mapstructure:"CORS_ORIGIN"`

	// Auth
	JWTSecret             string `mapstructure:"JWT_SECRET"`
	InternalPublishSecret string `mapstructure:"INTERNAL_PUBLISH_SECRET"`

	// Cloudflare account
	CFAccountID string `mapstructure:"CF_ACCOUNT_ID"`
	CFAPIToken  string `mapstructure:"CF_API_TOKEN"`

	// D1
	D1DatabaseID string `mapstructure:"D1_DATABASE_ID"`

	// KV namespaces
	KVSessionsNamespaceID   string `mapstructure:"KV_SESSIONS_NAMESPACE_ID"`
	KVRatelimitsNamespaceID string `mapstructure:"KV_RATELIMITS_NAMESPACE_ID"`
	KVQuotasNamespaceID     string `mapstructure:"KV_QUOTAS_NAMESPACE_ID"`

	// R2 (S3-compatible endpoint)
	R2Endpoint        string `mapstructure:"R2_ENDPOINT"`
	R2AccessKeyID     string `mapstructure:"R2_ACCESS_KEY_ID"`
	R2SecretAccessKey string `mapstructure:"R2_SECRET_ACCESS_KEY"`
	R2BucketName      string `mapstructure:"R2_BUCKET_NAME"`

	// Durable Object publish endpoint
	DOPublishURL string `mapstructure:"DO_PUBLISH_URL"`
}

// IsDevelopment reports whether the config targets a development environment.
// "" is treated as dev so the binary starts on a bare host.
func (c *Config) IsDevelopment() bool {
	env := strings.ToLower(strings.TrimSpace(c.AppEnv))
	return env == "" || env == "development" || env == "dev"
}

// defaultEnv lists fields that have sensible fallbacks in development.
var defaultEnv = map[string]string{
	"APP_ENV":     "development",
	"PORT":        "8080",
	"LOG_LEVEL":   "info",
	"CORS_ORIGIN": "http://localhost:3000",
}

// allBindings is every env var Viper needs to know about so AutomaticEnv +
// Unmarshal will populate the struct fields. The order is kept in sync with
// the struct above for ease of review.
var allBindings = []string{
	"APP_ENV", "PORT", "LOG_LEVEL", "CORS_ORIGIN",
	"JWT_SECRET", "INTERNAL_PUBLISH_SECRET",
	"CF_ACCOUNT_ID", "CF_API_TOKEN",
	"D1_DATABASE_ID",
	"KV_SESSIONS_NAMESPACE_ID", "KV_RATELIMITS_NAMESPACE_ID", "KV_QUOTAS_NAMESPACE_ID",
	"R2_ENDPOINT", "R2_ACCESS_KEY_ID", "R2_SECRET_ACCESS_KEY", "R2_BUCKET_NAME",
	"DO_PUBLISH_URL",
}

// requiredInProd is the strict set enforced when APP_ENV != development.
// Order matters only for deterministic error output.
var requiredInProd = []string{
	"JWT_SECRET", "INTERNAL_PUBLISH_SECRET",
	"CF_ACCOUNT_ID", "CF_API_TOKEN",
	"D1_DATABASE_ID",
	"KV_SESSIONS_NAMESPACE_ID", "KV_RATELIMITS_NAMESPACE_ID", "KV_QUOTAS_NAMESPACE_ID",
	"R2_ENDPOINT", "R2_ACCESS_KEY_ID", "R2_SECRET_ACCESS_KEY", "R2_BUCKET_NAME",
	"DO_PUBLISH_URL",
}

// Load builds a Config from the process environment.
//
// In development mode (APP_ENV unset, "development", or "dev") a `.env`
// file in the working directory is layered under the live environment if
// present; missing required vars are tolerated so the binary can boot for
// local hacking.
//
// In any other environment, env vars are the sole source and every value
// in requiredInProd must be non-empty after trim. Missing vars are
// aggregated into a single error via errors.Join so the operator can fix
// the full set in one pass.
func Load() (*Config, error) {
	v := viper.New()

	for k, val := range defaultEnv {
		v.SetDefault(k, val)
	}

	// BindEnv explicitly so the empty-string case is distinguishable from
	// the unset case in tests, and so we don't rely on prefix-based magic.
	for _, key := range allBindings {
		if err := v.BindEnv(key); err != nil {
			return nil, fmt.Errorf("config: bind %s: %w", key, err)
		}
	}
	v.AutomaticEnv()

	// Pull APP_ENV from the bound env before we decide whether to read a
	// .env file. We use Viper here (not os.Getenv) so the default applies.
	appEnv := strings.ToLower(strings.TrimSpace(v.GetString("APP_ENV")))
	isDev := appEnv == "" || appEnv == "development" || appEnv == "dev"

	if isDev {
		v.SetConfigName(".env")
		v.SetConfigType("env")
		// Look in the working dir first, then the backend/ subdir so
		// `go run ./cmd/api` from either repo root or backend/ works.
		v.AddConfigPath(".")
		v.AddConfigPath("./backend")
		if cwd, err := os.Getwd(); err == nil {
			v.AddConfigPath(filepath.Dir(cwd))
		}
		if err := v.ReadInConfig(); err != nil {
			// Missing .env is fine in dev — that's the whole point of
			// having defaults. Anything else is real and worth surfacing.
			var nfe viper.ConfigFileNotFoundError
			if !errors.As(err, &nfe) && !os.IsNotExist(err) {
				return nil, fmt.Errorf("config: read .env: %w", err)
			}
		}
	}

	var c Config
	if err := v.Unmarshal(&c); err != nil {
		return nil, fmt.Errorf("config: unmarshal: %w", err)
	}

	if !c.IsDevelopment() {
		var missing []error
		for _, key := range requiredInProd {
			if strings.TrimSpace(v.GetString(key)) == "" {
				missing = append(missing, fmt.Errorf("missing required env var: %s", key))
			}
		}
		if len(missing) > 0 {
			return nil, errors.Join(append([]error{
				fmt.Errorf("config: %d required env var(s) missing in %q environment", len(missing), c.AppEnv),
			}, missing...)...)
		}
	}

	return &c, nil
}
