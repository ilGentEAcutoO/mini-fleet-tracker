// Package hash provides password hashing with argon2id, configured per
// RFC 9106 §4 ("Recommendations for application designers"): m=64 MiB,
// t=3 iterations, p=2 lanes. The encoded output is the standard PHC
// string format which carries the algorithm and parameters inline, so
// future parameter bumps are forward-compatible without a DB migration.
//
// Why argon2id (and not bcrypt or scrypt): argon2id won the 2015 Password
// Hashing Competition, is the OWASP top recommendation, and is the only
// modern KDF that resists both GPU and side-channel attacks by design.
// Go's golang.org/x/crypto/argon2 is the canonical implementation.
package hash

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Recommended argon2id parameters per RFC 9106 §4. 64 MiB memory is the
// "second recommended" profile — it's a fair compromise between resistance
// and the modest RAM available to a Cloudflare Container instance running
// the API. If memory pressure ever forces us down to 19 MiB ("first
// recommended"), bumping t to 2 keeps comparable strength.
const (
	argonMemoryKiB = 64 * 1024 // 64 MiB
	argonTime      = 3
	argonParallel  = 2
	argonKeyLen    = 32 // 256-bit digest
	argonSaltLen   = 16 // 128-bit salt, per RFC 9106 §3.1
)

// argonVersion is the on-disk argon2 version this package emits. PHC
// strings carry the version inline so the verifier can refuse to operate
// on a hash from a future, incompatible argon2 release.
const argonVersion = argon2.Version

// phcPrefix is the algorithm tag in the PHC string. The serialised form
// follows §2 of the PHC specification:
//
//	$argon2id$v=19$m=65536,t=3,p=2$<salt-b64>$<hash-b64>
//
// Both b64 sections use raw standard encoding (no padding), as is
// conventional for argon2 PHC strings.
const phcPrefix = "$argon2id$"

// errMalformedHash is returned internally when the PHC string cannot be
// parsed. VerifyPassword folds this into a plain false return so callers
// cannot distinguish "wrong password" from "corrupt hash row" by looking
// at the error path — the latter would leak the existence of a row to a
// timing-aware attacker.
var errMalformedHash = errors.New("hash: malformed argon2id PHC string")

// HashPassword returns the PHC-encoded argon2id digest of password. Each
// call generates a fresh 16-byte salt from crypto/rand. The encoded
// string is self-describing — VerifyPassword does not need the parameters
// to be passed alongside it.
func HashPassword(password string) (string, error) {
	if password == "" {
		// Empty passwords are almost certainly a bug at the call site
		// (the form validator should have rejected them first). Bail
		// loudly so we don't paper over upstream validation gaps.
		return "", errors.New("hash: password is empty")
	}

	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("hash: read salt: %w", err)
	}

	digest := argon2.IDKey(
		[]byte(password),
		salt,
		argonTime,
		argonMemoryKiB,
		argonParallel,
		argonKeyLen,
	)

	return encodePHC(salt, digest), nil
}

// VerifyPassword reports whether password produces a digest matching
// encoded. It returns false (no error) on malformed input so callers
// uniformly handle the negative path — leaking "malformed" vs "wrong
// password" through the error channel would give an attacker a timing
// or behaviour signal.
//
// The comparison itself uses subtle.ConstantTimeCompare to avoid the
// byte-by-byte short-circuit that the standard "==" / bytes.Equal would
// expose to a timing attack.
func VerifyPassword(password, encoded string) bool {
	salt, expected, err := decodePHC(encoded)
	if err != nil {
		return false
	}

	got := argon2.IDKey(
		[]byte(password),
		salt,
		argonTime,
		argonMemoryKiB,
		argonParallel,
		uint32(len(expected)),
	)

	return subtle.ConstantTimeCompare(got, expected) == 1
}

// encodePHC serialises (salt, digest) as a PHC argon2id string using the
// package-level parameters. RawStdEncoding (no "=" padding) matches the
// convention every argon2 implementation in the wild uses.
func encodePHC(salt, digest []byte) string {
	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argonVersion,
		argonMemoryKiB,
		argonTime,
		argonParallel,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest),
	)
}

// decodePHC reverses encodePHC and additionally validates the parameter
// block — if the encoded hash carries parameters we don't agree with
// (e.g. m=4096, a much weaker setting), we reject it rather than silently
// re-hashing at the legacy strength. A re-hash path can be added later
// once we have stored hashes that need upgrading.
func decodePHC(encoded string) ([]byte, []byte, error) {
	if !strings.HasPrefix(encoded, phcPrefix) {
		return nil, nil, errMalformedHash
	}
	// Layout: ["", "argon2id", "v=N", "m=...,t=...,p=...", "<salt>", "<hash>"].
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 {
		return nil, nil, errMalformedHash
	}

	var version int
	if _, scanErr := fmt.Sscanf(parts[2], "v=%d", &version); scanErr != nil {
		return nil, nil, errMalformedHash
	}
	if version != argonVersion {
		// Refuse to operate on hashes from a future argon2 release.
		return nil, nil, errMalformedHash
	}

	var m, t uint32
	var p uint8
	if _, scanErr := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); scanErr != nil {
		return nil, nil, errMalformedHash
	}
	if m != argonMemoryKiB || t != argonTime || p != argonParallel {
		return nil, nil, errMalformedHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return nil, nil, errMalformedHash
	}
	digest, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return nil, nil, errMalformedHash
	}
	if len(salt) != argonSaltLen || len(digest) != argonKeyLen {
		return nil, nil, errMalformedHash
	}
	return salt, digest, nil
}
