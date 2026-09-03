package relay

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/harshith/rzp-guard/internal/lifecycle"
)

// failingTee accepts nothing and fails, standing in for a full disk or an
// evidence file unlinked underneath a running guard.
type failingTee struct{}

func (failingTee) Write(b []byte) (int, error) { return 0, errors.New("no space left on device") }

// The trap this whole file exists for, asserted directly so it cannot be
// reintroduced by someone reaching for the obvious standard-library helper.
//
// io.MultiWriter returns the count of the writer that FAILED. With the child
// first and the evidence file second, a child that took every byte followed by a
// tee that took none reports zero -- which the relay reads as "provably
// pre-dispatch" and releases a single-use authorization on.
func TestMultiWriterReportsZeroAfterTheChildTookEverything(t *testing.T) {
	child := &bytes.Buffer{}
	n, err := io.MultiWriter(child, failingTee{}).Write([]byte("hello"))
	if err == nil {
		t.Fatal("expected the tee failure to surface")
	}
	if child.Len() != 5 {
		t.Fatalf("child took %d bytes, want 5", child.Len())
	}
	if n != 0 {
		t.Fatalf("n = %d; this test encodes WHY io.MultiWriter is unusable here. "+
			"If it no longer returns 0 the standard library changed and "+
			"child_tee.go should be re-justified.", n)
	}
}

// ChildTee must report what the CHILD took, whatever the audit copy did.
func TestChildTeeReportsTheChildsCountNotTheTees(t *testing.T) {
	child := &bytes.Buffer{}
	var broke error
	tee := NewChildTee(child, failingTee{}, func(err error) { broke = err })

	n, err := tee.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("a failed audit copy must not fail the write: %v", err)
	}
	if n != 5 {
		t.Fatalf("n = %d, want 5 -- the child accepted every byte", n)
	}
	if !tee.AuditBroken() {
		t.Fatal("the audit failure was not recorded")
	}
	if broke == nil {
		t.Fatal("onBreak was never called, so nothing would warn that the evidence is short")
	}

	// It must not keep writing to a sink it knows is broken, and must stay
	// truthful about the child on every subsequent line.
	if n, err = tee.Write([]byte("again")); err != nil || n != 5 {
		t.Fatalf("second write: n=%d err=%v, want 5 and nil", n, err)
	}
	if got := child.String(); got != "helloagain" {
		t.Fatalf("child received %q, want %q", got, "helloagain")
	}
}

// A child failure must still propagate: the release decision depends on it.
func TestChildTeePropagatesChildFailure(t *testing.T) {
	tee := NewChildTee(zeroWriter{}, &bytes.Buffer{}, nil)
	n, err := tee.Write([]byte("hello"))
	if err == nil {
		t.Fatal("a failed child write must surface")
	}
	if n != 0 {
		t.Fatalf("n = %d, want 0 -- the child took nothing", n)
	}
}

// A FAILED AUDIT COPY MUST NOT RELEASE AN AUTHORIZATION THE CHILD ALREADY HAS.
//
// End to end, through the relay, wired the way cmd/rzp-guard/main.go wires it
// under -child-tee. Before the fix this released rfa_001 back to AVAILABLE after
// 167 bytes had reached the child: the refund was on its way to Razorpay and its
// single-use grant was spendable again.
func TestFailedTeeDoesNotReleaseAfterTheChildAcceptedTheBytes(t *testing.T) {
	g := newGuard(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":20000}]`)
	child := &bytes.Buffer{}
	agent := &bytes.Buffer{}

	r := New(g, NewChildTee(child, failingTee{}, func(error) {}), agent, nil)
	r.SetClock(func() time.Time { return now })

	_ = r.PumpAgent(strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` +
			`{"name":"create_refund","arguments":{"payment_id":"pay_SYN0001","amount":20000}}}` + "\n"))

	if child.Len() == 0 {
		t.Fatal("test precondition: the child accepted no bytes")
	}
	if got := g.State("rfa_001"); got == lifecycle.Available {
		t.Fatalf("state = AVAILABLE after %d bytes reached the child. The refund may "+
			"have been dispatched and the authorization can be spent a second time.",
			child.Len())
	}

	// The consequence, asserted rather than reasoned about: a second identical
	// refund must not go through. "Not AVAILABLE" is the mechanism; "the replay
	// is refused" is what it buys, and only the second one is the property
	// anyone cares about.
	before := child.Len()
	_ = r.PumpAgent(strings.NewReader(
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":` +
			`{"name":"create_refund","arguments":{"payment_id":"pay_SYN0001","amount":20000}}}` + "\n"))
	if child.Len() != before {
		t.Fatalf("a replay reached the child after the tee failed: %d new bytes.\n%s",
			child.Len()-before, child.String()[before:])
	}
}
