package mandateauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A mandate authorizes money to move. The whole point of this package is that
// an altered one is refused, so every test below alters something.

func keypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// signed writes a mandate and its detached signature, and returns the path.
func signed(t *testing.T, body string, priv ed25519.PrivateKey) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mandate.json")
	raw := []byte(body)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SigPath(path), []byte(Sign(raw, priv)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, raw
}

const body = `{"mandate_id":"mnd_sig","authorized_refund_actions":[` +
	`{"action_id":"rfa_1","payment_id":"pay_SYN01","amount_paise":24000}]}`

func TestAGenuineSignatureVerifies(t *testing.T) {
	pub, priv := keypair(t)
	path, raw := signed(t, body, priv)

	res, err := Verify(path, raw, pub)
	if err != nil {
		t.Fatalf("a mandate signed by the configured key was refused: %v", err)
	}
	if !res.Verified {
		t.Fatal("Verified is false for a good signature")
	}
	if res.KeyID != hex.EncodeToString(pub[:4]) {
		t.Errorf("KeyID %q does not identify the verifying key", res.KeyID)
	}
}

// The attack this exists for: someone with write access to the mandate file
// raises the authorized amount. The guard would enforce the new number
// faithfully, which is exactly the problem.
func TestAnAlteredAmountIsRefused(t *testing.T) {
	pub, priv := keypair(t)
	path, _ := signed(t, body, priv)

	tampered := []byte(`{"mandate_id":"mnd_sig","authorized_refund_actions":[` +
		`{"action_id":"rfa_1","payment_id":"pay_SYN01","amount_paise":9900000}]}`)
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Verify(path, tampered, pub)
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("an altered mandate was accepted (err %v). Anyone who can write "+
			"the file could authorize any refund", err)
	}
}

// A single byte, because a signature that only catches large edits is not a
// signature.
func TestASingleByteChangeIsRefused(t *testing.T) {
	pub, priv := keypair(t)
	path, raw := signed(t, body, priv)

	altered := append([]byte(nil), raw...)
	altered[len(altered)-3] ^= 0x01
	if err := os.WriteFile(path, altered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(path, altered, pub); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a one-byte change was accepted: %v", err)
	}
}

// Signed by a real key, just not the one the operator configured. Without this
// check, an attacker generates their own keypair and signs whatever they like.
func TestAnotherKeysSignatureIsRefused(t *testing.T) {
	pub, _ := keypair(t)
	_, attacker := keypair(t)
	path, raw := signed(t, body, attacker)

	if _, err := Verify(path, raw, pub); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a mandate signed by an unconfigured key was accepted: %v", err)
	}
}

// Deleting the signature must not be a way to opt out of verification.
func TestRemovingTheSignatureIsRefusedNotIgnored(t *testing.T) {
	pub, priv := keypair(t)
	path, raw := signed(t, body, priv)
	if err := os.Remove(SigPath(path)); err != nil {
		t.Fatal(err)
	}

	_, err := Verify(path, raw, pub)
	if !errors.Is(err, ErrUnsigned) {
		t.Fatalf("a mandate with its signature deleted was accepted (err %v). "+
			"Verification that can be switched off by deleting a file is not "+
			"verification", err)
	}
}

// The unconfigured path is the pre-existing behaviour. It must keep working --
// every fixture in this repository is unsigned -- but it must not pass silently.
func TestNoKeyConfiguredProceedsButSaysWhatIsNotChecked(t *testing.T) {
	_, priv := keypair(t)
	path, raw := signed(t, body, priv)

	res, err := Verify(path, raw, nil)
	if err != nil {
		t.Fatalf("no key configured must not be an error: %v", err)
	}
	if res.Verified {
		t.Fatal("Verified is true with no key configured, which would report an " +
			"unverified mandate as verified")
	}
	if res.Warning == "" {
		t.Fatal("the unconfigured case returned no warning, so an operator has no " +
			"way to notice that nothing is being checked")
	}
}

func TestAMalformedKeyIsRefusedRatherThanIgnored(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ name, content string }{
		{"not hex", "zzzz"},
		{"too short", "aabbcc"},
	} {
		p := filepath.Join(dir, tc.name+".key")
		if err := os.WriteFile(p, []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadPublicKey(p); !errors.Is(err, ErrBadKey) {
			t.Errorf("%s: got %v, want ErrBadKey. A key that silently loads as nil "+
				"would turn enforcement off", tc.name, err)
		}
	}
}

func TestAnEmptyKeyPathMeansUnconfiguredNotBroken(t *testing.T) {
	pub, err := LoadPublicKey("")
	if err != nil {
		t.Fatalf("an empty path must mean unconfigured, not an error: %v", err)
	}
	if pub != nil {
		t.Fatal("an empty path returned a key")
	}
}
