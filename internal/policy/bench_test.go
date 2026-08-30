package policy

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/harshith/rzp-guard/internal/mandate"
	"github.com/harshith/rzp-guard/internal/storage"
)

// No performance number for this system existed before these benchmarks. The
// design implies the decision is cheap and the durable write dominates, but
// "implies" is not a measurement, and the README said so.
//
// What matters operationally is the RATIO: the guard sits in front of a network
// call to Razorpay that costs 100-500ms. If the guard's own cost is in the
// microseconds it is free; if a large mandate pushes it into milliseconds that
// is a real budget, because Decide() runs under a mutex that serialises every
// concurrent refund in the session.

// benchMandate builds a mandate with n distinct actions, through the real
// loader so validation cost is included.
func benchMandate(b *testing.B, n int) *mandate.Mandate {
	b.Helper()
	acts := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		acts = append(acts, map[string]any{
			"action_id":    fmt.Sprintf("act_%05d", i),
			"payment_id":   fmt.Sprintf("pay_SYN%05d", i),
			"amount_paise": 10000 + i,
		})
	}
	doc := map[string]any{
		"mandate_id":    "mnd_bench",
		"expires_at":    time.Now().UTC().Add(4 * time.Hour).Format(time.RFC3339),
		"allowed_tools": []string{"fetch_payment", RefundTool},
		"global": map[string]any{
			"max_cumulative_paise": int64(1) << 40,
			"max_calls_per_minute": 1 << 30,
		},
		"authorized_refund_actions": acts,
	}
	raw, _ := json.Marshal(doc)
	m, err := mandate.Load(raw)
	if err != nil {
		b.Fatalf("mandate.Load: %v", err)
	}
	return m
}

func benchArgs(b *testing.B, s string) map[string]any {
	b.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		b.Fatal(err)
	}
	return m
}

// The pure decision with no durable store: the authorization logic alone.
func BenchmarkDecideAllowInMemory(b *testing.B) {
	m := benchMandate(b, 4096)
	args := benchArgs(b, `{"payment_id":"pay_SYN00003","amount":10003}`)
	now := time.Now().UTC()

	_ = args
	// Actions are single-use, so each iteration must consume a different one.
	// Creating a fresh Guard per iteration and fencing it with StopTimer/
	// StartTimer measured mostly timer overhead -- 11us for work that is
	// actually sub-microsecond. One guard, a pool of distinct payments.
	g := New(m)
	pool := make([]map[string]any, 4096)
	for i := range pool {
		pool[i] = benchArgs(b, fmt.Sprintf(
			`{"payment_id":"pay_SYN%05d","amount":%d}`, i, 10000+i))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i >= len(pool) {
			b.Fatalf("raise the action pool: b.N=%d exceeds %d", b.N, len(pool))
		}
		d := g.Decide(RefundTool, pool[i], now)
		if !d.Allowed {
			b.Fatalf("iteration %d: %s: %s", i, d.Rule, d.Reason)
		}
	}
}

// The deny path for an unknown payment. This is the shape a hostile call takes,
// so it is the one an attacker could try to make expensive.
func BenchmarkDecideDenyNoAction(b *testing.B) {
	g := New(benchMandate(b, 8))
	args := benchArgs(b, `{"payment_id":"pay_NOTREAL","amount":10000}`)
	now := time.Now().UTC()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if d := g.Decide(RefundTool, args, now); d.Allowed {
			b.Fatal("expected deny")
		}
	}
}

// A permitted read: allowlist check only, then pass through.
func BenchmarkDecideAllowedRead(b *testing.B) {
	g := New(benchMandate(b, 8))
	args := benchArgs(b, `{"payment_id":"pay_SYN00003"}`)
	now := time.Now().UTC()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if d := g.Decide("fetch_payment", args, now); !d.Allowed {
			b.Fatal("expected allow")
		}
	}
}

// How the deny path scales with mandate size. mandate.Find() is a linear scan,
// so this is where an O(n) surprise would show up.
func BenchmarkDecideDenyByMandateSize(b *testing.B) {
	for _, n := range []int{1, 10, 100, 1000} {
		b.Run(fmt.Sprintf("actions=%d", n), func(b *testing.B) {
			g := New(benchMandate(b, n))
			args := benchArgs(b, `{"payment_id":"pay_NOTREAL","amount":10000}`)
			now := time.Now().UTC()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				g.Decide(RefundTool, args, now)
			}
		})
	}
}

// THE NUMBER THAT MATTERS: the full authorized path including the durable
// reservation and the durable rate-window write. Two SQLite writes.
func BenchmarkDecideAllowDurable(b *testing.B) {
	m := benchMandate(b, 2000) // enough distinct actions for b.N iterations
	st, err := storage.Open(filepath.Join(b.TempDir(), "bench.db"), m.MandateID)
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()

	g := NewWithStore(m, st)
	now := time.Now().UTC()
	args := make([]map[string]any, 2000)
	for i := range args {
		args[i] = benchArgs(b, fmt.Sprintf(
			`{"payment_id":"pay_SYN%05d","amount":%d}`, i, 10000+i))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i >= len(args) {
			b.StopTimer()
			b.SetBytes(0)
			b.Fatalf("benchmark exceeded %d prepared actions; raise the pool", len(args))
		}
		d := g.Decide(RefundTool, args[i], now)
		if !d.Allowed {
			b.Fatalf("iteration %d: %s: %s", i, d.Rule, d.Reason)
		}
	}
}

// The receipt derivation alone: one SHA-256 over a short string.
func BenchmarkReceiptFor(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := mandate.ReceiptFor("mnd_bench", "act_00042"); err != nil {
			b.Fatal(err)
		}
	}
}
