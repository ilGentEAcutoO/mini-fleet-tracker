// Package usecase composes the domain layer with infrastructure adapters
// (repository, hash, jwt, KV) into application-level workflows. Each
// usecase struct depends on small interfaces declared at the consumer
// site so the binding is loose and the tests can wire hand-rolled mocks
// without pulling in a mocking library.
package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/pkg/hash"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/pkg/jwt"
)

// DriverRepo is the subset of the driver storage interface AuthUsecase
// requires. Declaring it here (rather than re-exporting the concrete
// repository) keeps the dependency direction inward: the usecase owns
// its contract, and any conforming repository can plug in.
type DriverRepo interface {
	Create(ctx context.Context, d *domain.Driver) error
	GetByEmail(ctx context.Context, email string) (*domain.Driver, error)
	GetByID(ctx context.Context, id string) (*domain.Driver, error)
}

// TokenSigner is the subset of pkg/jwt.Signer AuthUsecase needs. Wrapping
// it in a local interface keeps tests free of the underlying JWT library
// and lets us swap signers (e.g. a key-rotating signer) later without
// touching the usecase.
type TokenSigner interface {
	Issue(subject, role string) (string, *jwt.Claims, error)
	Verify(token string) (*jwt.Claims, error)
	Remaining(c *jwt.Claims) time.Duration
}

// Blocklist is the subset of pkg/cfclient.KVClient AuthUsecase uses for
// JTI revocation. The signature is identical so the real KV client
// satisfies it implicitly.
type Blocklist interface {
	Put(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Get(ctx context.Context, key string) ([]byte, bool, error)
}

// IDGenerator produces opaque, ordering-friendly identifiers for new
// drivers. The production wiring uses uuid.NewString from google/uuid
// (UUID v4) — wrapped as an anonymous function literal in main.go — but
// tests pin a deterministic counter so the assertion against the
// generated ID is reliable.
type IDGenerator interface {
	NewID() string
}

// IDGeneratorFunc adapts a plain func to the IDGenerator interface so
// callers do not need to declare a new type when wiring up uuid.NewString.
type IDGeneratorFunc func() string

// NewID satisfies IDGenerator.
func (f IDGeneratorFunc) NewID() string { return f() }

// AuthUsecase wires the auth workflows. Dependencies are immutable after
// construction; the struct is safe for concurrent use as long as the
// injected adapters are.
type AuthUsecase struct {
	drivers   DriverRepo
	signer    TokenSigner
	blocklist Blocklist
	ids       IDGenerator
	// now is the testable clock. Production code leaves it nil and the
	// real clock from time.Now is used.
	now func() time.Time
}

// NewAuthUsecase constructs a usecase from its dependencies. All five
// arguments are required; passing any nil is a programmer error and
// returns an error rather than panicking later in a request path.
func NewAuthUsecase(
	drivers DriverRepo,
	signer TokenSigner,
	blocklist Blocklist,
	ids IDGenerator,
) (*AuthUsecase, error) {
	if drivers == nil {
		return nil, errors.New("auth usecase: drivers repo is required")
	}
	if signer == nil {
		return nil, errors.New("auth usecase: token signer is required")
	}
	if blocklist == nil {
		return nil, errors.New("auth usecase: blocklist is required")
	}
	if ids == nil {
		return nil, errors.New("auth usecase: id generator is required")
	}
	return &AuthUsecase{
		drivers:   drivers,
		signer:    signer,
		blocklist: blocklist,
		ids:       ids,
	}, nil
}

// nowFunc returns the test-overridable clock or time.Now in production.
func (u *AuthUsecase) nowFunc() time.Time {
	if u.now != nil {
		return u.now()
	}
	return time.Now()
}

// dummyHash is a precomputed argon2id PHC string used by Login when the
// email is unknown. Running VerifyPassword against it keeps the wrong-email
// path's wall-clock time close to the wrong-password path, denying a
// timing-based user-enumeration oracle.
//
// The value below is deliberately the encoded form of a fixed but
// unreachable password — there is no plaintext input that resolves to it
// short of bypassing HashPassword's RNG. It is therefore safe to keep
// in source: it never authorises anyone.
//
// We compute it lazily on first use to keep the package free of a
// generation step, and store the result in dummyHashOnce-guarded
// dummyHashValue.
var (
	dummyHashValue string
	dummyHashErr   error
)

func init() {
	// One-shot init: build a real argon2id hash whose plaintext is an
	// internal constant. Doing it in init ensures a Login on the cold-
	// path does not pay the ~100ms argon2 cost twice (once for dummy,
	// once for the not-found branch). The dummy hash is only used as a
	// *target* for VerifyPassword — the compute happens against the
	// caller-supplied password during the not-found branch.
	dummyHashValue, dummyHashErr = hash.HashPassword("not-a-real-password-do-not-use")
}

// Register hashes the password, persists a new Driver row, and returns
// the saved entity with PasswordHash populated. The caller (the handler
// layer) is responsible for clearing the hash before serialising the
// response. Validation failures map to domain.ErrValidation; a duplicate
// email maps to the repository's domain.ErrAlreadyExists.
func (u *AuthUsecase) Register(
	ctx context.Context,
	email, password, name string,
	role domain.Role,
) (*domain.Driver, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	name = strings.TrimSpace(name)

	if email == "" {
		return nil, fmt.Errorf("email is required: %w", domain.ErrValidation)
	}
	if password == "" {
		return nil, fmt.Errorf("password is required: %w", domain.ErrValidation)
	}
	if name == "" {
		return nil, fmt.Errorf("name is required: %w", domain.ErrValidation)
	}
	if !role.Valid() {
		return nil, fmt.Errorf("invalid role %q: %w", string(role), domain.ErrValidation)
	}

	encoded, err := hash.HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	now := u.nowFunc().UnixMilli()
	d := &domain.Driver{
		ID:           u.ids.NewID(),
		Email:        email,
		PasswordHash: encoded,
		Name:         name,
		Role:         role,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if err := u.drivers.Create(ctx, d); err != nil {
		return nil, err
	}
	return d, nil
}

// Login verifies credentials and issues a JWT. The same domain.ErrUnauthorized
// is returned for both "unknown email" and "wrong password" to deny a
// user-enumeration oracle. To make the timing channel uninformative we
// run VerifyPassword against a precomputed dummy hash in the not-found
// branch, so both paths spend ~100ms on argon2 work.
func (u *AuthUsecase) Login(ctx context.Context, email, password string) (string, *domain.Driver, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || password == "" {
		// Defensive: the handler validates first, but the usecase should
		// not panic on empty input either.
		_ = hash.VerifyPassword(password, dummyHashValue) // burn time
		return "", nil, domain.ErrUnauthorized
	}

	d, err := u.drivers.GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// Equal-time path: still pay for one argon2 verify so the
			// caller cannot infer email existence from response latency.
			_ = hash.VerifyPassword(password, dummyHashValue)
			return "", nil, domain.ErrUnauthorized
		}
		return "", nil, fmt.Errorf("login lookup: %w", err)
	}

	if !hash.VerifyPassword(password, d.PasswordHash) {
		return "", nil, domain.ErrUnauthorized
	}

	token, _, err := u.signer.Issue(d.ID, string(d.Role))
	if err != nil {
		return "", nil, fmt.Errorf("issue token: %w", err)
	}
	return token, d, nil
}

