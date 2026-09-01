package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harshith/rzp-guard/internal/mandateauth"
)

// keygen -> sign -> verify, then tamper and confirm the OLD signature refuses.
// The signing side and the verifying side are separate programs, so this is the
// only place that proves they agree about what is covered.
func TestSigningProducesASignatureTheGuardAccepts(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := cmdMandateKeygen([]string{"merchant"}); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	body := []byte(`{"mandate_id":"mnd_sign","authorized_refund_actions":[` +
		`{"action_id":"rfa_1","payment_id":"pay_SYN01","amount_paise":24000}]}`)
	if err := os.WriteFile("m.json", body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmdMandateSign([]string{"merchant.key", "m.json"}); err != nil {
		t.Fatalf("sign: %v", err)
	}

	pub, err := mandateauth.LoadPublicKey("merchant.pub")
	if err != nil {
		t.Fatalf("the public key keygen wrote does not load: %v", err)
	}
	if _, err := mandateauth.Verify("m.json", body, pub); err != nil {
		t.Fatalf("the guard rejected a mandate the operator just signed: %v", err)
	}

	// The attack: raise the authorized amount without re-signing.
	tampered := []byte(`{"mandate_id":"mnd_sign","authorized_refund_actions":[` +
		`{"action_id":"rfa_1","payment_id":"pay_SYN01","amount_paise":9900000}]}`)
	if err := os.WriteFile("m.json", tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mandateauth.Verify("m.json", tampered, pub); err == nil {
		t.Fatal("a mandate whose amount was raised after signing was accepted")
	}
}

// The private key authorizes refunds for as long as it exists. It must not be
// written world-readable, on any platform that enforces the mode.
func TestTheSigningKeyIsNotWrittenWorldReadable(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	if err := cmdMandateKeygen([]string{"merchant"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat("merchant.key")
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		// Windows does not honour Unix mode bits; the command warns
		// unconditionally for that reason, so this is a Unix-only assertion.
		if os.Getenv("OS") != "Windows_NT" {
			t.Fatalf("signing key written with mode %04o", mode)
		}
	}
}

// Handing the tool a .pub instead of a .key must say so, not produce a file
// that silently fails to verify later.
func TestSigningWithThePublicKeyIsRefusedClearly(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	if err := cmdMandateKeygen([]string{"merchant"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("m.json", []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := cmdMandateSign([]string{"merchant.pub", "m.json"})
	if err == nil {
		t.Fatal("signing with the public key was accepted")
	}
	if !strings.Contains(err.Error(), "PRIVATE") {
		t.Errorf("error does not point at the mistake: %v", err)
	}
}

// A signature over a re-encoding rather than the bytes on disk would verify
// here and fail in the guard. Pin that it covers the file exactly.
func TestTheSignatureCoversTheFileBytesNotAReEncoding(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	if err := cmdMandateKeygen([]string{"merchant"}); err != nil {
		t.Fatal(err)
	}
	// Whitespace a JSON re-encoder would normalise away.
	body := []byte("{\n  \"mandate_id\" :  \"mnd_ws\"\n}\n")
	if err := os.WriteFile("m.json", body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmdMandateSign([]string{"merchant.key", "m.json"}); err != nil {
		t.Fatal(err)
	}

	keyHex, _ := os.ReadFile("merchant.key")
	kb, _ := hex.DecodeString(strings.TrimSpace(string(keyHex)))
	pub := ed25519.PrivateKey(kb).Public().(ed25519.PublicKey)
	if _, err := mandateauth.Verify("m.json", body, pub); err != nil {
		t.Fatalf("a mandate with incidental whitespace failed to verify, so the "+
			"signature is not over the file bytes: %v", err)
	}
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	_ = filepath.Separator
}
