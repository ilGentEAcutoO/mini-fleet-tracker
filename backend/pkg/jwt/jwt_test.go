package jwt

import (
	"strings"
	"testing"
	"time"

	gjwt "github.com/golang-jwt/jwt/v5"
)

const testSecret = "test-secret-do-not-use-anywhere-else"

func newTestSigner(t *testing.T) *Signer {
	t.Helper()
	s, err := NewSigner(testSecret, time.Hour)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

func TestNewSigner_RejectsEmptySecret(t *testing.T) {
	if _, err := NewSigner("", time.Hour); err == nil {
		t.Fatal("NewSigner(\"\") should fail")
	}
	if _, err := NewSigner("   ", time.Hour); err == nil {
		t.Fatal("NewSigner(\"   \") should fail")
	}
}

func TestNewSigner_RejectsNonPositiveTTL(t *testing.T) {
	if _, err := NewSigner(testSecret, 0); err == nil {
		t.Fatal("NewSigner with TTL=0 should fail")
	}
	if _, err := NewSigner(testSecret, -time.Second); err == nil {
		t.Fatal("NewSigner with TTL<0 should fail")
	}
}

func TestSigner_Issue_RejectsEmptySubject(t *testing.T) {
	s := newTestSigner(t)
	if _, _, err := s.Issue("", "driver"); err == nil {
		t.Fatal("Issue(\"\", ...) should fail")
	}
}

func TestSigner_IssueVerify_RoundTrip(t *testing.T) {
	s := newTestSigner(t)
	const sub = "drv_01"
	const role = "driver"

	token, claims, err := s.Issue(sub, role)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty token string")
	}
	// Sanity-check the claim payload.
	if claims.Subject != sub {
		t.Fatalf("claims.Subject: want %q, got %q", sub, claims.Subject)
	}
	if claims.Role != role {
		t.Fatalf("claims.Role: want %q, got %q", role, claims.Role)
	}
	if claims.JTI == "" {
		t.Fatal("claims.JTI must be populated")
	}
	if claims.Issuer != Issuer {
		t.Fatalf("claims.Issuer: want %q, got %q", Issuer, claims.Issuer)
	}
	if claims.ExpiresAt == nil || claims.IssuedAt == nil {
		t.Fatal("ExpiresAt and IssuedAt must be set")
	}
	if !claims.ExpiresAt.After(claims.IssuedAt.Time) {
		t.Fatalf("exp (%v) must be after iat (%v)", claims.ExpiresAt.Time, claims.IssuedAt.Time)
	}

	parsed, err := s.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if parsed.Subject != sub || parsed.Role != role || parsed.JTI != claims.JTI {
		t.Fatalf("parsed claims drift: sub=%q role=%q jti=%q", parsed.Subject, parsed.Role, parsed.JTI)
	}
}

func TestSigner_Verify_TamperedSignature(t *testing.T) {
	s := newTestSigner(t)
	token, _, err := s.Issue("drv_01", "driver")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// Replace the last two characters of the signature so the HMAC fails.
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT segments, got %d", len(parts))
	}
	tampered := parts[0] + "." + parts[1] + "." + parts[2][:len(parts[2])-2] + "XY"

	if _, err := s.Verify(tampered); err == nil {
		t.Fatal("Verify should reject a tampered signature")
	}
}

func TestSigner_Verify_ExpiredToken(t *testing.T) {
	// Use a 1-second TTL plus a frozen-then-advanced clock so the test does
	// not actually sleep.
	s, err := NewSigner(testSecret, time.Second)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	clock := time.Now()
	s.now = func() time.Time { return clock }

	token, _, err := s.Issue("drv_01", "driver")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Advance the clock past expiration.
	clock = clock.Add(2 * time.Second)

	if _, err := s.Verify(token); err == nil {
		t.Fatal("Verify should reject an expired token")
	}
}

func TestSigner_Verify_WrongAlgorithm(t *testing.T) {
	s := newTestSigner(t)
	// Build an "alg=none" token by hand. The standard parser rejects it
	// when WithValidMethods is restricted to HS256, which is exactly what
	// our Signer configures.
	claims := gjwt.MapClaims{
		"sub":  "drv_01",
		"role": "driver",
		"iss":  Issuer,
	}
	noneTok := gjwt.NewWithClaims(gjwt.SigningMethodNone, claims)
	noneStr, err := noneTok.SignedString(gjwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none: %v", err)
	}
	if _, err := s.Verify(noneStr); err == nil {
		t.Fatal("Verify should reject alg=none")
	}
}

func TestSigner_Remaining(t *testing.T) {
	s, err := NewSigner(testSecret, time.Hour)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	clock := time.Now()
	s.now = func() time.Time { return clock }

	_, claims, err := s.Issue("drv_01", "driver")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// At t=0 the remaining lifetime is essentially the full TTL.
	rem := s.Remaining(claims)
	if rem <= 59*time.Minute || rem > time.Hour {
		t.Fatalf("expected ~1h remaining at t=0, got %v", rem)
	}
	// At t=TTL+ε the remaining lifetime is 0 (clamped, not negative).
	clock = clock.Add(2 * time.Hour)
	if rem := s.Remaining(claims); rem != 0 {
		t.Fatalf("expected 0 remaining after expiration, got %v", rem)
	}
	// Nil claims is safe and returns 0.
	if rem := s.Remaining(nil); rem != 0 {
		t.Fatalf("Remaining(nil) should be 0, got %v", rem)
	}
}

func TestSigner_AccessTTL(t *testing.T) {
	s, err := NewSigner(testSecret, 45*time.Minute)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	if s.AccessTTL() != 45*time.Minute {
		t.Fatalf("AccessTTL: want 45m, got %v", s.AccessTTL())
	}
}

func TestSigner_Verify_WrongIssuer(t *testing.T) {
	// A token signed by a signer with a different issuer string must fail
	// validation. We construct that by mutating the issuer claim directly
	// before signing.
	good := newTestSigner(t)
	clock := time.Now()
	good.now = func() time.Time { return clock }

	claims := &Claims{
		Subject: "drv_01",
		Role:    "driver",
		JTI:     "jti-1",
		RegisteredClaims: gjwt.RegisteredClaims{
			Issuer:    "someone-else",
			IssuedAt:  gjwt.NewNumericDate(clock),
			ExpiresAt: gjwt.NewNumericDate(clock.Add(time.Hour)),
		},
	}
	token := gjwt.NewWithClaims(gjwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("sign wrong-iss: %v", err)
	}

	if _, err := good.Verify(signed); err == nil {
		t.Fatal("Verify should reject a token with a foreign issuer")
	}
}
