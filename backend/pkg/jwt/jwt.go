// Package jwt issues and verifies short-lived JWTs for the Mini Fleet
// Tracker API. The signing algorithm is HS256 with a shared secret loaded
// from JWT_SECRET in config — adequate for a single-issuer demo where the
// API, the gateway Worker, and the Durable Object all share one trust
// anchor. RS256 / EdDSA is not used: the extra public-key plumbing would
// not buy anything until a third-party verifier needs to be onboarded.
//
// Claims layout matches the design brief:
//
//	{ "iss": "mini-fleet-tracker", "sub": "<driver-id>", "role": "driver|manager",
//	  "jti": "<uuid v4>", "iat": <unix>, "exp": <unix> }
//
// `jti` is the per-token unique identifier used by Logout to blocklist a
// single token in KV without invalidating the user's other sessions.
package jwt

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Issuer is the fixed value of the `iss` claim. Embedded verifiers (the
// gateway Worker, the Durable Object) compare against this constant; if
// the brand name ever changes update both sides in the same change.
const Issuer = "mini-fleet-tracker"

// Claims is the custom claim payload. It composes jwt.RegisteredClaims to
// inherit the standard iat / exp / iss / nbf machinery from the library
// while adding the application-specific Role and JTI fields.
//
// JSON tags on the embedded struct's fields come from jwt.RegisteredClaims;
// we only need to name the bespoke ones.
type Claims struct {
	Subject string `json:"sub,omitempty"`
	Role    string `json:"role,omitempty"`
	JTI     string `json:"jti,omitempty"`
	jwt.RegisteredClaims
}

// Signer creates and verifies tokens with a fixed secret and access TTL.
// One Signer per process is the expected lifecycle — the type holds no
// mutable state and is safe for concurrent use.
type Signer struct {
	secret    []byte
	issuer    string
	accessTTL time.Duration
	// now is wired for tests; production code leaves it nil and uses
	// time.Now under the hood.
	now func() time.Time
}

// NewSigner returns a Signer ready to Issue and Verify tokens. The secret
// must be non-empty; accessTTL must be strictly positive. Both checks fail
// closed because a zero-length secret or zero TTL would be silently
// catastrophic in production.
func NewSigner(secret string, accessTTL time.Duration) (*Signer, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, errors.New("jwt: secret is required")
	}
	if accessTTL <= 0 {
		return nil, errors.New("jwt: accessTTL must be positive")
	}
	return &Signer{
		secret:    []byte(secret),
		issuer:    Issuer,
		accessTTL: accessTTL,
	}, nil
}

// AccessTTL is the configured token lifetime. The usecase layer reads this
// when computing the KV blocklist entry's TTL for Logout — the blocklist
// only needs to retain a revoked JTI until its parent token would have
// expired naturally.
func (s *Signer) AccessTTL() time.Duration {
	return s.accessTTL
}

// nowFunc returns the test override when set, time.Now otherwise. Kept
// internal so callers cannot accidentally rebind the clock at runtime.
func (s *Signer) nowFunc() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Issue produces a signed token for subject + role. A fresh UUID v4 jti
// is included so Logout can revoke this specific token without touching
// other live sessions for the same subject. The returned Claims is the
// exact struct serialised into the token; tests can introspect it.
func (s *Signer) Issue(subject, role string) (string, *Claims, error) {
	if s == nil {
		return "", nil, errors.New("jwt: nil signer")
	}
	if strings.TrimSpace(subject) == "" {
		return "", nil, errors.New("jwt: subject is required")
	}

	now := s.nowFunc()
	claims := &Claims{
		Subject: subject,
		Role:    role,
		JTI:     uuid.NewString(),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.accessTTL)),
			// NotBefore is set to iat so a clock-skew check on the gateway
			// side sees a single "first valid at" instant.
			NotBefore: jwt.NewNumericDate(now),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(s.secret)
	if err != nil {
		return "", nil, fmt.Errorf("jwt: sign: %w", err)
	}
	return signed, claims, nil
}

// Verify parses and validates the token. It returns the embedded claims
// or an error if the signature is bad, the structure is malformed, the
// token is expired, or the algorithm header does not match HS256.
//
// The library's default validation already checks exp, nbf, and iat; we
// additionally pin the algorithm via a keyfunc so a "none" or RS256
// header cannot bypass the symmetric secret.
func (s *Signer) Verify(tokenStr string) (*Claims, error) {
	if s == nil {
		return nil, errors.New("jwt: nil signer")
	}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(s.issuer),
		jwt.WithTimeFunc(s.nowFunc),
	)
	token, err := parser.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("jwt: unexpected signing method %q", t.Method.Alg())
		}
		return s.secret, nil
	})
	if err != nil {
		return nil, fmt.Errorf("jwt: parse: %w", err)
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, errors.New("jwt: invalid token")
	}
	return claims, nil
}

// Remaining returns how much of the token's lifetime is left at the
// signer's notion of "now". Logout uses this to decide the TTL of the
// blocklist KV entry — there is no point pinning a revoked JTI past
// its natural expiration. A nil claims or missing exp returns 0.
func (s *Signer) Remaining(c *Claims) time.Duration {
	if s == nil || c == nil || c.ExpiresAt == nil {
		return 0
	}
	d := c.ExpiresAt.Sub(s.nowFunc())
	if d < 0 {
		return 0
	}
	return d
}
