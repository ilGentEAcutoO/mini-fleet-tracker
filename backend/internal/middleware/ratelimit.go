package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog/log"
)

// Bucket parameters for a token-bucket rate limiter.
//
// The semantics are the classic continuous-refill bucket:
//   - the bucket starts full (Capacity tokens)
//   - each request consumes 1 token; if no tokens are available the request
//     is denied with 429
//   - between requests the bucket refills at RefillRate tokens per second,
//     capped at Capacity
//
// A request that succeeds returns Retry-After: 0 (omitted); a denied
// request returns Retry-After: ceil((1-tokens)/RefillRate) seconds — i.e.
// how long until at least one whole token is available again.
//
// The Bucket value is immutable after construction; concurrency safety
// comes from the storage layer's read-modify-write atomicity (KV uses
// last-writer-wins; that's acceptable for rate limiting because we never
// want to be more strict than the configuration).
type Bucket struct {
	// Capacity is the maximum number of tokens the bucket can hold.
	Capacity int
	// RefillRate is how many tokens are added per second.
	RefillRate float64
}

// Validate reports configuration errors. Mostly defensive — the wiring
// layer should construct Buckets with literal numbers.
func (b Bucket) Validate() error {
	if b.Capacity <= 0 {
		return errors.New("rate-limit: bucket capacity must be > 0")
	}
	if b.RefillRate <= 0 {
		return errors.New("rate-limit: bucket refill rate must be > 0")
	}
	return nil
}

// RateLimiterStorage is the narrow contract the rate-limit middlewares
// need from a key/value backend. The production binding wraps a
// cfclient.KVClient; tests use an in-memory map with a faked clock.
//
// Implementations are not required to be linearizable across replicas —
// rate limiting is "eventually consistent enough" by design. The KV
// backend, with its last-writer-wins semantics, can over-count under
// concurrent writes from the same IP, which is the safe direction.
type RateLimiterStorage interface {
	// Read returns the stored state and the time-to-live remaining on
	// the entry (if the backend exposes it; zero is acceptable). When the
	// key is absent, returns (nil, 0, nil).
	Read(ctx context.Context, key string) (state []byte, ttlRemaining time.Duration, err error)

	// Write persists state with the given TTL. A TTL of zero means
	// "no expiration"; callers should set a positive TTL so rate-limit
	// state does not accumulate forever.
	Write(ctx context.Context, key string, state []byte, ttl time.Duration) error
}

// bucketState is the per-key persisted state. JSON is used (rather than
// gob or a custom binary format) so KV values are debuggable from the
// Cloudflare dashboard's value preview.
type bucketState struct {
	Tokens       float64 `json:"tokens"`
	LastRefillMs int64   `json:"last_refill_ms"`
}

