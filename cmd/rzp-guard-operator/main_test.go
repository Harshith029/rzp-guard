package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/harshith/rzp-guard/internal/lifecycle"
	"github.com/harshith/rzp-guard/internal/mandate"
	"github.com/harshith/rzp-guard/internal/opauth"
	"github.com/harshith/rzp-guard/internal/storage"
)

const mandateDoc = `{
  "mandate_id": "mnd_operator_test",
  "expires_at": "2030-01-01T00:00:00Z",
  "allowed_tools": ["create_refund"],
  "authorized_refund_actions": [
    {"action_id": "rfa_stuck_001", "payment_id": "pay_SYN00000000001", "amount_paise": 50000}
  ],
  "global": {"max_cumulative_paise": 200000, "max_calls_per_minute": 10}
}`

// stuckState builds a state file holding one IN_DOUBT action, with the operator
// operator credential configured the way  configures it.
func stuckState(t *testing.T, configureToken bool) (dbPath, mandatePath, token string) {
	t.Helper()
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "state.db")
	mandatePath = filepath.Join(dir, "mandate.json")
	if err := os.WriteFile(mandatePath, []byte(mandateDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := mandate.Load([]byte(mandateDoc))
	if err != nil {
		t.Fatal(err)
	}
	st, err := storage.Open(dbPath, m.MandateID)
	if err != nil {
		t.Fatal(err)
	}
	if configureToken {
		token, err = opauth.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		v, err := opauth.Verifier(token)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.InitOperatorVerifier(v); err != nil {
			t.Fatal(err)
		}
	}
	led := lifecycle.NewLedger(m.Limits.MaxCumulativePaise, st)
	receipt, err := mandate.ReceiptFor(m.MandateID, "rfa_stuck_001")
	if err != nil {
		t.Fatal(err)
	}
	if err := led.Reserve("rfa_stuck_001", receipt, 50000); err != nil {
		t.Fatal(err)
	}
	if err := led.MarkInDoubt("rfa_stuck_001"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return dbPath, mandatePath, token
}

// buildOperator compiles the CLI under test.
//
// -buildvcs=false is deliberate. Go stamps VCS metadata when it finds a repo,
// and a repo it can find but not read -- a CI export, a shallow tree, mismatched
// ownership inside a container -- fails the build with
// "error obtaining VCS status: exit status 128". Build metadata is irrelevant
// to a test fixture, and a reviewer cloning this repo should not hit a failure
// that has nothing to do with the code.
func buildOperator(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "rzp-guard-operator")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-buildvcs=false", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build operator: %v\n%s", err, out)
	}
	return bin
}

func runOperator(t *testing.T, bin, token string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "RZP_GUARD_OPERATOR_TOKEN="+token)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func stateOf(t *testing.T, dbPath string) lifecycle.State {
	t.Helper()
	st, err := storage.Open(dbPath, "mnd_operator_test")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	snap, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return lifecycle.State(snap.States["rfa_stuck_001"])
}

// A wrong token must NOT resolve anything.
//
// This test exists because the first version of this CLI read the token from
// the environment and then constructed the console with THAT SAME token, so the
// comparison was against itself and any sufficiently long value was accepted. A
// wrong token resolved a locked refund. The expected value now comes from the
// verifier that  recorded — a different source than the one being checked.
func TestWrongOperatorTokenIsRejectedAndChangesNothing(t *testing.T) {
	bin := buildOperator(t)
	dbPath, mandatePath, _ := stuckState(t, true)

	out, err := runOperator(t, bin, "wrong-token-but-long-enough",
		"-mandate", mandatePath, "-state", dbPath,
		"resolve", "rfa_stuck_001", "-outcome", "landed",
		"-operator", "ops@merchant", "-reason", "attempted")
	if err == nil {
		t.Fatalf("a wrong operator token resolved the action:\n%s", out)
	}
	if !strings.Contains(out, "token rejected") {
		t.Fatalf("expected a token rejection, got:\n%s", out)
	}
	if got := stateOf(t, dbPath); got != lifecycle.InDoubt {
		t.Fatalf("state = %s, want IN_DOUBT: a rejected resolution changed state", got)
	}
}

