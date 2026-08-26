package opauth

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewTokenIsHighEntropyAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 256; i++ {
		tok, err := NewToken()
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		if !strings.HasPrefix(tok, "rzpop_") {
			t.Fatalf("token %q lacks the rzpop_ prefix", tok)
		}
		if seen[tok] {
			t.Fatalf("NewToken repeated a value after %d draws", i)
		}
		seen[tok] = true
		// URL-safe so it survives copy/paste and environment variables intact.
		if strings.ContainsAny(tok, "+/= ") {
			t.Fatalf("token %q contains characters that do not survive transport", tok)
		}
	}
}

func TestVerifierSaltsEveryDerivation(t *testing.T) {
	const tok = "rzpop_example"
	a, err := Verifier(tok)
	if err != nil {
		t.Fatalf("Verifier: %v", err)
	}
	b, _ := Verifier(tok)

	if a == b {
		t.Fatal("two derivations of the same token are identical — the salt is not " +
			"random, so identical tokens would be visible in stored verifiers")
	}
	// Both must still verify: the salt is stored alongside the hash.
	for i, v := range []string{a, b} {
		if err := Verify(tok, v); err != nil {
			t.Fatalf("verifier %d rejected its own token: %v", i, err)
		}
	}
	if got := strings.Split(a, "$"); len(got) != 6 || got[0] != "argon2id" {
		t.Fatalf("verifier format is %q, want argon2id$t$m$p$salt$hash", a)
	}
}

func TestVerifyRejectsWrongTokens(t *testing.T) {
	const tok = "rzpop_correct"
	stored, err := Verifier(tok)
	if err != nil {
		t.Fatal(err)
	}

	if err := Verify(tok, stored); err != nil {
		t.Fatalf("the correct token must verify: %v", err)
	}
	for _, wrong := range []string{
		"rzpop_wrong",
		"rzpop_correc",   // prefix
		"rzpop_correct ", // trailing space
		"RZPOP_CORRECT",  // case
		"",
	} {
		if err := Verify(wrong, stored); !errors.Is(err, ErrTokenRejected) {
			t.Errorf("Verify(%q) returned %v, want ErrTokenRejected", wrong, err)
		}
	}
}

// A malformed verifier must be distinguishable from a wrong token, and must
// never verify. Returning "rejected" for garbage would be safe; returning nil
// would be catastrophic.
func TestVerifyRefusesMalformedVerifiers(t *testing.T) {
	for _, tc := range []struct{ name, stored string }{
		{"empty", ""},
		{"not argon2id", "bcrypt$1$2$3$c2FsdA$aGFzaA"},
		{"too few fields", "argon2id$3$65536$4$c2FsdA"},
		{"non-numeric params", "argon2id$x$y$z$c2FsdA$aGFzaA"},
		{"bad base64 salt", "argon2id$3$65536$4$!!!$aGFzaA"},
		{"bad base64 hash", "argon2id$3$65536$4$c2FsdA$!!!"},
		{"plaintext token stored directly", "rzpop_correct"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Verify("rzpop_correct", tc.stored)
			if err == nil {
				t.Fatal("a malformed verifier must never verify")
			}
			if !errors.Is(err, ErrMalformedVerifier) {
				t.Fatalf("got %v, want ErrMalformedVerifier so it is distinguishable "+
					"from a wrong token", err)
			}
		})
	}
}

// The Grant is the whole point of this package: an unforgeable proof of
// authentication, so a resolver cannot be handed a boolean the caller chose.
func TestGrantCannotBeForgedOutsideThePackage(t *testing.T) {
	// A zero-value Grant is what any other package can construct.
	var zero Grant
	if zero.Valid() {
		t.Fatal("a zero-value Grant is valid — every other package can mint one, " +
			"which defeats the entire mechanism")
	}
	if zero.Subject() != "" {
		t.Fatalf("a zero-value Grant carries subject %q", zero.Subject())
	}

	stored, err := Verifier("rzpop_correct")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Authenticate("alice", "rzpop_wrong", stored); !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("Authenticate with a wrong token returned %v, want ErrTokenRejected", err)
	}
	bad, _ := Authenticate("alice", "rzpop_wrong", stored)
	if bad.Valid() {
		t.Fatal("a failed Authenticate returned a valid Grant")
	}

	g, err := Authenticate("alice", "rzpop_correct", stored)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !g.Valid() {
		t.Fatal("a successful Authenticate must return a valid Grant")
	}
	if g.Subject() != "alice" {
		t.Fatalf("Subject = %q, want the authenticated subject", g.Subject())
	}
}

// Credential delivery fails closed: the token is only committed when the write
// is provably durable, and an existing destination is never overwritten.
func TestWriteTokenExclusiveRefusesAnExistingPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")

	if err := os.WriteFile(path, []byte("pre-existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteTokenExclusive(path, "rzpop_new", true); err == nil {
		t.Fatal("writing over an existing path must be refused — a failed run " +
			"must not destroy the credential a previous one delivered")
	}
	// And it must not have been touched.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "pre-existing" {
		t.Fatalf("the existing file was modified: %q", string(b))
	}
}

func TestWriteTokenExclusiveWritesAndReportsDurability(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")

	durable, err := WriteTokenExclusive(path, "rzpop_new", true)
	if err != nil {
		t.Fatalf("WriteTokenExclusive: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A trailing newline is deliberate: the file is read by shells and editors,
	// and a missing final newline is a well-known source of copy/paste damage.
	// The token itself must be exactly what was passed in.
	if got := strings.TrimRight(string(b), "\n"); got != "rzpop_new" {
		t.Fatalf("token file contains %q, want the token followed by a newline", string(b))
	}
	// Durability is reported honestly per platform rather than assumed: Windows
	// cannot fsync a directory, which is why it is not a supported target.
	if durable != DirSyncSupported() {
		t.Fatalf("durable=%v but DirSyncSupported()=%v on %s — the report must "+
			"match what the platform can actually guarantee",
			durable, DirSyncSupported(), runtime.GOOS)
	}
}
