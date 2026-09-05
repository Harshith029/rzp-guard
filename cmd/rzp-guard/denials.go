package main

import (
	"fmt"
	"sync"
	"time"
)

// A REGRESSION THIS COMMIT INTRODUCED, AND THE OPEN FINDING IT SITS ON TOP OF.
//
// The engineering audit lists, still OPEN: *"No rate limit on refusals; an agent
// can loop. Bounded, no state change, but noisy."* That was tolerable precisely
// because a refusal cost 779 nanoseconds and touched nothing durable -- the
// looping agent wasted its own time and nobody else's.
//
// The denial queue changed that. Recording every refusal put a SQLite UPSERT on
// the deny path, so an agent looping on a refused call now drives one durable
// write per attempt at roughly 2.5ms each. That converts "noisy" into a way for
// an untrusted party to saturate the state file the money path depends on,
// which is a worse property than the one the queue was added to fix.
//
// So the queue coalesces, and the writes are bounded.
//
// WHAT COALESCING COSTS: up to flushEvery of staleness on the occurrence count
// an operator sees. Nothing else. The first refusal of anything new is written
// immediately, because the whole point of the queue is that a person sees a
// blocked refund promptly.
//
// WHAT THE RATE CAP COSTS, STATED PLAINLY: under a flood of DISTINCT refusals,
// some are not recorded. That is a real loss of visibility and it is the right
// trade -- the alternative is letting an untrusted party decide how much the
// guard writes to disk. It is counted rather than silent: `dropped` is exported
// as a metric and named in the queue's own output, so "the queue is short"
// never quietly means "the queue overflowed".

const (
	// flushEvery bounds how stale a repeat count may be. Short enough that an
	// operator watching a live incident sees the retries climbing.
	flushEvery = 5 * time.Second

	// maxTracked bounds memory. An agent can mint unlimited distinct payment
	// ids, and a map keyed on them is an unbounded allocation an untrusted party
	// controls.
	maxTracked = 2048

	// maxWritesPerSecond bounds durable writes from the deny path. Ten a second
	// is far above any real refusal rate -- a merchant refusing ten legitimate
	// refunds a second has a problem the queue cannot help with -- and far below
	// what a loop can generate.
	maxWritesPerSecond = 10
)

// denialKey identifies one distinct refusal. It matches the UNIQUE constraint in
// the denial table, so what is coalesced in memory is exactly what would have
// been coalesced in SQL.
type denialKey struct {
	rule      string
	paymentID string
	amount    int64
}

// denialSink is what the recorder writes through. An interface so a test can
// count writes without a database, and so the recorder cannot reach anything
// else on the store.
type denialSink interface {
	RecordDenial(tool, rule, paymentID string, amountPaise int64, reason string) error
}

type pendingDenial struct {
	tool, reason string
	count        int
	lastFlush    time.Time
}

// denialRecorder coalesces refusals and caps how fast they reach the disk.
type denialRecorder struct {
	mu      sync.Mutex
	sink    denialSink
	now     func() time.Time
	pending map[denialKey]*pendingDenial

	// A simple token bucket over a one-second window. Not a rolling window:
	// this bounds disk writes, and an extra write at a window boundary is not
	// worth the bookkeeping.
	windowStart  time.Time
	windowWrites int

	dropped int64
	onError func(error)
}

func newDenialRecorder(sink denialSink, onError func(error)) *denialRecorder {
	return &denialRecorder{
		sink:    sink,
		now:     func() time.Time { return time.Now().UTC() },
		pending: make(map[denialKey]*pendingDenial, 64),
		onError: onError,
	}
}

