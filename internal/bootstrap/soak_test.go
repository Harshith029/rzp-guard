package bootstrap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/harshith/rzp-guard/internal/mandate"
	"github.com/harshith/rzp-guard/internal/policy"
)

// SUSTAINED LOAD, which nothing here had ever been put under.
//
// The project could say what one decision costs and what one commit costs. It
// could not say whether anything GROWS -- whether a guard left running for a
// long session leaks memory, or lets the write-ahead log expand without bound.
// Those are the failures that do not show up in a unit test and do show up at
// 3am on day nine.
//
// Skipped under -short. It writes tens of thousands of durable rows and takes
// minutes, which is too slow for the default lane and exactly right before
// anyone claims this is deployable.
//
//	go test ./internal/bootstrap/ -run Soak -v -timeout 30m

func soakMandate(t *testing.T, n int) *mandate.Mandate {
	t.Helper()
	acts := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		acts = append(acts, map[string]any{
			"action_id":    fmt.Sprintf("soak_%06d", i),
			"payment_id":   fmt.Sprintf("pay_SYN%06d", i),
			"amount_paise": 1000,
		})
	}
	doc := map[string]any{
		"mandate_id":    "mnd_soak",
		"expires_at":    time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339),
		"allowed_tools": []string{"fetch_payment", policy.RefundTool},
		"global": map[string]any{
			"max_cumulative_paise": int64(1) << 40,
			"max_calls_per_minute": 1 << 30,
		},
		"authorized_refund_actions": acts,
	}
	raw, _ := json.Marshal(doc)
	m, err := mandate.Load(raw)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func heapMiB() float64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return float64(ms.HeapAlloc) / (1024 * 1024)
}

func dbBytes(t *testing.T, path string) int64 {
	t.Helper()
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(path + suffix); err == nil {
			total += fi.Size()
		}
	}
	return total
}

// A long session must not grow without bound in memory or on disk.
func TestSoakSustainedRefunds(t *testing.T) {
	if testing.Short() {
		t.Skip("soak test: minutes and tens of thousands of durable writes")
	}
	const total = 6000
	const sample = 1000

	path := filepath.Join(t.TempDir(), "soak.db")
	m := soakMandate(t, total)
	boot, err := Open(path, m, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Close()

	base := heapMiB()
	start := time.Now()
	var firstHeap, lastHeap float64
	var firstDB, lastDB int64

	for i := 0; i < total; i++ {
		now := time.Now().UTC()
		args := map[string]any{
			"payment_id": fmt.Sprintf("pay_SYN%06d", i),
			"amount":     int64(1000),
		}
		d := boot.Guard.Decide(policy.RefundTool, args, now)
		if !d.Allowed {
			t.Fatalf("call %d refused: %s: %s", i, d.Rule, d.Reason)
		}
		// Settle it, so the full lifecycle runs rather than only reservations.
		if err := boot.Guard.CommitMany(d.MatchedActionIDs); err != nil {
			t.Fatalf("call %d commit: %v", i, err)
		}

		if i == sample {
			firstHeap, firstDB = heapMiB(), dbBytes(t, path)
		}
		if i == total-1 {
			lastHeap, lastDB = heapMiB(), dbBytes(t, path)
		}
	}
	elapsed := time.Since(start)

	t.Logf("%d refunds in %s  (%.0f/sec)", total, elapsed.Round(time.Millisecond),
		float64(total)/elapsed.Seconds())
	t.Logf("heap: %.1f MiB at start, %.1f at call %d, %.1f at call %d",
		base, firstHeap, sample, lastHeap, total-1)
	t.Logf("db+wal: %d KiB at call %d, %d KiB at call %d",
		firstDB/1024, sample, lastDB/1024, total-1)

	// MEMORY. The ledger keeps one entry per action, so heap grows with the
	// mandate -- that is by design and bounded by the mandate. What must NOT
	// happen is growth per CALL beyond that. 5x between the two samples would
	// mean something accumulates that should not.
	if firstHeap > 0 && lastHeap > firstHeap*5 {
		t.Fatalf("heap grew from %.1f to %.1f MiB across %d calls; a long session "+
			"is accumulating something per call", firstHeap, lastHeap, total-sample)
	}

	// DISK. action_state is one row per action and call_log is pruned in-session,
	// so the file grows with the mandate, not with uptime. A 10x expansion over
	// the last 5/6 of the run would mean the WAL is not checkpointing.
	if firstDB > 0 && lastDB > firstDB*10 {
		t.Fatalf("state file grew from %d to %d KiB; the write-ahead log is not "+
			"being checkpointed and will fill the disk on a long session",
			firstDB/1024, lastDB/1024)
	}
}

// The rate window is the one table written once per forwarded call. Under a
// sustained burst it must stay bounded by its retention, not by uptime.
func TestSoakRateWindowStaysBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("soak test")
	}
	const calls = 4000

	path := filepath.Join(t.TempDir(), "rate.db")
	m := soakMandate(t, 1)
	boot, err := Open(path, m, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Close()

	// Write directly to the durable rate window: this isolates its growth from
	// the action ledger, which legitimately grows with the mandate.
	base := time.Now().UTC().UnixNano()
	for i := 0; i < calls; i++ {
		// Spread across two hours, so the retention window (1h) must discard
		// roughly half of them.
		at := base + int64(i)*int64(2*time.Hour)/int64(calls)
		if err := boot.Store.RecordCall(at); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	rows, err := boot.Store.RecentCalls(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d calls written across 2h; %d retained", calls, len(rows))

	// Pruning keeps one hour. Roughly half the writes fall outside it, so a
	// table still holding everything means pruning is not happening.
	if len(rows) >= calls {
		t.Fatalf("all %d calls are still present; the rate window is not being "+
			"pruned and grows with uptime", calls)
	}
}