func TestCorrectOperatorTokenResolvesAndAudits(t *testing.T) {
	bin := buildOperator(t)
	dbPath, mandatePath, realToken := stuckState(t, true)

	out, err := runOperator(t, bin, realToken,
		"-mandate", mandatePath, "-state", dbPath,
		"resolve", "rfa_stuck_001", "-outcome", "landed",
		"-operator", "ops@merchant", "-reason", "found in Razorpay Test Mode")
	if err != nil {
		t.Fatalf("resolve failed: %v\n%s", err, out)
	}
	if got := stateOf(t, dbPath); got != lifecycle.Committed {
		t.Fatalf("state = %s, want COMMITTED", got)
	}

	audit, err := runOperator(t, bin, realToken, "-mandate", mandatePath, "-state", dbPath, "-operator", "ops@merchant", "audit")
	if err != nil {
		t.Fatalf("audit failed: %v\n%s", err, audit)
	}
	if !strings.Contains(audit, "ops@merchant") ||
		!strings.Contains(audit, "found in Razorpay Test Mode") {
		t.Fatalf("audit trail missing the operator or the reason:\n%s", audit)
	}
}

// Without a configured token the tool must refuse outright rather than fall
// back to accepting whatever it was handed.
func TestUnconfiguredStateFileRefusesResolution(t *testing.T) {
	bin := buildOperator(t)
	dbPath, mandatePath, _ := stuckState(t, false)

	out, err := runOperator(t, bin, "any-token-at-all-here-32",
		"-mandate", mandatePath, "-state", dbPath,
		"resolve", "rfa_stuck_001", "-outcome", "landed",
		"-operator", "ops", "-reason", "x")
	if err == nil {
		t.Fatalf("resolution succeeded with no configured token:\n%s", out)
	}
	if !strings.Contains(out, "no operator credential exists") {
		t.Fatalf("unexpected failure:\n%s", out)
	}
	if got := stateOf(t, dbPath); got != lifecycle.InDoubt {
		t.Fatalf("state = %s, want IN_DOUBT", got)
	}
}

// Flags must be parsed regardless of where they sit relative to the command.
// Go's flag package stops at the first non-flag argument, so an earlier version
// silently defaulted -outcome when it followed the subcommand.
func TestFlagsAreParsedAfterTheSubcommand(t *testing.T) {
	bin := buildOperator(t)
	dbPath, mandatePath, realToken := stuckState(t, true)

	out, err := runOperator(t, bin, realToken,
		"-mandate", mandatePath, "-state", dbPath,
		"resolve", "rfa_stuck_001",
		"-outcome", "not-landed", "-operator", "ops", "-reason", "confirmed absent")
	if err != nil {
		t.Fatalf("flags after the subcommand were not parsed: %v\n%s", err, out)
	}
	if got := stateOf(t, dbPath); got != lifecycle.Available {
		t.Fatalf("state = %s, want AVAILABLE for not-landed", got)
	}
}

// The guard must have NO path that writes the operator credential.
//
// It previously recorded RZP_GUARD_OPERATOR_TOKEN on every start, so anyone able
// to relaunch the process could install their own token and then resolve locked
// refunds without knowing the real one. Demonstrated end to end before the fix:
// the attacker was rejected, restarted the guard with their own token, and
// resolved the action.
func TestGuardCannotReplaceTheOperatorCredential(t *testing.T) {
	bin := buildOperator(t)
	dbPath, mandatePath, realToken := stuckState(t, true)

	// A second init must be refused outright.
	out, err := runOperator(t, bin, "", "-mandate", mandatePath, "-state", dbPath, "init", "-out", filepath.Join(t.TempDir(), "tok"))
	if err == nil {
		t.Fatalf("a second init replaced an existing credential:\n%s", out)
	}
	if !strings.Contains(out, "already exists") {
		t.Fatalf("unexpected init failure:\n%s", out)
	}

	// And the original token still works, so nothing was overwritten.
	out, err = runOperator(t, bin, realToken,
		"-mandate", mandatePath, "-state", dbPath,
		"resolve", "rfa_stuck_001", "-outcome", "landed",
		"-operator", "ops", "-reason", "still the original credential")
	if err != nil {
		t.Fatalf("the original credential stopped working: %v\n%s", err, out)
	}
}

