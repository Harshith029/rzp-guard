package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/harshith/rzp-guard/internal/mandate"
	"github.com/harshith/rzp-guard/internal/policy"
)

// The status file exists so an operator can see what is stuck WITHOUT stopping
// the guard. Its whole value is that it is readable while the exclusive lock is
// held, so these tests check the document is complete, correct, and safe to
// read at any moment.

func testGuard(t *testing.T) *policy.Guard {
	t.Helper()
	doc := `{"mandate_id":"mnd_status","expires_at":"2030-01-01T00:00:00Z",
		"allowed_tools":["create_refund"],
		"authorized_refund_actions":[
			{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":50000},
			{"action_id":"rfa_002","payment_id":"pay_SYN0002","amount_paise":30000}],
		"global":{"max_cumulative_paise":500000,"max_calls_per_minute":10}}`
	m, err := mandate.Load([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return policy.New(m)
}

func readStatus(t *testing.T, path string) statusDoc {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d statusDoc
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("status file is not valid JSON: %v\n%s", err, b)
	}
	return d
}

// A healthy guard must say so unambiguously, or an operator learns to ignore
// the file.
func TestStatusReportsHealthyWhenNothingIsStuck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	sw := newStatusWriter(path, testGuard(t), "mnd_status")

	if err := sw.write(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	d := readStatus(t, path)

	if d.NeedsOperator {
		t.Fatal("needs_operator is true with nothing in doubt")
	}
	if d.InDoubtCount != 0 || len(d.InDoubtActions) != 0 {
		t.Fatalf("in-doubt = %d %v, want none", d.InDoubtCount, d.InDoubtActions)
	}
	if d.MandateID != "mnd_status" {
		t.Fatalf("mandate_id = %q", d.MandateID)
	}
	if d.PID == 0 || d.Program != "rzp-guard" || d.Schema != 1 {
		t.Fatalf("identity fields incomplete: %+v", d)
	}
	if d.RemainingPaise != 500000 {
		t.Fatalf("remaining = %d, want the full cap", d.RemainingPaise)
	}
}

// The case the file exists for: money in an unknown state, visible without
// touching the state file.
func TestStatusSurfacesInDoubtWithoutTheStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	g := testGuard(t)
	sw := newStatusWriter(path, g, "mnd_status")

	args := map[string]any{"payment_id": "pay_SYN0001", "amount": int64(50000)}
	if d := g.Decide(policy.RefundTool, args, time.Now().UTC()); !d.Allowed {
		t.Fatalf("setup: %s %s", d.Rule, d.Reason)
	}
	if err := g.MarkInDoubt("rfa_001"); err != nil {
		t.Fatal(err)
	}

	if err := sw.write(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	d := readStatus(t, path)

	if !d.NeedsOperator {
		t.Fatal("needs_operator is false while an action is IN_DOUBT; that flag " +
			"is the one thing a monitoring rule watches")
	}
	if d.InDoubtCount != 1 || len(d.InDoubtActions) != 1 || d.InDoubtActions[0] != "rfa_001" {
		t.Fatalf("in-doubt = %d %v, want [rfa_001]", d.InDoubtCount, d.InDoubtActions)
	}
	// The budget must still be shown as held: an IN_DOUBT action has NOT
	// released its money, and a status file implying otherwise would invite an
	// operator to over-authorize.
	if d.EncumberedPais != 50000 {
		t.Fatalf("encumbered = %d, want 50000 still held", d.EncumberedPais)
	}
	if d.RemainingPaise != 450000 {
		t.Fatalf("remaining = %d, want 450000", d.RemainingPaise)
	}
}

// The file names payment-linked action ids, so it must not be world-readable.
func TestStatusFileIsNotWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not honour 0600; the project declares linux/amd64")
	}
	path := filepath.Join(t.TempDir(), "status.json")
	sw := newStatusWriter(path, testGuard(t), "mnd_status")
	if err := sw.write(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Fatalf("status file mode is %o, want 600: it names action ids tied to "+
			"real payments", mode)
	}
}

// Rewriting must never leave a reader holding a truncated document. The writer
// goes through a temp file and a rename for exactly this reason.
func TestStatusRewriteIsAtomicAndLeavesNoLitter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")
	sw := newStatusWriter(path, testGuard(t), "mnd_status")

	for i := 0; i < 25; i++ {
		if err := sw.write(time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		readStatus(t, path) // parses, or the test fails
	}

	// Temp files must not accumulate: this writes every few seconds for the
	// life of the process, so a leak here fills the operator's disk.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %d files after 25 rewrites (%v); the temp file "+
			"is not being cleaned up", len(entries), names)
	}
}
