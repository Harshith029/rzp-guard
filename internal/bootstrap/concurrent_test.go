package bootstrap

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/harshith/rzp-guard/internal/mandate"
	"github.com/harshith/rzp-guard/internal/policy"
	"github.com/harshith/rzp-guard/internal/storage"
)

// HOW THIS SYSTEM ACTUALLY SCALES, tested rather than asserted.
//
// The README has always said "one guard per state file, enforced by an
// exclusive lock", and that has been read -- including by me, in the project
// dossier -- as "it does not scale". Those are different claims and only the
// first was ever true.
//
// The lock excludes a second guard from ONE state file. It says nothing about
// many guards over many state files, which is the shape a real deployment
// takes: one agent session, one mandate, one state file. The natural shard key
// is the mandate, and it was there from the start.
//
// What had never been demonstrated is that concurrent sessions are genuinely
// independent -- that budgets, ledgers and receipts do not leak between them.
// Untested isolation is not isolation, so these run real concurrent guards and
// check it.

// buildMandate makes a distinct mandate with n actions of amount each.
func buildMandate(t *testing.T, id string, n int, amount int64) *mandate.Mandate {
	t.Helper()
	acts := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		acts = append(acts, map[string]any{
			"action_id":    fmt.Sprintf("%s_%02d", id, i),
			"payment_id":   fmt.Sprintf("pay_SYN%s%02d", id, i),
			"amount_paise": amount,
		})
	}
	doc := map[string]any{
		"mandate_id":    id,
		"expires_at":    time.Now().UTC().Add(4 * time.Hour).Format(time.RFC3339),
		"allowed_tools": []string{"fetch_payment", policy.RefundTool},
		"global": map[string]any{
			"max_cumulative_paise": amount * int64(n),
			"max_calls_per_minute": 1000,
		},
		"authorized_refund_actions": acts,
	}
	raw, _ := json.Marshal(doc)
	m, err := mandate.Load(raw)
	if err != nil {
		t.Fatalf("mandate.Load: %v", err)
	}
	return m
}

// THE SCALING CLAIM: N concurrent sessions, each with its own mandate and state
// file, run independently and correctly.
func TestManyConcurrentSessionsAreIndependent(t *testing.T) {
	const sessions = 24
	const perSession = 8
	const amount = int64(1000)

	dir := t.TempDir()
	now := time.Now().UTC()

	type result struct {
		mandateID  string
		allowed    int
		encumbered int64
		err        error
	}
	results := make([]result, sessions)

	var wg sync.WaitGroup
	start := make(chan struct{}) // release them together, to maximise overlap
	for i := 0; i < sessions; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("mnd_s%02d", i)
			m := buildMandate(t, id, perSession, amount)

			<-start
			boot, err := Open(filepath.Join(dir, id+".db"), m, now)
			if err != nil {
				results[i] = result{mandateID: id, err: err}
				return
			}
			defer boot.Close()

			r := result{mandateID: id}
			for j := 0; j < perSession; j++ {
				args := map[string]any{
					"payment_id": fmt.Sprintf("pay_SYN%s%02d", id, j),
					"amount":     amount,
				}
				if d := boot.Guard.Decide(policy.RefundTool, args, now); d.Allowed {
					r.allowed++
				}
			}
			r.encumbered = boot.Guard.Encumbered()
			results[i] = r
		}(i)
	}
	close(start)
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			t.Fatalf("session %d (%s) failed to start: %v", i, r.mandateID, r.err)
		}
		if r.allowed != perSession {
			t.Fatalf("session %s authorized %d of %d refunds; concurrent sessions "+
				"are interfering with each other", r.mandateID, r.allowed, perSession)
		}
		// The decisive check: each session's budget reflects ITS OWN spending
		// only. A shared or leaked ledger would show a multiple of this.
		if want := amount * perSession; r.encumbered != want {
			t.Fatalf("session %s encumbered %d, want %d; budget is leaking between "+
				"concurrent sessions", r.mandateID, r.encumbered, want)
		}
	}
}

