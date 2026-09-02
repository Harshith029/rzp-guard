package opauth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func storedWith(t, m, p string) string {
	salt := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef"))
	hash := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	return fmt.Sprintf("argon2id$%s$%s$%s$%s$%s", t, m, p, salt, hash)
}

// Verify parses t, m and p out of a file on disk and hands them to argon2.
// Before they were checked, argon2.IDKey PANICKED on t=0 and on p=0, and would
// have attempted a 4 TiB allocation for m near uint32 max. Both reproduced.
//
// Not a privilege escalation -- whoever can write this file already controls the
// credential -- but it crashes the OPERATOR RECOVERY PATH, which is the tool you
// reach for when a refund is already IN_DOUBT. A truncated write gets there with
// no attacker at all.
func TestHostileStoredParametersAreRefusedNotExecuted(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tt, mm, pp string
		want       error
	}{
		{"zero rounds panics argon2", "0", "65536", "4", ErrMalformedVerifier},
		{"zero parallelism panics argon2", "3", "65536", "0", ErrMalformedVerifier},
		{"zero memory", "3", "0", "4", ErrMalformedVerifier},
		{"memory below 8*p", "3", "7", "4", ErrMalformedVerifier},
		// Never reaches argon2, so the allocation is never attempted.
		{"4 TiB of memory", "3", "4294967295", "4", ErrMalformedVerifier},
		{"four billion rounds", "4294967295", "65536", "1", ErrMalformedVerifier},
		{"downgraded to trivially weak", "1", "8", "1", ErrWeakVerifier},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("PANIC instead of a refusal: %v", r)
				}
			}()
			err := Verify("some-token", storedWith(tc.tt, tc.mm, tc.pp))
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// A downgrade must be distinguishable from corruption: one means re-provision,
// the other means the file is damaged, and an operator mid-incident needs to
// know which.
func TestADowngradeIsReportedSeparatelyFromCorruption(t *testing.T) {
	weak := Verify("tok", storedWith("1", "8", "1"))
	if !errors.Is(weak, ErrWeakVerifier) || errors.Is(weak, ErrMalformedVerifier) {
		t.Errorf("a weak-but-parseable verifier reported as %v", weak)
	}
	if !strings.Contains(weak.Error(), "Re-provision") {
		t.Errorf("the downgrade error does not say what to do about it: %v", weak)
	}
	broken := Verify("tok", storedWith("0", "65536", "4"))
	if !errors.Is(broken, ErrMalformedVerifier) {
		t.Errorf("a structurally impossible verifier reported as %v", broken)
	}
}

// The floor must not reject what this build itself writes, or provisioning
// produces a credential the verifier refuses.
func TestTheCredentialThisBuildWritesStillVerifies(t *testing.T) {
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	stored, err := Verifier(token)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(token, stored); err != nil {
		t.Fatalf("a freshly provisioned credential was refused: %v", err)
	}
	if err := Verify(token+"x", stored); !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("a wrong token gave %v, want ErrTokenRejected", err)
	}
	// And the floor genuinely sits below what is written, so raising the
	// constants later does not lock an operator out of recovery.
	if argonTime < minArgonTime || argonMemory < minArgonMemory || argonThreads < minArgonThreads {
		t.Errorf("the floor (t=%d m=%d p=%d) is above what this build writes "+
			"(t=%d m=%d p=%d): provisioning and verification disagree",
			minArgonTime, minArgonMemory, minArgonThreads,
			argonTime, argonMemory, argonThreads)
	}
}
