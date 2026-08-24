// Package opauth is the operator credential: generation, storage format and
// verification.
//
// Two properties matter, and an earlier design had neither.
//
//  1. THE GUARD MUST NOT OWN THIS CREDENTIAL. Previously the guard wrote the
//     token hash into the state file on every start, so anyone able to relaunch
//     the process could set their own token and then resolve IN_DOUBT actions
//     without ever knowing the real one. Demonstrated end to end. The guard now
//     has no path that writes a verifier at all; only `rzp-guard-operator init`
//     (once) and `rotate` (authenticated with the current token) can.
//
//  2. LENGTH IS NOT ENTROPY. "At least 16 characters" permits a memorable
//     passphrase, and an unsalted SHA-256 of it is offline-guessable the moment
//     the SQLite file is copied. Tokens are now GENERATED with 256 bits of
//     crypto/rand and stored as a salted Argon2id verifier.
package opauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. Deliberately costly: this verifier sits in a file that
// may be copied, so offline guessing is the threat model.
const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024 // 64 MiB
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
	saltLen             = 16
	tokenBytes          = 32 // 256 bits
)

var (
	ErrMalformedVerifier = errors.New("stored operator verifier is malformed")
	ErrTokenRejected     = errors.New("operator token rejected")
)

// NewToken returns a fresh high-entropy operator token, URL-safe so it survives
// copy/paste and environment variables intact.
func NewToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("opauth: generate token: %w", err)
	}
	return "rzpop_" + base64.RawURLEncoding.EncodeToString(b), nil
}

// Verifier derives a storable verifier for a token, with a fresh random salt.
// Format: argon2id$t$m$p$salt$hash, all base64 raw-url.
func Verifier(token string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("opauth: salt: %w", err)
	}
	sum := argon2.IDKey([]byte(token), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("argon2id$%d$%d$%d$%s$%s",
		argonTime, argonMemory, argonThreads,
		base64.RawURLEncoding.EncodeToString(salt),
		base64.RawURLEncoding.EncodeToString(sum)), nil
}

// Verify checks a presented token against a stored verifier in constant time.
func Verify(token, stored string) error {
	parts := strings.Split(stored, "$")
	if len(parts) != 6 || parts[0] != "argon2id" {
		return ErrMalformedVerifier
	}
	var t, m uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[1]+" "+parts[2]+" "+parts[3], "%d %d %d", &t, &m, &p); err != nil {
		return ErrMalformedVerifier
	}
	salt, err := base64.RawURLEncoding.DecodeString(parts[4])
	if err != nil {
		return ErrMalformedVerifier
	}
	want, err := base64.RawURLEncoding.DecodeString(parts[5])
	if err != nil {
		return ErrMalformedVerifier
	}
	got := argon2.IDKey([]byte(token), salt, t, m, p, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrTokenRejected
	}
	return nil
}