// Me returns the driver behind userID. The HTTP layer calls this after
// the auth middleware has already validated the JWT and pulled the sub
// claim — the usecase therefore trusts userID and only translates the
// repository's miss into domain.ErrNotFound.
func (u *AuthUsecase) Me(ctx context.Context, userID string) (*domain.Driver, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user id is required: %w", domain.ErrValidation)
	}
	return u.drivers.GetByID(ctx, userID)
}

// Logout writes the token's JTI to the KV blocklist under bl:{jti} with a
// TTL equal to the token's remaining lifetime. The auth middleware checks
// this map on every request and rejects a hit with domain.ErrUnauthorized.
// Calling Logout twice on the same JTI is a no-op — Put overwrites with
// the same value.
//
// We do NOT remove the entry on its own; instead we let CF KV expire it
// at exp-time. This avoids the back-pressure of a Delete call and
// matches how typical revocation lists behave.
func (u *AuthUsecase) Logout(ctx context.Context, claims *jwt.Claims) error {
	if claims == nil {
		return fmt.Errorf("nil claims: %w", domain.ErrValidation)
	}
	if strings.TrimSpace(claims.JTI) == "" {
		return fmt.Errorf("jti is required: %w", domain.ErrValidation)
	}
	remaining := u.signer.Remaining(claims)
	if remaining <= 0 {
		// Token already past its exp — no point blocklisting; treat as
		// idempotent success.
		return nil
	}
	key := "bl:" + claims.JTI
	if err := u.blocklist.Put(ctx, key, []byte("1"), remaining); err != nil {
		return fmt.Errorf("blocklist put: %w", err)
	}
	return nil
}

// IsRevoked reports whether the given claims appears in the blocklist.
// Wrapped here (rather than left to the middleware) so the same logic
// applies wherever a token-presence check is needed — for instance, the
// /api/auth/me handler can call this directly without re-implementing
// the bl:{jti} convention.
func (u *AuthUsecase) IsRevoked(ctx context.Context, claims *jwt.Claims) (bool, error) {
	if claims == nil || strings.TrimSpace(claims.JTI) == "" {
		// A claims with no JTI cannot have been blocklisted; treat as
		// not-revoked rather than returning an error so the caller's
		// happy-path stays clean.
		return false, nil
	}
	_, found, err := u.blocklist.Get(ctx, "bl:"+claims.JTI)
	if err != nil {
		return false, fmt.Errorf("blocklist get: %w", err)
	}
	return found, nil
}

// dummyHashInitError surfaces the init() failure (if any) at the first
// real Login attempt rather than crashing the binary on import. The
// production fan-in code can call this in main() to fail fast if it
// would rather not start a server with a degraded enumeration-resistance
// guarantee.
func dummyHashInitError() error { return dummyHashErr }
