package relay

import (
	"strings"
	"sync"
	"testing"

	"github.com/harshith/rzp-guard/internal/lifecycle"
)

// An action entering IN_DOUBT is money in an unknown state: the refund may have
// reached Razorpay, the budget stays encumbered, and nothing moves until an
// operator decides. Before the alerter existed that transition was SILENT
// mid-session -- discoverable only by someone choosing to run `operator list`.
//
// These tests pin that every route to IN_DOUBT notifies, and that the routes
// that are NOT ambiguous stay quiet. An alert on every refund would be noise,
// and noise is how a real alert gets ignored.

type alertRecorder struct {
	mu   sync.Mutex
	got  []string
	ids  []string
	reas []string
}

func (a *alertRecorder) fn(actionID, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.got = append(a.got, actionID+": "+reason)
	a.ids = append(a.ids, actionID)
	a.reas = append(a.reas, reason)
}

func (a *alertRecorder) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.got)
}

const oneAction = `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":50000}]`

const authorizedCall = `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":` +
	`{"name":"create_refund","arguments":{"payment_id":"pay_SYN0001","amount":50000}}}`

func armed(t *testing.T) (*Relay, *alertRecorder) {
	t.Helper()
	g := newGuard(t, oneAction)
	r, _, _ := newRelay(t, g)
	rec := &alertRecorder{}
	r.SetAlerter(rec.fn)
	return r, rec
}

// Every ambiguous outcome must produce exactly one alert naming the action.
func TestEveryRouteToInDoubtAlerts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reply     string
		wantInMsg string
	}{
		{
			name:      "child returns a JSON-RPC error",
			reply:     `{"jsonrpc":"2.0","id":7,"error":{"code":-32000,"message":"upstream"}}`,
			wantInMsg: "error",
		},
		{
			// A response envelope with NO result field at all. isResponse()
			// deliberately accepts it -- a message with an id and no method is a
			// response even when malformed, and it must still settle the
			// reservation rather than leave it RESERVED until session end.
			name:      "child returns a response with no result",
			reply:     `{"jsonrpc":"2.0","id":7}`,
			wantInMsg: "empty result",
		},
		{
			name: "reply carries no matching refund entity",
			reply: `{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text",` +
				`"text":"{\"entity\":\"refund\",\"id\":\"rfnd_x\",\"payment_id\":\"pay_OTHER\",` +
				`\"amount\":50000,\"receipt\":\"nope\"}"}]}}`,
			wantInMsg: "refund entity",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, rec := armed(t)
			feed(t, r, authorizedCall)
			if err := r.PumpChild(strings.NewReader(tc.reply + "\n")); err != nil {
				t.Fatal(err)
			}

			if rec.count() != 1 {
				t.Fatalf("got %d alerts, want exactly 1: %v", rec.count(), rec.got)
			}
			if rec.ids[0] != "rfa_001" {
				t.Fatalf("alert names action %q, want rfa_001", rec.ids[0])
			}
			if !strings.Contains(rec.reas[0], tc.wantInMsg) {
				t.Fatalf("reason %q does not explain which path produced it "+
					"(want it to mention %q); that is the operator's first "+
					"question and it changes what they check", rec.reas[0], tc.wantInMsg)
			}
			if r.guard.State("rfa_001") != lifecycle.InDoubt {
				t.Fatalf("state is %s, want IN_DOUBT", r.guard.State("rfa_001"))
			}
		})
	}
}

// A session ending with a call still outstanding is the crash-adjacent case.
func TestCloseInflightAlerts(t *testing.T) {
	r, rec := armed(t)
	feed(t, r, authorizedCall)

	stranded := r.CloseInflight()
	if len(stranded) != 1 || stranded[0] != "rfa_001" {
		t.Fatalf("stranded = %v, want [rfa_001]", stranded)
	}
	if rec.count() != 1 {
		t.Fatalf("got %d alerts for a stranded refund, want 1: %v", rec.count(), rec.got)
	}
	if !strings.Contains(rec.reas[0], "flight") {
		t.Fatalf("reason %q does not say the call was still in flight", rec.reas[0])
	}
}

// The quiet paths. An alert that fires on healthy traffic trains an operator to
// ignore it, which is worse than having none.
func TestNoAlertOnNormalOutcomes(t *testing.T) {
	t.Run("a committed refund", func(t *testing.T) {
		r, rec := armed(t)
		feed(t, r, authorizedCall)
		receipt := r.guard.Decide // referenced to keep the guard in scope
		_ = receipt

		// Build a reply that matches payment, amount and the injected receipt.
		rcpt := inflightReceipt(t, r, "7")
		reply := `{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":` +
			`"{\"entity\":\"refund\",\"id\":\"rfnd_ok\",\"payment_id\":\"pay_SYN0001\",` +
			`\"amount\":50000,\"receipt\":\"` + rcpt + `\"}"}]}}`
		if err := r.PumpChild(strings.NewReader(reply + "\n")); err != nil {
			t.Fatal(err)
		}
		if r.guard.State("rfa_001") != lifecycle.Committed {
			t.Fatalf("state is %s, want COMMITTED", r.guard.State("rfa_001"))
		}
		if rec.count() != 0 {
			t.Fatalf("a successful refund raised %d alert(s): %v", rec.count(), rec.got)
		}
	})

	t.Run("a blocked refund", func(t *testing.T) {
		r, rec := armed(t)
		feed(t, r, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":`+
			`{"name":"create_refund","arguments":{"payment_id":"pay_NOPE","amount":100}}}`)
		if rec.count() != 0 {
			t.Fatalf("a blocked call raised %d alert(s); a block is the guard "+
				"working, not money at risk: %v", rec.count(), rec.got)
		}
	})

	t.Run("a permitted read", func(t *testing.T) {
		r, rec := armed(t)
		feed(t, r, `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":`+
			`{"name":"fetch_payment","arguments":{"payment_id":"pay_SYN0001"}}}`)
		if err := r.PumpChild(strings.NewReader(
			`{"jsonrpc":"2.0","id":11,"result":{"content":[]}}` + "\n")); err != nil {
			t.Fatal(err)
		}
		if rec.count() != 0 {
			t.Fatalf("a read raised %d alert(s): %v", rec.count(), rec.got)
		}
	})
}

// A nil alerter must not panic. Losing the notification is bad; taking the
// guard down with it is worse.
func TestNilAlerterIsIgnored(t *testing.T) {
	g := newGuard(t, oneAction)
	r, _, _ := newRelay(t, g)
	r.SetAlerter(nil)

	feed(t, r, authorizedCall)
	if err := r.PumpChild(strings.NewReader(
		`{"jsonrpc":"2.0","id":7,"result":{}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if r.guard.State("rfa_001") != lifecycle.InDoubt {
		t.Fatal("the transition must still happen without an alerter")
	}
}

// inflightReceipt reads the receipt the guard injected for a tracked request.
func inflightReceipt(t *testing.T, r *Relay, id string) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.inflight[id]
	if !ok {
		t.Fatalf("no in-flight entry for id %s", id)
	}
	return p.receipt
}
