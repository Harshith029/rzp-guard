package lifecycle

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// The state machine is the fail-closed core of this project and had no direct
// test. It was exercised only through policy and relay, which cover the happy
// paths and none of the edges that matter — the ones where an ambiguous outcome
// must NOT return the budget.

// recordingStore is a Persister that can be told to fail, so the
// durability-before-forwarding contract is testable.
type recordingStore struct {
	mu          sync.Mutex
	reserveErr  error
	setStateErr error
	reserves    []string
	transitions []string
}

func (s *recordingStore) Reserve(actionID, receipt string, amountPaise int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reserveErr != nil {
		return s.reserveErr
	}
	s.reserves = append(s.reserves, fmt.Sprintf("%s:%s:%d", actionID, receipt, amountPaise))
	return nil
}

func (s *recordingStore) SetState(actionID, from, to string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setStateErr != nil {
		return s.setStateErr
	}
	s.transitions = append(s.transitions, fmt.Sprintf("%s:%s->%s", actionID, from, to))
	return nil
}

func TestReserveThenCommitConsumesTheAction(t *testing.T) {
	st := &recordingStore{}
	l := NewLedger(100000, st)

	if !l.IsAvailable("a1") {
		t.Fatal("a fresh action must start AVAILABLE")
	}
	if err := l.Reserve("a1", "rzpg_x", 24000); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if got := l.State("a1"); got != Reserved {
		t.Fatalf("state = %s, want %s", got, Reserved)
	}
	// Budget counts reserved, not just committed: two concurrent refunds must
	// not both pass a cumulative check before either result returns.
	if l.Encumbered() != 24000 {
		t.Fatalf("Encumbered = %d, want the reserved amount", l.Encumbered())
	}
	if err := l.Commit("a1"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := l.State("a1"); got != Committed {
		t.Fatalf("state = %s, want %s", got, Committed)
	}
	if l.Committed() != 24000 || l.Encumbered() != 24000 {
		t.Fatalf("after commit: committed=%d encumbered=%d; the spend must stay counted",
			l.Committed(), l.Encumbered())
	}
	// Single use.
	if err := l.Reserve("a1", "rzpg_x", 24000); !errors.Is(err, ErrNotAvailable) {
		t.Fatalf("re-reserving a committed action returned %v, want ErrNotAvailable", err)
	}
}

// The property the whole design rests on: once bytes may have reached the
// provider, an ambiguous outcome must not hand the budget back.
func TestInDoubtIsTerminalAndKeepsTheBudget(t *testing.T) {
	l := NewLedger(50000, &recordingStore{})
	if err := l.Reserve("a1", "r", 30000); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := l.MarkInDoubt("a1"); err != nil {
		t.Fatalf("MarkInDoubt: %v", err)
	}

	if l.Encumbered() != 30000 {
		t.Fatalf("IN_DOUBT released the budget (encumbered=%d); the money may well "+
			"have moved, so it must stay encumbered", l.Encumbered())
	}
	if l.Remaining() != 20000 {
		t.Fatalf("Remaining = %d, want 20000", l.Remaining())
	}

	// Every automatic route out is refused. Only an operator grant may resolve.
	for name, err := range map[string]error{
		"Commit":                    l.Commit("a1"),
		"ReleaseConfirmedRejection": l.ReleaseConfirmedRejection("a1"),
		"MarkInDoubt again":         l.MarkInDoubt("a1"),
	} {
		if !errors.Is(err, ErrBadTransition) {
			t.Errorf("%s on an IN_DOUBT action returned %v, want ErrBadTransition", name, err)
		}
	}
	if got := l.State("a1"); got != InDoubt {
		t.Fatalf("state = %s after failed transitions, want %s", got, InDoubt)
	}
	if ids := l.InDoubtActions(); len(ids) != 1 || ids[0] != "a1" {
		t.Fatalf("InDoubtActions = %v, want [a1] so an operator can find it", ids)
	}
}

// Release exists for ONE case: positive evidence the provider rejected the
// request. It must be reachable only from RESERVED.
func TestReleaseOnlyFromReserved(t *testing.T) {
	l := NewLedger(50000, &recordingStore{})
	if err := l.ReleaseConfirmedRejection("never-reserved"); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("releasing an AVAILABLE action returned %v, want ErrBadTransition", err)
	}

	_ = l.Reserve("a1", "r", 10000)
	if err := l.ReleaseConfirmedRejection("a1"); err != nil {
		t.Fatalf("release from RESERVED: %v", err)
	}
	if l.Encumbered() != 0 {
		t.Fatalf("a confirmed rejection must return the budget, encumbered=%d", l.Encumbered())
	}
	if !l.IsAvailable("a1") {
		t.Fatal("a released action becomes AVAILABLE again")
	}
	// And a committed action can never be released.
	_ = l.Reserve("a2", "r", 10000)
	_ = l.Commit("a2")
	if err := l.ReleaseConfirmedRejection("a2"); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("releasing a COMMITTED action returned %v, want ErrBadTransition", err)
	}
}

