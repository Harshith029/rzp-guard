package relay

import (
	"fmt"
	"io"
	"sync"
)

// ChildTee writes each line to the child and copies it to an audit sink.
//
// WHY THIS EXISTS RATHER THAN io.MultiWriter.
//
// The relay decides whether a failed write may release a single-use
// authorization by asking how many bytes moved: zero is provably pre-dispatch
// and safe to release, anything else is ambiguous and must go IN_DOUBT.
//
// io.MultiWriter returns the byte count of the writer that FAILED, not of the
// ones that succeeded. Under -child-tee the writers are (child, evidence file),
// so a child that accepted every byte followed by a tee that accepted none
// returns n == 0 -- indistinguishable from a write that never left the process.
// The relay would release an authorization for a refund already on its way to
// Razorpay, and that authorization can then be spent a second time.
//
// So the child's count is the only one that answers "did this leave the
// process", and it is the only one this returns. A broken audit copy is a real
// problem, but it is not evidence that nothing was dispatched.
type ChildTee struct {
	child io.Writer
	audit io.Writer

	mu     sync.Mutex
	broken bool
	// onBreak reports the first audit failure. The session continues -- the
	// child holds the bytes and its response still has to be handled -- but the
	// evidence file is now short, and nothing downstream may read it as a
	// complete record of what crossed the boundary.
	onBreak func(error)
}

// NewChildTee returns a writer that forwards to child and mirrors to audit.
// onBreak is called at most once, the first time the audit copy fails.
func NewChildTee(child, audit io.Writer, onBreak func(error)) *ChildTee {
	return &ChildTee{child: child, audit: audit, onBreak: onBreak}
}

func (t *ChildTee) Write(p []byte) (int, error) {
	// The child goes first and its result is returned unmodified. Everything
	// after this point is bookkeeping that must not change what the caller
	// believes about dispatch.
	n, err := t.child.Write(p)
	if err != nil {
		return n, err
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.broken {
		return n, nil
	}
	// Mirror only what the child actually took, so the evidence file cannot
	// claim more crossed the boundary than did.
	if _, aerr := t.audit.Write(p[:n]); aerr != nil {
		t.broken = true
		if t.onBreak != nil {
			t.onBreak(fmt.Errorf("child tee failed after %d bytes reached the child; "+
				"the evidence file is now incomplete and must not be read as a full "+
				"record: %w", n, aerr))
		}
	}
	return n, nil
}

// AuditBroken reports whether the audit copy has failed at any point.
func (t *ChildTee) AuditBroken() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.broken
}
