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

func buildOperator(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "rzp-guard-operator")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
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

	audit, err := runOperator(t, bin, realToken, "-mandate", mandatePath, "-state", dbPath, "audit")
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
	out, err := runOperator(t, bin, "", "-mandate", mandatePath, "-state", dbPath, "init")
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

	out, err := runOperator(t, bin, "wrong-token-entirely-here",
		"-mandate", mandatePath, "-state", dbPath,
		"rotate", "-operator", "attacker", "-reason", "takeover")
	if err == nil {
		t.Fatalf("rotation succeeded without the current token:\n%s", out)
	}

	out, err = runOperator(t, bin, realToken,
		"-mandate", mandatePath, "-state", dbPath,
		"rotate", "-operator", "ops", "-reason", "scheduled rotation")
	if err != nil {
		t.Fatalf("rotation with the correct token failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "rzpop_") {
		t.Fatalf("rotation did not emit a new token:\n%s", out)
	}

	audit, _ := runOperator(t, bin, "", "-mandate", mandatePath, "-state", dbPath, "audit")
	if !strings.Contains(audit, "ROTATED") {
		t.Fatalf("rotation was not audited:\n%s", audit)
	}
}

// Audit text is bounded and must not carry control characters: the trail is read
// by humans and may later be rendered.
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