// Rotation requires the CURRENT token and is audited.
func TestRotationRequiresTheCurrentToken(t *testing.T) {
	bin := buildOperator(t)
	dbPath, mandatePath, realToken := stuckState(t, true)
	newTokPath := filepath.Join(t.TempDir(), "newtok")

	out, err := runOperator(t, bin, "wrong-token-entirely-here",
		"-mandate", mandatePath, "-state", dbPath,
		"-operator", "attacker", "rotate", "-reason", "takeover")
	if err == nil {
		t.Fatalf("rotation succeeded without the current token:%s", out)
	}

	out, err = runOperator(t, bin, realToken,
		"-mandate", mandatePath, "-state", dbPath,
		"-operator", "ops", "rotate", "-reason", "scheduled rotation",
		"-out", newTokPath)
	if err != nil {
		t.Fatalf("rotation with the correct token failed: %v %s", err, out)
	}
	if strings.Contains(out, "rzpop_") {
		t.Fatalf("rotation printed the new secret to a pipe: %s", out)
	}

	// The OLD token must stop working, and the new one must start.
	if _, err := runOperator(t, bin, realToken, "-mandate", mandatePath,
		"-state", dbPath, "-operator", "ops", "audit"); err == nil {
		t.Fatal("the pre-rotation token still authenticates")
	}
	nb, err := os.ReadFile(newTokPath)
	if err != nil {
		t.Fatalf("new token file missing: %v", err)
	}
	newToken := strings.TrimSpace(string(nb))
	if !strings.HasPrefix(newToken, "rzpop_") {
		t.Fatalf("new token malformed: %q", newToken)
	}
	audit, err := runOperator(t, bin, newToken, "-mandate", mandatePath,
		"-state", dbPath, "-operator", "ops", "audit")
	if err != nil {
		t.Fatalf("audit with the new token failed: %v %s", err, audit)
	}
	if !strings.Contains(audit, "ROTATED") {
		t.Fatalf("rotation was not audited: %s", audit)
	}
}

func TestAuditTextIsBoundedAndControlCharsRefused(t *testing.T) {
	bin := buildOperator(t)
	dbPath, mandatePath, realToken := stuckState(t, true)

	out, err := runOperator(t, bin, realToken,
		"-mandate", mandatePath, "-state", dbPath,
		"resolve", "rfa_stuck_001", "-outcome", "landed",
		"-operator", "ops", "-reason", "line one\nIN_DOUBT -> COMMITTED by someone else")
	if err == nil {
		t.Fatalf("a reason containing a newline was accepted:\n%s", out)
	}
	if !strings.Contains(out, "control character") {
		t.Fatalf("unexpected failure:\n%s", out)
	}

	long := strings.Repeat("x", 600)
	out, err = runOperator(t, bin, realToken,
		"-mandate", mandatePath, "-state", dbPath,
		"resolve", "rfa_stuck_001", "-outcome", "landed",
		"-operator", "ops", "-reason", long)
	if err == nil {
		t.Fatalf("a 600-character reason was accepted:\n%s", out)
	}
}

