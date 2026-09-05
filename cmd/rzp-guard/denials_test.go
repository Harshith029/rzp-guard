package main

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// countingSink records what actually reached the database.
type countingSink struct {
	mu     sync.Mutex
	writes int
	rows   map[denialKey]int
	err    error
}

func newCountingSink() *countingSink {
	return &countingSink{rows: map[denialKey]int{}}
}

func (c *countingSink) RecordDenial(tool, rule, paymentID string, amount int64, reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.writes++
	c.rows[denialKey{rule, paymentID, amount}]++
	return nil
}

func (c *countingSink) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes
}

// fixedClock lets the flush interval be crossed without sleeping.
type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fixedClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}
func (f *fixedClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

func newTestRecorder(t *testing.T) (*denialRecorder, *countingSink, *fixedClock) {
	t.Helper()
	sink := newCountingSink()
	clock := &fixedClock{t: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)}
	d := newDenialRecorder(sink, nil)
	d.now = clock.now
	return d, sink, clock
}

// THE REGRESSION THIS EXISTS TO PREVENT.
//
// A denial cost 779ns and touched nothing durable, which is why an agent
// looping on a refused call was merely noisy. Recording every refusal put a
// SQLite write on that path at ~2.5ms each, handing an untrusted party a way to
// saturate the state file the money path depends on.
func TestALoopingAgentDoesNotDriveAWritePerRefusal(t *testing.T) {
	d, sink, _ := newTestRecorder(t)

	for i := 0; i < 1000; i++ {
		d.record("create_refund", "NO_AUTHORIZED_ACTION", "pay_SYN0001", 7500, "no action")
	}

	// One write: the first, because a refusal nobody has seen must be visible
	// immediately. The other 999 are coalesced.
	if got := sink.count(); got != 1 {
		t.Fatalf("1000 identical refusals drove %d durable writes, want 1", got)
	}
}

// The first refusal of anything NEW still goes straight to disk. Delaying it
// would mean a customer waits up to the flush interval before their blocked
// refund is even visible, which is what the queue exists to prevent.
func TestANewRefusalIsRecordedImmediately(t *testing.T) {
	d, sink, _ := newTestRecorder(t)
	d.record("create_refund", "NO_AUTHORIZED_ACTION", "pay_SYN0001", 7500, "x")
	if got := sink.count(); got != 1 {
		t.Fatalf("a refusal nobody had seen took %d writes to appear, want 1", got)
	}
	d.record("create_refund", "NO_AUTHORIZED_ACTION", "pay_SYN0002", 7500, "x")
	if got := sink.count(); got != 2 {
		t.Fatalf("a second, distinct refusal did not appear: %d writes", got)
	}
}

// The repeat count must stay truthful, or an operator cannot tell a stuck agent
// from a one-off. Crossing the flush interval writes the accumulated count.
func TestTheRetryCountSurvivesCoalescing(t *testing.T) {
	d, sink, clock := newTestRecorder(t)
	k := denialKey{"NO_AUTHORIZED_ACTION", "pay_SYN0001", 7500}

	d.record("create_refund", k.rule, k.paymentID, k.amount, "x") // writes, count 1
	for i := 0; i < 9; i++ {
		d.record("create_refund", k.rule, k.paymentID, k.amount, "x") // buffered
	}
	clock.advance(flushEvery + time.Second)
	d.record("create_refund", k.rule, k.paymentID, k.amount, "x") // due: flushes 10

	sink.mu.Lock()
	got := sink.rows[k]
	sink.mu.Unlock()
	if got != 11 {
		t.Fatalf("the database saw %d occurrences of 11 refusals; an operator "+
			"cannot tell a loop from a one-off", got)
	}
}

// A flood of DISTINCT refusals is the case an attacker controls: unlimited
// payment ids means an unbounded map and an unbounded write rate. Both are
// capped, and what is lost is COUNTED -- a short queue because it overflowed
// must not look like a short queue because nothing was refused.
func TestAFloodOfDistinctRefusalsIsCappedAndCounted(t *testing.T) {
	d, sink, _ := newTestRecorder(t)

	for i := 0; i < 5000; i++ {
		d.record("create_refund", "NO_AUTHORIZED_ACTION",
			"pay_SYNflood"+string(rune('a'+i%26))+itoa(i), int64(1000+i), "x")
	}

	if got := sink.count(); got > maxWritesPerSecond+1 {
		t.Errorf("5000 distinct refusals in one second drove %d durable writes; "+
			"the cap is %d", got, maxWritesPerSecond)
	}
	if d.Dropped() == 0 {
		t.Fatal("nothing was reported dropped, so the queue is silently " +
			"incomplete -- which is worse than being short")
	}
	if d.DroppedNote() == "" {
		t.Error("no operator-facing note that the queue is incomplete")
	}
}

// Whatever is still buffered at shutdown must reach disk. The last few seconds
// of refusals before a restart are exactly the ones somebody will ask about.
func TestFlushWritesWhatIsStillBuffered(t *testing.T) {
	d, sink, _ := newTestRecorder(t)
	k := denialKey{"NO_AUTHORIZED_ACTION", "pay_SYN0001", 7500}

	d.record("create_refund", k.rule, k.paymentID, k.amount, "x")
	for i := 0; i < 4; i++ {
		d.record("create_refund", k.rule, k.paymentID, k.amount, "x")
	}
	d.Flush()

	sink.mu.Lock()
	got := sink.rows[k]
	sink.mu.Unlock()
	if got != 5 {
		t.Fatalf("shutdown left %d of 5 refusals unrecorded", 5-got)
	}
}

// A broken store must not take the guard down. The refusal already happened and
// nothing was forwarded, so losing the record costs visibility, not safety.
func TestAFailingStoreIsReportedNotFatal(t *testing.T) {
	sink := newCountingSink()
	sink.err = errors.New("database is locked")
	var reported int
	d := newDenialRecorder(sink, func(error) { reported++ })

	d.record("create_refund", "NO_AUTHORIZED_ACTION", "pay_SYN0001", 7500, "x")
	if reported == 0 {
		t.Error("a broken queue was silent; an operator would think nothing had " +
			"been refused")
	}
}

// itoa avoids pulling strconv in just for a fixture.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