// rateLimitErrorBody is the standard 429 envelope.
type rateLimitErrorBody struct {
	Error      string `json:"error"`
	Message    string `json:"message"`
	RetryAfter int64  `json:"retry_after_seconds,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
}

// PerIPConfig is the constructor input for NewPerIP.
type PerIPConfig struct {
	Storage   RateLimiterStorage
	KeyPrefix string  // e.g. "rl-login" → keys become "rl-login:1.2.3.4"
	Bucket    Bucket  // capacity + refill rate
	TTL       time.Duration
	// Now is optional; tests inject a deterministic clock. Production
	// leaves it nil so time.Now is used.
	Now func() time.Time
}

// NewPerIP returns a Fiber middleware that enforces cfg.Bucket per
// c.IP() against cfg.Storage. The TTL on the persisted state should be
// long enough that a quiet client's bucket does not silently reset to
// Capacity between requests — but the worst case if it does is a single
// extra burst, so the exact value is not security-critical.
//
// Returns nil + an error when cfg is invalid so the wiring layer can
// fail fast at boot.
func NewPerIP(cfg PerIPConfig) (fiber.Handler, error) {
	if cfg.Storage == nil {
		return nil, errors.New("rate-limit: storage is required")
	}
	if strings.TrimSpace(cfg.KeyPrefix) == "" {
		return nil, errors.New("rate-limit: key prefix is required")
	}
	if err := cfg.Bucket.Validate(); err != nil {
		return nil, err
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 5 * time.Minute
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	return func(c *fiber.Ctx) error {
		ip := strings.TrimSpace(c.IP())
		if ip == "" {
			ip = "unknown"
		}
		key := cfg.KeyPrefix + ":" + ip
		return enforceBucket(c, cfg.Storage, key, cfg.Bucket, cfg.TTL, now)
	}, nil
}

// NewPerIPCriticalFailClosed is identical to NewPerIP except for the
// behaviour when the storage layer fails to read state. NewPerIP (and
// NewPerUser) admit the request on a storage error — the right call for
// non-critical routes because denying every request during a KV outage
// is worse than letting a few extra hit the global cap.
//
// For the login + register endpoints that calculus inverts. A 30s KV
// outage with the standard fail-open would let an attacker bypass the
// brute-force ceiling for that full window — long enough to test a few
// thousand passwords. Critical routes therefore fail CLOSED: a storage
// read error returns 503 (with Retry-After) so legitimate users see a
// retryable error and credential-stuffing attempts hit a wall.
//
// The constructor signature is identical to NewPerIP so callers in
// bootstrap.go can swap one for the other based on route sensitivity
// without re-shaping the wiring code. TASK-054 / security review M1.
func NewPerIPCriticalFailClosed(cfg PerIPConfig) (fiber.Handler, error) {
	if cfg.Storage == nil {
		return nil, errors.New("rate-limit: storage is required")
	}
	if strings.TrimSpace(cfg.KeyPrefix) == "" {
		return nil, errors.New("rate-limit: key prefix is required")
	}
	if err := cfg.Bucket.Validate(); err != nil {
		return nil, err
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 5 * time.Minute
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	return func(c *fiber.Ctx) error {
		ip := strings.TrimSpace(c.IP())
		if ip == "" {
			ip = "unknown"
		}
		key := cfg.KeyPrefix + ":" + ip
		return enforceBucketFailClosed(c, cfg.Storage, key, cfg.Bucket, cfg.TTL, now)
	}, nil
}

// PerUserConfig is the constructor input for NewPerUser.
type PerUserConfig struct {
	Storage   RateLimiterStorage
	KeyPrefix string
	Bucket    Bucket
	TTL       time.Duration
	Now       func() time.Time
}

// NewPerUser returns a middleware that enforces cfg.Bucket per
// authenticated user (from the AuthUserIDLocalsKey). Must run AFTER
// NewAuth — without an auth user ID the middleware degrades to per-IP
// (so unauthenticated requests are not silently un-throttled).
func NewPerUser(cfg PerUserConfig) (fiber.Handler, error) {
	if cfg.Storage == nil {
		return nil, errors.New("rate-limit: storage is required")
	}
	if strings.TrimSpace(cfg.KeyPrefix) == "" {
		return nil, errors.New("rate-limit: key prefix is required")
	}
	if err := cfg.Bucket.Validate(); err != nil {
		return nil, err
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 5 * time.Minute
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	return func(c *fiber.Ctx) error {
		uid := AuthUserIDFromCtx(c)
		var key string
		if uid != "" {
			key = cfg.KeyPrefix + ":u:" + uid
		} else {
			// Defensive fallback: if NewPerUser is wired before NewAuth by
			// mistake, do not silently disable the limit. Use the IP so
			// the chain still throttles, just on a different dimension.
			ip := strings.TrimSpace(c.IP())
			if ip == "" {
				ip = "unknown"
			}
			key = cfg.KeyPrefix + ":ip:" + ip
		}
		return enforceBucket(c, cfg.Storage, key, cfg.Bucket, cfg.TTL, now)
	}, nil
}

// enforceBucket is the shared core of NewPerIP / NewPerUser. It loads the
// current bucket state, refills based on elapsed time, decrements one
// token, and either persists + calls c.Next or returns 429.
func enforceBucket(
	c *fiber.Ctx,
	storage RateLimiterStorage,
	key string,
	bucket Bucket,
	ttl time.Duration,
	now func() time.Time,
) error {
	ctx := c.UserContext()
	nowT := now()
	nowMs := nowT.UnixMilli()

	raw, _, err := storage.Read(ctx, key)
	if err != nil {
		// Fail-OPEN: better to serve a request than to 500 every caller
		// because KV is having a bad minute. We log at WARN so operators
		// see the visibility loss in their dashboard before the next
		// brute-force probe — TASK-054 / security review M1. The KVStorage
		// adapter itself does NOT log because it doesn't know the
		// request scope; correlation is the middleware's job.
		log.Warn().
			Err(err).
			Str("request_id", RequestIDFromCtx(c)).
			Str("route", c.Path()).
			Str("ip", c.IP()).
			Str("rate_limit_key", key).
			Msg("rate-limit storage read failed; allowing request (fail-open)")
		return c.Next()
	}

	var state bucketState
	if len(raw) == 0 {
		// Fresh bucket: start full.
		state.Tokens = float64(bucket.Capacity)
		state.LastRefillMs = nowMs
	} else {
		if err := json.Unmarshal(raw, &state); err != nil {
			// Corrupt state — treat as fresh and overwrite.
			state.Tokens = float64(bucket.Capacity)
			state.LastRefillMs = nowMs
		}
	}

	// Refill based on elapsed time. Negative elapsed (clock skew) is
	// treated as zero so a backwards-running clock cannot inflate the
	// bucket.
	elapsedMs := nowMs - state.LastRefillMs
	if elapsedMs > 0 {
		state.Tokens += (float64(elapsedMs) / 1000.0) * bucket.RefillRate
		if state.Tokens > float64(bucket.Capacity) {
			state.Tokens = float64(bucket.Capacity)
		}
	}
	state.LastRefillMs = nowMs

	if state.Tokens < 1.0 {
		// Compute seconds until the bucket has at least one token.
		needed := 1.0 - state.Tokens
		retry := math.Ceil(needed / bucket.RefillRate)
		if retry < 1 {
			retry = 1
		}
		// Persist the (refilled-but-still-empty) state so subsequent
		// requests within the same window do not get a misleading reset.
		_ = persistBucket(ctx, storage, key, &state, ttl)
		return respondTooMany(c, int64(retry), "rate limit exceeded")
	}

	state.Tokens -= 1.0
	if err := persistBucket(ctx, storage, key, &state, ttl); err != nil {
		// Persistence failure: still admit (legitimate-traffic protection)
		// but log so operators see the count drift. TASK-054.
		log.Warn().
			Err(err).
			Str("request_id", RequestIDFromCtx(c)).
			Str("route", c.Path()).
			Str("ip", c.IP()).
			Str("rate_limit_key", key).
			Msg("rate-limit storage write failed; allowing request (counter may drift)")
		return c.Next()
	}
	return c.Next()
}

// enforceBucketFailClosed is the variant used by NewPerIPCriticalFailClosed.
// Differs from enforceBucket on exactly two paths:
//
//   - storage Read error → 503 (fail-CLOSED) instead of fail-open.
//   - storage Write error → still admits (state is in-memory at the write
//     site; the bucket itself rolled forward). Refusing a request whose
//     downstream check already passed would be incoherent — the cap was
//     consulted; only the persistence is flaky.
//
// 503 (not 429) because the failure is infra-level transient. SPAs treat
// 503 as retryable, which matches reality; 429 would tell them
// "you exceeded the limit" which is misleading.
func enforceBucketFailClosed(
	c *fiber.Ctx,
	storage RateLimiterStorage,
	key string,
	bucket Bucket,
	ttl time.Duration,
	now func() time.Time,
) error {
	ctx := c.UserContext()
	nowT := now()
	nowMs := nowT.UnixMilli()

	raw, _, err := storage.Read(ctx, key)
	if err != nil {
		// Fail-CLOSED — TASK-054 / security review M1. Log the storage
		// error then return 503 with a generous Retry-After. We use
		// 503 (not 429) so the SPA's "retryable infra error" pill is
		// shown rather than the "you hit the limit" pill — the cause
		// here is KV, not the client's request rate.
		log.Warn().
			Err(err).
			Str("request_id", RequestIDFromCtx(c)).
			Str("route", c.Path()).
			Str("ip", c.IP()).
			Str("rate_limit_key", key).
			Msg("rate-limit storage read failed; denying request (fail-closed for critical route)")
		c.Set(fiber.HeaderRetryAfter, "60")
		return c.Status(fiber.StatusServiceUnavailable).JSON(rateLimitErrorBody{
			Error:      "service_unavailable",
			Message:    "rate-limit storage unavailable; please retry",
			RetryAfter: 60,
			RequestID:  RequestIDFromCtx(c),
		})
	}

	var state bucketState
	if len(raw) == 0 {
		state.Tokens = float64(bucket.Capacity)
		state.LastRefillMs = nowMs
	} else {
		if err := json.Unmarshal(raw, &state); err != nil {
			// Corrupt state — treat as fresh and overwrite.
			state.Tokens = float64(bucket.Capacity)
			state.LastRefillMs = nowMs
		}
	}

	elapsedMs := nowMs - state.LastRefillMs
	if elapsedMs > 0 {
		state.Tokens += (float64(elapsedMs) / 1000.0) * bucket.RefillRate
		if state.Tokens > float64(bucket.Capacity) {
			state.Tokens = float64(bucket.Capacity)
		}
	}
	state.LastRefillMs = nowMs

	if state.Tokens < 1.0 {
		needed := 1.0 - state.Tokens
		retry := math.Ceil(needed / bucket.RefillRate)
		if retry < 1 {
			retry = 1
		}
		_ = persistBucket(ctx, storage, key, &state, ttl)
		return respondTooMany(c, int64(retry), "rate limit exceeded")
	}

	state.Tokens -= 1.0
	if err := persistBucket(ctx, storage, key, &state, ttl); err != nil {
		// Persistence flake: counter drifts by at most one, log + admit.
		// The Read succeeded so the cap was honoured.
		log.Warn().
			Err(err).
			Str("request_id", RequestIDFromCtx(c)).
			Str("route", c.Path()).
			Str("ip", c.IP()).
			Str("rate_limit_key", key).
			Msg("rate-limit storage write failed on critical route; allowing request (counter may drift)")
		return c.Next()
	}
	return c.Next()
}

// persistBucket marshals state to JSON and writes it with the given TTL.
func persistBucket(ctx context.Context, storage RateLimiterStorage, key string, state *bucketState, ttl time.Duration) error {
	body, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("rate-limit: marshal state: %w", err)
	}
	return storage.Write(ctx, key, body, ttl)
}

// respondTooMany writes the standard 429 with a Retry-After header.
func respondTooMany(c *fiber.Ctx, retryAfterSeconds int64, msg string) error {
	if retryAfterSeconds < 1 {
		retryAfterSeconds = 1
	}
	c.Set(fiber.HeaderRetryAfter, strconv.FormatInt(retryAfterSeconds, 10))
	return c.Status(fiber.StatusTooManyRequests).JSON(rateLimitErrorBody{
		Error:      "too_many_requests",
		Message:    msg,
		RetryAfter: retryAfterSeconds,
		RequestID:  RequestIDFromCtx(c),
	})
}
