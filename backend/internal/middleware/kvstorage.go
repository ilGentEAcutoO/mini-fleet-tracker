package middleware

import (
	"context"
	"errors"
	"time"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/pkg/cfclient"
)

// KVStorage adapts a *cfclient.KVClient to the RateLimiterStorage contract.
//
// The wrapper is intentionally thin: Read maps a missing key to (nil, 0,
// nil) and a hit to (value, 0, nil). The Cloudflare KV REST API does not
// surface TTL-remaining on Get, so the ttlRemaining return is always
// zero; the rate-limit logic uses elapsed time from the bucket state for
// refill calculations anyway, so the missing field is not load-bearing.
//
// Write delegates to KVClient.Put with the caller's TTL. KVClient rounds
// any non-zero TTL up to its 60s minimum (Cloudflare's hard limit) so
// callers never need to special-case the small-window edge.
//
// Implements RateLimiterStorage at the bottom of the file via a static
// interface assertion so a contract drift in either direction (this
// wrapper, or the storage interface) fails to compile.
type KVStorage struct {
	kv *cfclient.KVClient
}

// NewKVStorage validates kv and returns a ready-to-use storage adapter.
// Returns an error rather than panicking so the wiring layer's main()
// can fail fast with a structured log line.
func NewKVStorage(kv *cfclient.KVClient) (*KVStorage, error) {
	if kv == nil {
		return nil, errors.New("rate-limit kv storage: kv client is required")
	}
	return &KVStorage{kv: kv}, nil
}

// Read satisfies RateLimiterStorage.Read. A missing key returns
// (nil, 0, nil); any other error is propagated so the middleware can
// fail open.
func (s *KVStorage) Read(ctx context.Context, key string) ([]byte, time.Duration, error) {
	value, found, err := s.kv.Get(ctx, key)
	if err != nil {
		return nil, 0, err
	}
	if !found {
		return nil, 0, nil
	}
	return value, 0, nil
}

// Write satisfies RateLimiterStorage.Write. The TTL is passed through
// verbatim; KVClient enforces Cloudflare's 60s minimum.
func (s *KVStorage) Write(ctx context.Context, key string, state []byte, ttl time.Duration) error {
	return s.kv.Put(ctx, key, state, ttl)
}

// Compile-time interface assertion.
var _ RateLimiterStorage = (*KVStorage)(nil)
