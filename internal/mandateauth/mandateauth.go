// Package mandateauth verifies that a mandate came from the merchant.
//
// THE GAP THIS CLOSES. rzp-guard enforces that an agent cannot exceed the
// authority in the mandate. It never established that the authority was
// genuine: the guard read a JSON file off disk and trusted it. Anyone who could
// write that file could authorize any refund, and the guard would enforce it
// faithfully. Two different boundaries:
//
//	merchant --[ authority -> guard ]--> guard --[ agent -> authority ]--> agent
//	           ^ this package                    ^ internal/policy
//
// WHY A DETACHED SIGNATURE OVER THE RAW BYTES. Signing a re-serialisation of
// the parsed mandate would mean the bytes that were signed are not the bytes
// that were read: any difference in key order, number formatting or whitespace
// between the signer's encoder and this one becomes either a false rejection or
// a gap to squeeze through. Verifying the exact file bytes before they are
// parsed removes canonicalisation from the trust path entirely. It also means
// verification happens before any parser touches attacker-influenced input.
//
// WHY IT IS OPT-IN. Turning this on by default would break every existing
// deployment and every fixture in this repository at once, four days from a
// deadline, on a money path. Instead: no key configured behaves exactly as
// before and says so loudly on stderr; a key configured makes a valid signature
// MANDATORY, and an unsigned or altered mandate refuses to start.
//
// WHAT IT STILL DOES NOT DO. It authenticates the file, not the human. A
// compromised signing key issues mandates the guard will honour, and key
// custody is outside this program. It is a real reduction in trusted surface,
// not a proof of merchant intent.
package mandateauth

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

var (
	// ErrUnsigned means a public key was configured but no signature was found.
	ErrUnsigned = errors.New("mandate is not signed")
	// ErrBadSignature means the signature did not verify against the key. The
	// mandate has been altered since it was signed, or it was signed by someone
	// else.
	ErrBadSignature = errors.New("mandate signature does not verify")
	// ErrBadKey means the configured public key is unusable.
	ErrBadKey = errors.New("mandate public key is not a valid ed25519 key")
)

// SigPath is the detached signature for a mandate: the mandate's own path with
// ".sig" appended, so the pair travels together and neither can be mistaken for
// the other.
func SigPath(mandatePath string) string { return mandatePath + ".sig" }

// Result reports what verification established. Verified is false in the
// unconfigured case, and Warning then explains what is NOT being checked --
// callers are expected to print it rather than discard it.
type Result struct {
	Verified bool
	KeyID    string // first 8 hex chars of the public key, for the audit trail
	Warning  string
}

// LoadPublicKey reads a hex-encoded ed25519 public key. An empty path is not an
// error: it means verification is not configured, which Verify reports.
func LoadPublicKey(path string) (ed25519.PublicKey, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading mandate public key: %w", err)
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("%w: not hex: %v", ErrBadKey, err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: %d bytes, want %d",
			ErrBadKey, len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

// Verify checks the detached signature for mandatePath against pub.
//
// A nil key is the unconfigured case: it returns Verified=false with a warning
// and no error, which is the pre-existing behaviour made explicit. A non-nil
// key makes the signature mandatory, and every failure is an error.
func Verify(mandatePath string, raw []byte, pub ed25519.PublicKey) (Result, error) {
	if pub == nil {
		return Result{
			Warning: "mandate signature NOT verified: no -mandate-pubkey configured. " +
				"The guard is enforcing the authority in this file without having " +
				"established that the merchant issued it. Anyone who can write the " +
				"file can authorize a refund.",
		}, nil
	}

	sigPath := SigPath(mandatePath)
	sigHex, err := os.ReadFile(sigPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{}, fmt.Errorf("%w: expected a detached signature at %s. "+
				"A public key is configured, so an unsigned mandate is refused rather "+
				"than trusted", ErrUnsigned, sigPath)
		}
		return Result{}, fmt.Errorf("reading %s: %w", sigPath, err)
	}
	sig, err := hex.DecodeString(strings.TrimSpace(string(sigHex)))
	if err != nil {
		return Result{}, fmt.Errorf("%w: %s is not hex: %v", ErrBadSignature, sigPath, err)
	}
	if len(sig) != ed25519.SignatureSize {
		return Result{}, fmt.Errorf("%w: signature is %d bytes, want %d",
			ErrBadSignature, len(sig), ed25519.SignatureSize)
	}
	// Over the EXACT bytes read from disk, before any parsing.
	if !ed25519.Verify(pub, raw, sig) {
		return Result{}, fmt.Errorf("%w: %s does not match %s under the configured "+
			"key. The mandate has been altered since it was signed, or it was signed "+
			"by a different key", ErrBadSignature, mandatePath, sigPath)
	}
	return Result{Verified: true, KeyID: hex.EncodeToString(pub[:4])}, nil
}

// Sign produces a detached signature. It lives here so the verifier and the
// signer cannot drift apart about what is covered: the raw bytes, nothing else.
// The private key belongs to the merchant and never reaches the guard.
func Sign(raw []byte, priv ed25519.PrivateKey) string {
	return hex.EncodeToString(ed25519.Sign(priv, raw))
}
