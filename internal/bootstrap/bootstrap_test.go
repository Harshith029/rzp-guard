package bootstrap

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/harshith/rzp-guard/internal/lifecycle"
	"github.com/harshith/rzp-guard/internal/mandate"
	"github.com/harshith/rzp-guard/internal/policy"
	"github.com/harshith/rzp-guard/internal/storage"
)

var now = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

func testMandate(t *testing.T, actions string, cumulative int64, perMinute int) *mandate.Mandate {
	t.Helper()
	doc := fmt.Sprintf(`{"mandate_id":"mnd_test","expires_at":%q,
		"allowed_tools":["fetch_payment","create_refund"],
		"authorized_refund_actions":%s,
		"global":{"max_cumulative_paise":%d,"max_calls_per_minute":%d}}`,
		now.Add(4*time.Hour).Format(time.RFC3339), actions, cumulative, perMinute)
	m, err := mandate.Load([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// THE RATE LIMIT MUST HOLD ACROSS A RESTART.
//
// The previous revision persisted the window but never restored it, so killing
// the process reset max_calls_per_minute to zero used.
func TestRateLimitHoldsAfterRestart(t *testing.T) {
	db := filepath.Join(t.TempDir(), "guard.db")

	var actions []string
	for k := 0; k < 8; k++ {
		actions = append(actions, fmt.Sprintf(
			`{"action_id":"rfa_%03d","payment_id":"pay_SYN%d","amount_paise":1000}`, k, k))
	}
	spec := "[" + strings.Join(actions, ",") + "]"
	const limit = 4

	// First process: consume the whole rate allowance.
	m := testMandate(t, spec, 1_000_000, limit)
	first, err := Open(db, m, now)
	if err != nil {
		t.Fatal(err)
	}
	for k := 0; k < limit; k++ {
		d := first.Guard.Decide(policy.RefundTool,
			map[string]any{"payment_id": fmt.Sprintf("pay_SYN%d", k), "amount": int64(1000)}, now)
		if !d.Allowed {
			t.Fatalf("call %d denied during setup: %s (%s)", k, d.Reason, d.Rule)
		}
		if err := first.Guard.Commit(d.MatchedActionID); err != nil {
			t.Fatal(err)
		}
	}
	// The next call must be rate-limited before the restart.
	if d := first.Guard.Decide(policy.RefundTool,
		map[string]any{"payment_id": "pay_SYN5", "amount": int64(1000)}, now); d.Rule != policy.RateLimitExceeded {
		t.Fatalf("pre-restart call %d rule = %s, want %s", limit, d.Rule, policy.RateLimitExceeded)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart, same wall clock: the window has not moved on.
	second, err := Open(db, m, now)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	d := second.Guard.Decide(policy.RefundTool,
		map[string]any{"payment_id": "pay_SYN6", "amount": int64(1000)}, now)
	if d.Allowed {
		t.Fatal("restart reset the rate window: a crash-loop would bypass " +
			"max_calls_per_minute entirely")
	}
	if d.Rule != policy.RateLimitExceeded {
		t.Fatalf("post-restart rule = %s, want %s", d.Rule, policy.RateLimitExceeded)
	}
}

// The rate window is a moving window, not a permanent cap: once the minute has
// passed, capacity returns even across a restart.
func TestRateWindowExpiresNormallyAcrossRestart(t *testing.T) {
	db := filepath.Join(t.TempDir(), "guard.db")
	spec := `[{"action_id":"rfa_001","payment_id":"pay_SYN0","amount_paise":1000},
	          {"action_id":"rfa_002","payment_id":"pay_SYN1","amount_paise":1000}]`
	m := testMandate(t, spec, 1_000_000, 1)

	first, err := Open(db, m, now)
	if err != nil {
		t.Fatal(err)
	}
	d := first.Guard.Decide(policy.RefundTool,
		map[string]any{"payment_id": "pay_SYN0", "amount": int64(1000)}, now)
	if !d.Allowed {
		t.Fatalf("setup denied: %s", d.Reason)
	}
	_ = first.Guard.Commit(d.MatchedActionID)
	first.Close()

	later := now.Add(2 * time.Minute)
	second, err := Open(db, m, later)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if d := second.Guard.Decide(policy.RefundTool,
		map[string]any{"payment_id": "pay_SYN1", "amount": int64(1000)}, later); !d.Allowed {
		t.Fatalf("window did not expire after two minutes: %s (%s)", d.Reason, d.Rule)
	}
}

// A mid-flight reservation is recovered as IN_DOUBT and reported by Open.
func TestBootstrapRecoversMidFlightReservationAsInDoubt(t *testing.T) {
	db := filepath.Join(t.TempDir(), "guard.db")
	spec := `[{"action_id":"rfa_001","payment_id":"pay_SYN0","amount_paise":60000}]`
	m := testMandate(t, spec, 100000, 10)

	first, err := Open(db, m, now)
	if err != nil {
		t.Fatal(err)
	}
	d := first.Guard.Decide(policy.RefundTool,
		map[string]any{"payment_id": "pay_SYN0", "amount": int64(60000)}, now)
	if !d.Allowed {
		t.Fatalf("setup denied: %s", d.Reason)
	}
	first.Close() // crash: no commit, no release

	second, err := Open(db, m, now)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	if len(second.RecoveredInDoubt) != 1 || second.RecoveredInDoubt[0] != "rfa_001" {
		t.Fatalf("RecoveredInDoubt = %v, want [rfa_001]", second.RecoveredInDoubt)
	}
	if second.Guard.State("rfa_001") != lifecycle.InDoubt {
		t.Fatalf("state = %s, want IN_DOUBT", second.Guard.State("rfa_001"))
	}
	if enc := second.Guard.Encumbered(); enc != 60000 {
		t.Fatalf("encumbered = %d, want 60000", enc)
	}
}

// ------------------------------------------------- cross-PROCESS ownership

// The previous test opened a second store in the SAME Go process, which proves
// nothing about an OS-level lock. This spawns a real second process.
func TestSecondProcessCannotOpenTheStateFile(t *testing.T) {
	if db := os.Getenv("RZP_GUARD_LOCK_PROBE"); db != "" {
		// Child role: try to take the state file the parent is holding.
		st, err := storage.Open(db, "mnd_test")
		if err != nil {
			fmt.Println("REFUSED")
			os.Exit(0)
		}
		st.Close()
		fmt.Println("ACQUIRED")
		os.Exit(0)
	}

	db := filepath.Join(t.TempDir(), "guard.db")
	parent, err := storage.Open(db, "mnd_test")
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	// Hold a real write so the exclusive lock is definitely taken.
	if err := parent.RecordCall(now.UnixNano()); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestSecondProcessCannotOpenTheStateFile")
	cmd.Env = append(os.Environ(), "RZP_GUARD_LOCK_PROBE="+db)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe process failed: %v\n%s", err, out)
	}
	got := string(out)
	if strings.Contains(got, "ACQUIRED") {
		t.Fatalf("a SEPARATE PROCESS acquired the state file while this one holds "+
			"it; two guards would each enforce the cumulative cap against their "+
			"own in-memory ledger\n%s", got)
	}
	if !strings.Contains(got, "REFUSED") {
		t.Fatalf("probe produced neither REFUSED nor ACQUIRED:\n%s", got)
	}
}
