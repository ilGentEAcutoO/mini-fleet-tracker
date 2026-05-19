package hash

import (
	"strings"
	"testing"
)

func TestHashPassword_RoundTrip(t *testing.T) {
	const pw = "correct horse battery staple"
	encoded, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !VerifyPassword(pw, encoded) {
		t.Fatalf("VerifyPassword should accept the matching password; got false")
	}
}

func TestHashPassword_DistinctHashesForSamePassword(t *testing.T) {
	// Two consecutive calls must yield different encoded strings because the
	// salt is fresh per call. If the salt RNG ever degenerates this test
	// will catch it.
	const pw = "same input"
	a, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword #1: %v", err)
	}
	b, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword #2: %v", err)
	}
	if a == b {
		t.Fatalf("expected distinct hashes for the same password; got identical %q", a)
	}
	// Both must still verify.
	if !VerifyPassword(pw, a) || !VerifyPassword(pw, b) {
		t.Fatalf("both hashes must verify against the source password")
	}
}

func TestHashPassword_EmptyRejected(t *testing.T) {
	if _, err := HashPassword(""); err == nil {
		t.Fatal("HashPassword(\"\") should return an error")
	}
}

func TestVerifyPassword_WrongPasswordRejected(t *testing.T) {
	encoded, err := HashPassword("the right one")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if VerifyPassword("the wrong one", encoded) {
		t.Fatal("VerifyPassword should reject a non-matching password")
	}
}

func TestVerifyPassword_MalformedHashReturnsFalse(t *testing.T) {
	// VerifyPassword promises a uniform false return for any malformed
	// input — empty string, wrong prefix, wrong segment count, bad b64,
	// or unexpected parameters. The point is to deny an attacker a side
	// channel that distinguishes "wrong password" from "corrupt row".
	cases := []struct {
		name    string
		encoded string
	}{
		{"empty string", ""},
		{"wrong algorithm tag", "$bcrypt$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA"},
		{"wrong segment count", "$argon2id$v=19$m=65536,t=3,p=2$saltonly"},
		{"bad b64 in salt", "$argon2id$v=19$m=65536,t=3,p=2$!!!notb64$aGFzaA"},
		{"unexpected memory cost", "$argon2id$v=19$m=4096,t=3,p=2$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNo"},
		{"unexpected version", "$argon2id$v=99$m=65536,t=3,p=2$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNo"},
		{"wrong salt length after b64 decode", "$argon2id$v=19$m=65536,t=3,p=2$c2hvcnQ$aGFzaA"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if VerifyPassword("anything", tc.encoded) {
				t.Fatalf("malformed hash %q should not verify", tc.encoded)
			}
		})
	}
}

// TestHashPassword_PHCStructure checks that the encoded string carries the
// parameters we documented and is split into the canonical six segments.
// A drift here would silently change interop with any other argon2
// implementation that consumes our PHC strings.
func TestHashPassword_PHCStructure(t *testing.T) {
	encoded, err := HashPassword("structure check")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 {
		t.Fatalf("expected 6 PHC segments, got %d in %q", len(parts), encoded)
	}
	if parts[0] != "" {
		t.Fatalf("PHC string must start with $; first segment was %q", parts[0])
	}
	if parts[1] != "argon2id" {
		t.Fatalf("algorithm segment: want %q, got %q", "argon2id", parts[1])
	}
	if parts[2] != "v=19" {
		t.Fatalf("version segment: want %q, got %q", "v=19", parts[2])
	}
	if parts[3] != "m=65536,t=3,p=2" {
		t.Fatalf("parameter segment: want %q, got %q", "m=65536,t=3,p=2", parts[3])
	}
	if parts[4] == "" || parts[5] == "" {
		t.Fatalf("salt and hash segments must be non-empty; got salt=%q hash=%q", parts[4], parts[5])
	}
}
