package handler

import (
	"context"
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"
)

// HealthzHandler exposes liveness + dependency-status checks. Each
// dependency is pinged with a short timeout; a failure renders the
// component status as "fail" but does NOT change the overall HTTP
// status — operators need the granular signal to triage (a load
// balancer that flips an instance out of rotation on a single bad
// dep check would amplify partial outages instead of containing them).
//
// The endpoint is wired into bootstrap.go as `GET /healthz` and is
// fronted only by the per-IP rate limiter; no auth, no CSRF. Response
// shape is intentionally identical to the inline closure it replaced
// (`status`, `commit`, `demo_expires_at`) with two new per-dep keys
// (`db`, `kv`) appended.
type HealthzHandler struct {
	d1     d1Pinger
	kv     kvPinger
	commit string
	expiry string
}

// d1Pinger is the minimum surface healthz needs from the D1 client.
// Declaring it here rather than re-using the migrator's Executor keeps
// the handler's dependency graph trim — we only need Exec for a
// `SELECT 1` round-trip. The real *cfclient.D1Client satisfies this
// directly, and tests inject a tiny stub.
type d1Pinger interface {
	Exec(ctx context.Context, sql string, args ...any) error
}

// kvPinger is the minimum surface healthz needs from a KV client. Like
// d1Pinger, we use the consumer-site interface idiom so tests do not
// need to spin up an httptest server just to exercise the failure path.
// The real *cfclient.KVClient.Get satisfies it directly.
type kvPinger interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
}

// healthzPingKey is the well-known KV key the handler reads on every
// /healthz call. It does NOT need to exist — KVClient.Get returns
// (nil, false, nil) for an absent key, which we treat as success. The
// only signal we care about is whether the round-trip itself succeeded
// (auth ok, namespace reachable, HTTP transport healthy). If the
// operator wants to make the check more strict in future they can
// pre-populate this key and assert the value matches.
const healthzPingKey = "healthz-ping"

// healthzTimeout caps both dep pings combined. Short enough that a
// stalled dep cannot wedge a synthetic monitor for minutes; long enough
// that a marginally slow CF API does not constantly flap to "fail".
const healthzTimeout = 2 * time.Second

// NewHealthzHandler validates its inputs and returns a ready-to-route
// handler. Returns an error rather than panicking so the boot sequence
// surfaces a misconfiguration in a structured log line. The commit and
// expiry strings are echoed verbatim — they are stamped at build time
// and parsed once at boot, so the handler treats them as opaque.
func NewHealthzHandler(d1 d1Pinger, kv kvPinger, commit, expiry string) (*HealthzHandler, error) {
	if d1 == nil {
		return nil, errors.New("healthz handler: d1 pinger is required")
	}
	if kv == nil {
		return nil, errors.New("healthz handler: kv pinger is required")
	}
	return &HealthzHandler{
		d1:     d1,
		kv:     kv,
		commit: commit,
		expiry: expiry,
	}, nil
}

// Check serves GET /healthz. Both dep pings are performed under a
// shared 2s timeout derived from the request context, so a per-call
// cancellation (Fiber tears down the request) propagates correctly.
//
// The HTTP status is always 200, even when a dep ping fails. Operators
// consume the JSON body to triage: a green `status` means everything
// is happy, "degraded" means at least one dep failed, and the per-dep
// `db`/`kv` keys pinpoint which one. This mirrors a /readyz vs /healthz
// split in spirit while keeping the demo to a single endpoint.
func (h *HealthzHandler) Check(c *fiber.Ctx) error {
	ctx, cancel := context.WithTimeout(c.UserContext(), healthzTimeout)
	defer cancel()

	dbStatus := "ok"
	if err := h.d1.Exec(ctx, "SELECT 1"); err != nil {
		dbStatus = "fail"
	}

	kvStatus := "ok"
	// We ignore the (value, found) return — a missing key is a perfectly
	// healthy outcome. Only transport / auth failures matter here.
	if _, _, err := h.kv.Get(ctx, healthzPingKey); err != nil {
		kvStatus = "fail"
	}

	overall := "ok"
	if dbStatus != "ok" || kvStatus != "ok" {
		overall = "degraded"
	}

	return c.JSON(fiber.Map{
		"status":          overall,
		"db":              dbStatus,
		"kv":              kvStatus,
		"commit":          h.commit,
		"demo_expires_at": h.expiry,
	})
}
