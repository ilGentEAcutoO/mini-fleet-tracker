package cfclient

import "errors"

// Sentinel errors so callers can errors.Is() the common failure modes.
// We deliberately keep this set small — the Cloudflare APIs mostly fail
// on auth or quota; richer classification can be added when a real
// failure mode justifies it.
var (
	// ErrUnauthorized signals an authentication failure (HTTP 401/403, or
	// an explicit auth-related error message in a 4xx D1 response). The
	// caller should treat this as a configuration problem, not a retryable
	// transient.
	ErrUnauthorized = errors.New("cfclient: unauthorized")

	// ErrNotFound signals a missing resource (HTTP 404). For KV.Get this
	// is folded into the (value, found=false, err=nil) return; for D1 it
	// surfaces when the database/account ID is wrong.
	ErrNotFound = errors.New("cfclient: not found")
)