// record notes one refusal, writing it through if it is new or due.
//
// NEVER FATAL. The refusal has already happened and nothing was forwarded, so
// losing a queue entry costs visibility, not safety. A guard that died because
// it could not record something it correctly refused would be trading the money
// path against the reporting path.
func (d *denialRecorder) record(tool, rule, paymentID string, amount int64, reason string) {
	k := denialKey{rule: rule, paymentID: paymentID, amount: amount}
	now := d.now()

	d.mu.Lock()
	p, seen := d.pending[k]
	if !seen {
		if len(d.pending) >= maxTracked {
			// The map is full of distinct refusals, which on a real merchant does
			// not happen and under a hostile agent is the normal case. Count it
			// and move on rather than growing an allocation an attacker sizes.
			d.dropped++
			d.mu.Unlock()
			return
		}
		p = &pendingDenial{tool: tool, reason: reason}
		d.pending[k] = p
	}
	p.count++
	p.reason = reason

	// A refusal nobody has seen before goes to disk NOW. Delaying the first one
	// would mean a customer waits up to flushEvery before their blocked refund
	// is even visible, which is the thing the queue exists to prevent.
	due := !seen || now.Sub(p.lastFlush) >= flushEvery
	if !due {
		d.mu.Unlock()
		return
	}
	if !d.allowWriteLocked(now) {
		d.dropped++
		d.mu.Unlock()
		return
	}
	p.lastFlush = now
	count := p.count
	p.count = 0
	tool, reason = p.tool, p.reason
	d.mu.Unlock()

	d.write(k, tool, reason, count)
}

// allowWriteLocked is the token bucket. Caller holds d.mu.
func (d *denialRecorder) allowWriteLocked(now time.Time) bool {
	if now.Sub(d.windowStart) >= time.Second {
		d.windowStart = now
		d.windowWrites = 0
	}
	if d.windowWrites >= maxWritesPerSecond {
		return false
	}
	d.windowWrites++
	return true
}

// write pushes one entry through, repeating it `count` times so the occurrence
// counter in the database matches what actually happened.
//
// The repeat is the UPSERT running count times, which looks wasteful and is not:
// it is bounded by flushEvery times the agent's request rate, happens at most
// maxWritesPerSecond times a second, and keeping the count truthful is the whole
// reason an operator can tell a stuck agent from a one-off.
func (d *denialRecorder) write(k denialKey, tool, reason string, count int) {
	if count <= 0 {
		return
	}
	// Cap the repeat. A count that large is a loop, and the operator needs to
	// know it is a loop rather than the exact integer.
	if count > 100 {
		count = 100
	}
	for i := 0; i < count; i++ {
		if err := d.sink.RecordDenial(tool, k.rule, k.paymentID, k.amount,
			clip(reason, 512)); err != nil {
			if d.onError != nil {
				d.onError(err)
			}
			return
		}
	}
}

// Flush writes everything still buffered. Call on shutdown, or the last few
// seconds of refusals before a restart are lost -- which are exactly the ones
// somebody is likely to be asking about.
//
// It ignores the rate cap: this runs once, at exit, and the writes it performs
// are bounded by the size of the map rather than by anything an agent controls.
func (d *denialRecorder) Flush() {
	d.mu.Lock()
	type item struct {
		k            denialKey
		tool, reason string
		count        int
	}
	var out []item
	for k, p := range d.pending {
		if p.count > 0 {
			out = append(out, item{k, p.tool, p.reason, p.count})
			p.count = 0
		}
	}
	d.mu.Unlock()

	for _, it := range out {
		d.write(it.k, it.tool, it.reason, it.count)
	}
}

// Dropped reports refusals never recorded, for the metrics endpoint. A queue
// that is short because it overflowed must not look like a queue that is short
// because nothing was refused.
func (d *denialRecorder) Dropped() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dropped
}

// DroppedNote is the line an operator sees when visibility has actually been
// lost, rather than a number they have to notice on a dashboard.
func (d *denialRecorder) DroppedNote() string {
	n := d.Dropped()
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d refusal(s) were NOT recorded: the deny path is rate "+
		"limited so an agent looping on a refused call cannot saturate the state "+
		"file. The queue below is incomplete", n)
}
