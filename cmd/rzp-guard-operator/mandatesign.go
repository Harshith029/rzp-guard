package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"github.com/harshith/rzp-guard/internal/mandateauth"
)

// Merchant-side signing, so `-mandate-pubkey` on the guard is usable rather
// than theoretical.
//
// These two commands run BEFORE the operator opens a state file or
// authenticates. They are deliberately outside that flow: signing a mandate is
// something the merchant does on their own machine, and it has nothing to do
// with the guard's durable state or the operator credential. Requiring a token
// and an exclusive lock to sign a file would be wrong, and would also mean the
// signing key had to live next to the guard -- which is precisely what signing
// is supposed to prevent.
//
// THE PRIVATE KEY MUST NOT LIVE ON THE GUARD HOST. If it does, an attacker who
// can write the mandate can also sign it, and verification establishes nothing.
// keygen says so and writes the key 0600.
//
// This moves no money. It writes an authorization the guard then enforces
// against every limit it already enforces: single use, cumulative cap, expiry,
// rate limit. A signature makes a mandate authentic, not unlimited.

func cmdMandateKeygen(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: rzp-guard-operator mandate-keygen <name>\n" +
			"  writes <name>.key (private, keep off the guard host) and " +
			"<name>.pub (give to the guard as -mandate-pubkey)")
	}
	name := args[0]
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	// 0600 before any bytes land: a key that is briefly world-readable has been
	// world-readable. On Windows the mode is not honoured, which is why the
	// warning below is unconditional rather than platform-gated.
	if err := os.WriteFile(name+".key", []byte(hex.EncodeToString(priv)+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(name+".pub", []byte(hex.EncodeToString(pub)+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Printf("private key: %s.key  (0600)\n", name)
	fmt.Printf("public key:  %s.pub\n", name)
	fmt.Println()
	fmt.Println("Keep the private key OFF the guard host. If an attacker who can")
	fmt.Println("write the mandate can also sign it, verification proves nothing.")
	fmt.Printf("Then run the guard with -mandate-pubkey %s.pub\n", name)
	return nil
}

func cmdMandateSign(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: rzp-guard-operator mandate-sign <key> <mandate.json>\n" +
			"  writes <mandate.json>.sig")
	}
	keyPath, mandatePath := args[0], args[1]

	keyHex, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("reading signing key: %w", err)
	}
	kb, err := hex.DecodeString(trimSpace(string(keyHex)))
	if err != nil {
		return fmt.Errorf("signing key is not hex: %w", err)
	}
	if len(kb) != ed25519.PrivateKeySize {
		return fmt.Errorf("signing key is %d bytes, want %d. Note this wants the "+
			"PRIVATE key (.key), not the public one", len(kb), ed25519.PrivateKeySize)
	}

	// Sign the bytes on disk, unparsed and unmodified. Signing a re-encoding
	// would mean the bytes that were signed are not the bytes the guard reads.
	raw, err := os.ReadFile(mandatePath)
	if err != nil {
		return fmt.Errorf("reading mandate: %w", err)
	}
	sigPath := mandateauth.SigPath(mandatePath)
	sig := mandateauth.Sign(raw, ed25519.PrivateKey(kb))
	if err := os.WriteFile(sigPath, []byte(sig+"\n"), 0o644); err != nil {
		return err
	}

	// Verify what was just written, rather than trusting that signing worked.
	// A signature file nobody has checked is a file, not a signature.
	pub := ed25519.PrivateKey(kb).Public().(ed25519.PublicKey)
	if _, err := mandateauth.Verify(mandatePath, raw, pub); err != nil {
		return fmt.Errorf("the signature just written does not verify: %w", err)
	}
	fmt.Printf("signed %s -> %s (verified)\n", mandatePath, sigPath)
	return nil
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\n' || s[0] == '\r' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 {
		c := s[len(s)-1]
		if c != ' ' && c != '\n' && c != '\r' && c != '\t' {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}