// Each session's durable state must stay in its own file. A session reading
// another's rows would mean one mandate could consume another's authorizations.
func TestConcurrentSessionsDoNotShareDurableState(t *testing.T) {
	const sessions = 12
	dir := t.TempDir()
	now := time.Now().UTC()

	var wg sync.WaitGroup
	errs := make([]error, sessions)
	for i := 0; i < sessions; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("mnd_i%02d", i)
			m := buildMandate(t, id, 3, 500)
			boot, err := Open(filepath.Join(dir, id+".db"), m, now)
			if err != nil {
				errs[i] = err
				return
			}
			defer boot.Close()

			args := map[string]any{
				"payment_id": fmt.Sprintf("pay_SYN%s00", id),
				"amount":     int64(500),
			}
			if d := boot.Guard.Decide(policy.RefundTool, args, now); !d.Allowed {
				errs[i] = fmt.Errorf("%s: refused its own action: %s", id, d.Reason)
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
	}

	// Now reopen each file alone and confirm it holds exactly ONE reserved
	// action -- its own, and nobody else's.
	for i := 0; i < sessions; i++ {
		id := fmt.Sprintf("mnd_i%02d", i)
		st, err := storage.Open(filepath.Join(dir, id+".db"), id)
		if err != nil {
			t.Fatalf("reopen %s: %v", id, err)
		}
		// RESERVED, not IN_DOUBT: storage.Open does not run recovery -- only
		// bootstrap.Open does -- so the row is exactly as the session left it.
		rows, err := st.ActionsInState("RESERVED")
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("%s holds %d reserved actions, want exactly its own 1; "+
				"state is bleeding across session files", id, len(rows))
		}
		if want := id + "_00"; rows[0].ActionID != want {
			t.Fatalf("%s holds action %s, want %s", id, rows[0].ActionID, want)
		}
		_ = st.Close()
	}
}

// Ownership is per MANDATE. Two guards on the SAME mandate must still be
// refused -- that is the guarantee sharding relies on, so it must survive
// contention.
//
// THIS TEST CAUGHT A REAL DEFECT. On ca1e4c1 it failed in CI with "0 of 16
// concurrent opens succeeded": not one winner and fifteen refusals, but NOBODY.
// Under locking_mode = EXCLUSIVE every opener held SHARED and none could
// upgrade. Reproduced 1 run in 12 under --cpus=0.5 with -race, then fixed by
// the bounded retry in storage.Open.
//
// The old version of this test discarded the error and counted successes, so it
// could see "0 of 16" but could not say whether the fifteen losers were refused
// as owners, refused for some unrelated reason, or still running. It now checks
// all four things the fix has to deliver.
func TestTheMandateLeaseHoldsUnderConcurrentOpens(t *testing.T) {
	const attempts = 16
	// storage.Open waits out contention for its own internal deadline (2s at the
	// time of writing) and then refuses. This bound is deliberately far looser,
	// because it has to hold under -race on a constrained CPU. It still fails if
	// any loser ever waits without a bound, which is the property under test.
	const bound = 90 * time.Second

	path := filepath.Join(t.TempDir(), "contended.db")
	m := buildMandate(t, "mnd_contend", 2, 1000)
	now := time.Now().UTC()

	var wg sync.WaitGroup
	errs := make([]error, attempts)
	live := make([]*Result, attempts)

	start := time.Now()
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			boot, err := Open(path, m, now)
			errs[i] = err
			live[i] = boot
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	// 1. Exactly one owner.
	winner, n := -1, 0
	for i, err := range errs {
		if err == nil {
			n++
			winner = i
		}
	}
	if n != 1 {
		for _, b := range live {
			if b != nil {
				_ = b.Close()
			}
		}
		t.Fatalf("%d of %d concurrent opens succeeded on ONE state file, want "+
			"exactly 1; each would enforce the cumulative cap against its own "+
			"in-memory ledger, so between them they could spend past it. Zero is "+
			"the deadlock this test caught on ca1e4c1", n, attempts)
	}
	defer live[winner].Close()

	// 2. Every loser refused as a NAMED ownership conflict, within a bound. An
	//    anonymous failure would be indistinguishable from a corrupt state file,
	//    and an unbounded wait would hang the guard at startup instead.
	for i, err := range errs {
		if i == winner {
			continue
		}
		if !errors.Is(err, storage.ErrNotOwner) {
			t.Fatalf("open %d was refused with %v, want storage.ErrNotOwner", i, err)
		}
	}
	if elapsed > bound {
		t.Fatalf("%d contending opens took %v, over the %v bound: a loser is "+
			"waiting without a deadline", attempts, elapsed, bound)
	}

	// 3. The winner still OWNS it -- both halves. It can still write through its
	//    own handle, and the file is still closed to everyone else. A "fix" that
	//    let the losers in once the winner settled would satisfy (1) and (2) and
	//    still be the two-ledger bug.
	args := map[string]any{"payment_id": "pay_SYNmnd_contend00", "amount": int64(1000)}
	if d := live[winner].Guard.Decide(policy.RefundTool, args, now); !d.Allowed {
		t.Fatalf("the owner was refused its own action after winning the race: %s", d.Reason)
	}
	late, err := storage.Open(path, "mnd_contend")
	if err == nil {
		late.Close()
		t.Fatal("a later open took the mandate while the owner was still live")
	}
	if !errors.Is(err, storage.ErrNotOwner) {
		t.Fatalf("a later open was refused with %v, want storage.ErrNotOwner", err)
	}
}