func TestCumulativeCapCountsReservationsNotJustCommits(t *testing.T) {
	l := NewLedger(50000, &recordingStore{})

	if err := l.Reserve("a1", "r1", 30000); err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if l.HasHeadroom(30000) {
		t.Fatal("HasHeadroom ignored the outstanding reservation")
	}
	// The second would fit only if the first were not counted until it settled.
	err := l.Reserve("a2", "r2", 30000)
	if !errors.Is(err, ErrCumulativeCap) {
		t.Fatalf("second reserve returned %v, want ErrCumulativeCap; counting only "+
			"committed spend would let two in-flight refunds both pass", err)
	}
	if got := l.State("a2"); got != Available {
		t.Fatalf("a capped action must stay AVAILABLE, got %s", got)
	}
	// Exactly the remainder is allowed.
	if err := l.Reserve("a3", "r3", 20000); err != nil {
		t.Fatalf("reserving exactly the remaining 20000: %v", err)
	}
	if l.Remaining() != 0 {
		t.Fatalf("Remaining = %d, want 0", l.Remaining())
	}
}

// Durability comes BEFORE the in-memory claim. If the write fails there is no
// reservation, so there must be no in-memory one either -- otherwise a crash
// would lose an action the guard believed it had reserved.
func TestFailedDurableWriteLeavesNoClaim(t *testing.T) {
	st := &recordingStore{reserveErr: errors.New("disk full")}
	l := NewLedger(50000, st)

	if err := l.Reserve("a1", "r", 10000); err == nil {
		t.Fatal("Reserve must fail when the durable write fails")
	}
	if got := l.State("a1"); got != Available {
		t.Fatalf("state = %s after a failed durable write, want AVAILABLE", got)
	}
	if l.Encumbered() != 0 {
		t.Fatalf("a failed reserve encumbered %d paise", l.Encumbered())
	}

	// Same rule on the way out: a failed state write must not move the entry.
	st.reserveErr = nil
	if err := l.Reserve("a2", "r", 10000); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	st.setStateErr = errors.New("disk full")
	if err := l.Commit("a2"); err == nil {
		t.Fatal("Commit must fail when the durable write fails")
	}
	if got := l.State("a2"); got != Reserved {
		t.Fatalf("state = %s after a failed commit write, want RESERVED", got)
	}
}

// Restore keeps a surviving RESERVED row ENCUMBERED.
//
// The promotion of mid-flight reservations to IN_DOUBT belongs to the store's
// recovery step, which runs first (see storage.RecoverStartup, covered
// end-to-end by TestBootstrapRecoversMidFlightReservationAsInDoubt). Restore's
// own job is the defence-in-depth half: if a RESERVED row reaches it anyway --
// recovery skipped, a new row racing in, a future caller wiring the two in the
// wrong order -- the budget must still be held and the action must not be
// reusable.
//
// An earlier version of this test asserted Restore did the promotion itself.
// That was my misreading, not a defect; the doc comment says which layer owns
// it. Recorded here because a test that encodes the wrong contract is worse
// than no test: it entrenches a misunderstanding.
func TestRestoreKeepsSurvivingReservationsEncumbered(t *testing.T) {
	l := NewLedger(100000, &recordingStore{})
	l.Restore(
		map[string]string{"a1": string(Reserved), "a2": string(Committed), "a3": string(InDoubt)},
		map[string]int64{"a1": 5000, "a2": 6000, "a3": 7000},
	)

	if got := l.State("a1"); got != Reserved {
		t.Fatalf("Restore altered a RESERVED row to %s; promotion is the store's job", got)
	}
	if l.IsAvailable("a1") {
		t.Fatal("a surviving RESERVED row must not be reusable")
	}
	if err := l.Reserve("a1", "r", 5000); !errors.Is(err, ErrNotAvailable) {
		t.Fatalf("re-reserving a restored RESERVED action returned %v, want ErrNotAvailable", err)
	}
	if got := l.State("a2"); got != Committed {
		t.Fatalf("COMMITTED must survive restore, got %s", got)
	}
	if got := l.State("a3"); got != InDoubt {
		t.Fatalf("IN_DOUBT must survive restore, got %s", got)
	}
	// All three remain encumbered: none of them is spendable again.
	if l.Encumbered() != 18000 {
		t.Fatalf("Encumbered = %d, want 18000 — a restart must not free budget",
			l.Encumbered())
	}
	if l.IsAvailable("a1") {
		t.Fatal("a recovered in-flight action must not be reusable")
	}
}

// The ledger is reached from two pumps running concurrently.
func TestConcurrentReservesRespectTheCap(t *testing.T) {
	l := NewLedger(10000, &recordingStore{})

	const n = 32
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // maximise overlap
			errs[i] = l.Reserve(fmt.Sprintf("a%d", i), "r", 1000)
		}(i)
	}
	close(start)
	wg.Wait()

	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		}
	}
	if ok != 10 {
		t.Fatalf("%d reservations of 1000 succeeded against a 10000 cap, want exactly 10", ok)
	}
	if l.Encumbered() != 10000 {
		t.Fatalf("Encumbered = %d, want exactly the cap", l.Encumbered())
	}
}

// The set forms of the Persister, for the fake. They delegate to the single
// forms so the recorded call log and the injected errors behave identically
// whether a test exercises one action or several.
func (s *recordingStore) ReserveMany(receipt string, rs []Reservation) error {
	for _, r := range rs {
		if err := s.Reserve(r.ActionID, receipt, r.AmountPaise); err != nil {
			return err
		}
	}
	return nil
}

func (s *recordingStore) SetStateMany(actionIDs []string, from, to string) error {
	for _, id := range actionIDs {
		if err := s.SetState(id, from, to); err != nil {
			return err
		}
	}
	return nil
}