// list and audit disclose payment ids, receipts, amounts and audit reasons.
// They are recovery evidence, not public data: an earlier version gated only
// mutation, leaving all of it readable by any local caller.
func TestReadCommandsRequireTheCredential(t *testing.T) {
	bin := buildOperator(t)
	dbPath, mandatePath, realToken := stuckState(t, true)

	for _, cmd := range []string{"list", "audit"} {
		out, err := runOperator(t, bin, "", "-mandate", mandatePath, "-state", dbPath,
			"-operator", "someone", cmd)
		if err == nil {
			t.Fatalf("%s ran without a credential:\n%s", cmd, out)
		}
		if strings.Contains(out, "pay_SYN") || strings.Contains(out, "rzpg_") {
			t.Fatalf("%s leaked recovery evidence before authenticating:\n%s", cmd, out)
		}

		out, err = runOperator(t, bin, "wrong-token-entirely-here",
			"-mandate", mandatePath, "-state", dbPath, "-operator", "someone", cmd)
		if err == nil {
			t.Fatalf("%s ran with a wrong credential:\n%s", cmd, out)
		}
	}

	// The correct credential still works.
	out, err := runOperator(t, bin, realToken, "-mandate", mandatePath, "-state", dbPath,
		"-operator", "ops", "list")
	if err != nil {
		t.Fatalf("list with the correct credential failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "rfa_stuck_001") {
		t.Fatalf("authenticated list showed nothing:\n%s", out)
	}
}

// Terminal delivery cannot be proven, so init refuses to commit a credential
// after printing one -- unless the operator explicitly accepts that outcome.
//
// An earlier revision detected the problem and then committed anyway after a
// warning. Warning after taking the unsafe action is not fail-closed: a
// disconnect or a lost scrollback leaves a state file whose recovery authority
// nobody can exercise.
func TestInitRefusesUnprovableDeliveryAndCommitsNothing(t *testing.T) {
	bin := buildOperator(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "fresh.db")
	mandatePath := filepath.Join(dir, "mandate.json")
	if err := os.WriteFile(mandatePath, []byte(mandateDoc), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runOperator(t, bin, "", "-mandate", mandatePath, "-state", dbPath, "init")
	if err == nil {
		t.Fatalf("init committed a credential with unprovable delivery:\n%s", out)
	}
	if !strings.Contains(out, "cannot be proven") {
		t.Fatalf("unexpected failure:\n%s", out)
	}
	if strings.Contains(out, "rzpop_") {
		t.Fatalf("a token was emitted anyway:\n%s", out)
	}

	// THE POINT: nothing was committed, so provisioning can still be done
	// properly. A committed verifier here would be an unrecoverable state file.
	st, err := storage.Open(dbPath, "mnd_operator_test")
	if err != nil {
		t.Fatal(err)
	}
	_, configured, _, err := st.OperatorVerifier()
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	if configured {
		t.Fatal("a credential was committed despite refusing delivery -- this state " +
			"file would be permanently unrecoverable")
	}

	// -out onto a platform that can fsync a directory is the supported path.
	tokPath := filepath.Join(dir, "token")
	out, err = runOperator(t, bin, "", "-mandate", mandatePath, "-state", dbPath,
		"init", "-out", tokPath)
	if err != nil {
		t.Fatalf("init -out failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "rzpop_") {
		t.Fatalf("the token leaked to stdout despite -out:\n%s", out)
	}
	b, err := os.ReadFile(tokPath)
	if err != nil || !strings.HasPrefix(string(b), "rzpop_") {
		t.Fatalf("token file missing or malformed: %v %q", err, string(b))
	}
}

func TestFailedTokenDeliveryLeavesInitRetryable(t *testing.T) {
	bin := buildOperator(t)
	dir := t.TempDir()
	mandatePath := filepath.Join(dir, "mandate.json")
	if err := os.WriteFile(mandatePath, []byte(mandateDoc), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		out  func() string
	}{
		{"unwritable path", func() string {
			return filepath.Join(dir, "no", "such", "dir", "tok")
		}},
		{"destination already exists", func() string {
			p := filepath.Join(dir, "taken")
			if err := os.WriteFile(p, []byte("SOMEONE ELSES SECRET\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "state.db")
			outPath := tc.out()

			out, err := runOperator(t, bin, "", "-mandate", mandatePath, "-state", dbPath,
				"init", "-out", outPath)
			if err == nil {
				t.Fatalf("init reported success despite a failed delivery:\n%s", out)
			}

			// An existing destination must be untouched.
			if tc.name == "destination already exists" {
				b, readErr := os.ReadFile(outPath)
				if readErr != nil || string(b) != "SOMEONE ELSES SECRET\n" {
					t.Fatalf("an existing file was modified: %q (err %v)", string(b), readErr)
				}
			}

			// THE POINT: no credential was committed, so init can be retried.
			good := filepath.Join(t.TempDir(), "tok")
			out, err = runOperator(t, bin, "", "-mandate", mandatePath, "-state", dbPath,
				"init", "-out", good)
			if err != nil {
				t.Fatalf("init could not be retried after a failed delivery -- "+
					"recovery is permanently locked out: %v\n%s", err, out)
			}
			b, err := os.ReadFile(good)
			if err != nil || !strings.HasPrefix(string(b), "rzpop_") {
				t.Fatalf("retry did not deliver a token: %v %q", err, string(b))
			}
		})
	}
}

// The same ordering matters more for rotate: delivering after rotating would
// invalidate a WORKING credential while the replacement reached nobody.
func TestFailedRotationDeliveryLeavesTheOldTokenWorking(t *testing.T) {
	bin := buildOperator(t)
	dbPath, mandatePath, realToken := stuckState(t, true)

	taken := filepath.Join(t.TempDir(), "taken")
	if err := os.WriteFile(taken, []byte("EXISTING\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runOperator(t, bin, realToken, "-mandate", mandatePath, "-state", dbPath,
		"-operator", "ops", "rotate", "-reason", "attempted", "-out", taken)
	if err == nil {
		t.Fatalf("rotation succeeded despite a failed delivery:\n%s", out)
	}
	if b, _ := os.ReadFile(taken); string(b) != "EXISTING\n" {
		t.Fatalf("an existing file was modified during a failed rotation: %q", string(b))
	}

	// The old credential must still authenticate: no rotation happened.
	out, err = runOperator(t, bin, realToken, "-mandate", mandatePath, "-state", dbPath,
		"-operator", "ops", "audit")
	if err != nil {
		t.Fatalf("a failed rotation destroyed the working credential: %v\n%s", err, out)
	}
}
