package relay

import (
	"bytes"
	"encoding/json"
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

// childRecorder captures every byte the relay writes toward the child, so a
// block can be proven at the boundary rather than inferred from a log line.
type childRecorder struct{ buf bytes.Buffer }

func (c *childRecorder) Write(p []byte) (int, error) { return c.buf.Write(p) }
func (c *childRecorder) String() string              { return c.buf.String() }
func (c *childRecorder) Lines() []string {
	s := strings.TrimSpace(c.buf.String())
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func newGuard(t *testing.T, actions string) *policy.Guard {
	t.Helper()
	doc := `{"mandate_id":"mnd_test","expires_at":"` +
		now.Add(4*time.Hour).Format(time.RFC3339) + `",
		"allowed_tools":["fetch_payment","fetch_all_payments","create_refund"],
		"authorized_refund_actions":` + actions + `,
		"global":{"max_cumulative_paise":500000,"max_calls_per_minute":10}}`
	m, err := mandate.Load([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return policy.New(m)
}

func newRelay(t *testing.T, g *policy.Guard) (*Relay, *childRecorder, *bytes.Buffer) {
	t.Helper()
	child := &childRecorder{}
	agent := &bytes.Buffer{}
	r := New(g, child, agent, nil)
	r.SetClock(func() time.Time { return now })
	return r, child, agent
}

func feed(t *testing.T, r *Relay, lines ...string) {
	t.Helper()
	if err := r.PumpAgent(strings.NewReader(strings.Join(lines, "\n") + "\n")); err != nil {
		t.Fatal(err)
	}
}

// THE CENTRAL PROOF. An unauthorized create_refund must never reach the child.
// A call whose bytes never entered the child process cannot have produced an
// HTTP request to Razorpay.
func TestUnauthorizedRefundNeverReachesTheChild(t *testing.T) {
	g := newGuard(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":50000}]`)
	r, child, agent := newRelay(t, g)

	feed(t, r, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":`+
		`{"name":"create_refund","arguments":{"payment_id":"pay_SYN9999","amount":90000}}}`)

	if got := child.String(); got != "" {
		t.Fatalf("child received %d bytes for a blocked call:\n%s", len(got), got)
	}
	out := agent.String()
	if !strings.Contains(out, "BLOCKED by rzp-guard") ||
		!strings.Contains(out, policy.NoAuthorizedAction) {
		t.Fatalf("agent did not receive a readable block: %s", out)
	}
	if !strings.Contains(out, `"isError":true`) {
		t.Fatalf("block was not surfaced as a tool error: %s", out)
	}
}

func TestDangerousToolInMandateNeverReachesTheChild(t *testing.T) {
	doc := `{"mandate_id":"mnd_test","expires_at":"` +
		now.Add(time.Hour).Format(time.RFC3339) + `",
		"allowed_tools":["initiate_payment","create_instant_settlement"],
		"authorized_refund_actions":[],
		"global":{"max_cumulative_paise":900000,"max_calls_per_minute":10}}`
	m, err := mandate.Load([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	r, child, agent := newRelay(t, policy.New(m))

	// Deliberately DATA-FREE. The allowlist is immutable regardless of
	// arguments, so proving it needs no realistic money-movement payload -- and
	// a public hiring repo under an automatic-disqualification rule should not
	// carry one unnecessarily.
	feed(t, r,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+
			`{"name":"initiate_payment","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":`+
			`{"name":"create_instant_settlement","arguments":{}}}`)

	if got := child.String(); got != "" {
		t.Fatalf("child received bytes for tools outside the build surface:\n%s", got)
	}
	if !strings.Contains(agent.String(), policy.ToolNotSupported) {
		t.Fatalf("expected TOOL_NOT_SUPPORTED: %s", agent.String())
	}
}

// Non-tools/call traffic must pass through untouched, or the relay is not
// transparent and would break the MCP handshake.
func TestNonToolCallTrafficIsForwardedByteForByte(t *testing.T) {
	g := newGuard(t, `[]`)
	r, child, _ := newRelay(t, g)

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
		`{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`
	notify := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	toolsList := `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`

	feed(t, r, initialize, notify, toolsList)

	got := child.Lines()
	want := []string{initialize, notify, toolsList}
	if len(got) != len(want) {
		t.Fatalf("child got %d lines, want %d:\n%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d not byte-identical:\n got: %s\nwant: %s", i, got[i], want[i])
		}
	}
}

// An approved refund reaches the child with the canonical integer amount and
// the guard's receipt, not whatever the agent sent.
func TestApprovedRefundIsForwardedWithCanonicalAmountAndReceipt(t *testing.T) {
	g := newGuard(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":50000}]`)
	r, child, _ := newRelay(t, g)

	feed(t, r, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":`+
		`{"name":"create_refund","arguments":{"payment_id":"pay_SYN0001","amount":50000,`+
		`"receipt":"attacker-chosen","speed":"normal"}}}`)

	lines := child.Lines()
	if len(lines) != 1 {
		t.Fatalf("child got %d lines, want 1: %v", len(lines), lines)
	}
	var msg struct {
		ID     json.Number `json:"id"`
		Method string      `json:"method"`
		Params struct {
			Name      string `json:"name"`
			Arguments struct {
				PaymentID string      `json:"payment_id"`
				Amount    json.Number `json:"amount"`
				Receipt   string      `json:"receipt"`
				Speed     string      `json:"speed"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &msg); err != nil {
		t.Fatalf("child line is not valid JSON-RPC: %v\n%s", err, lines[0])
	}
	if msg.ID.String() != "9" || msg.Method != "tools/call" {
		t.Fatalf("envelope mangled: id=%s method=%s", msg.ID, msg.Method)
	}
	if s := msg.Params.Arguments.Amount.String(); s != "50000" {
		t.Fatalf("forwarded amount = %s, want exactly 50000 (no float, no rounding)", s)
	}
	if strings.Contains(s(msg.Params.Arguments.Receipt), "attacker") {
		t.Fatalf("agent-supplied receipt survived: %s", msg.Params.Arguments.Receipt)
	}
	want, _ := mandate.ReceiptFor("mnd_test", "rfa_001")
	if msg.Params.Arguments.Receipt != want {
		t.Fatalf("receipt = %q, want %q", msg.Params.Arguments.Receipt, want)
	}
	if msg.Params.Arguments.Speed != "normal" {
		t.Fatalf("non-authorizing field was altered: speed=%q", msg.Params.Arguments.Speed)
	}
}

func s(v string) string { return v }

// A fractional amount must be blocked at the relay: the runtime schema declares
// amount as {"type":"number"}, so the child would accept it.
func TestFractionalAmountNeverReachesTheChild(t *testing.T) {
	g := newGuard(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":50000}]`)
	r, child, agent := newRelay(t, g)

	feed(t, r, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":`+
		`{"name":"create_refund","arguments":{"payment_id":"pay_SYN0001","amount":50000.9}}}`)

	if got := child.String(); got != "" {
		t.Fatalf("fractional amount was forwarded:\n%s", got)
	}
	if !strings.Contains(agent.String(), policy.MalformedArguments) {
		t.Fatalf("expected MALFORMED_ARGUMENTS: %s", agent.String())
	}
}

// A successful child reply commits; a tool error releases the authorization.
func TestChildReplyResolvesTheReservation(t *testing.T) {
	g := newGuard(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":20000}]`)
	r, _, _ := newRelay(t, g)

	feed(t, r, `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":`+
		`{"name":"create_refund","arguments":{"payment_id":"pay_SYN0001","amount":20000}}}`)
	if g.State("rfa_001") != lifecycle.Reserved {
		t.Fatalf("state = %s, want RESERVED", g.State("rfa_001"))
	}

	reply := `{"jsonrpc":"2.0","id":11,"result":{"content":[{"type":"text",` +
		`"text":"{\"id\":\"rfnd_X\",\"entity\":\"refund\"}"}]}}`
	if err := r.PumpChild(strings.NewReader(reply + "\n")); err != nil {
		t.Fatal(err)
	}
	if g.State("rfa_001") != lifecycle.Committed {
		t.Fatalf("state = %s, want COMMITTED", g.State("rfa_001"))
	}
}

// ONCE BYTES HAVE LEFT, NOTHING AUTO-RELEASES.
//
// A previous revision released the authorization on any JSON-RPC error or any
// isError result, assuming an error proves the request was rejected before
// execution. That was never verified and is not safe: the child can fail after
// dispatching the HTTP request, or while formatting a response to a refund
// Razorpay actually processed. Both shapes now hold the action AND the budget.
func TestErrorRepliesHoldTheActionAndBudgetInDoubt(t *testing.T) {
	cases := []struct {
		name  string
		reply string
	}{
		{
			"mcp tool error",
			`{"jsonrpc":"2.0","id":12,"result":{"content":[{"type":"text",` +
				`"text":"creating refund failed"}],"isError":true}}`,
		},
		{
			"jsonrpc error",
			`{"jsonrpc":"2.0","id":12,"error":{"code":-32603,"message":"internal error"}}`,
		},
		{
			"empty result",
			`{"jsonrpc":"2.0","id":12}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGuard(t,
				`[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":20000}]`)
			r, _, _ := newRelay(t, g)

			feed(t, r, `{"jsonrpc":"2.0","id":12,"method":"tools/call","params":`+
				`{"name":"create_refund","arguments":{"payment_id":"pay_SYN0001","amount":20000}}}`)
			if err := r.PumpChild(strings.NewReader(tc.reply + "\n")); err != nil {
				t.Fatal(err)
			}

			if got := g.State("rfa_001"); got != lifecycle.InDoubt {
				t.Fatalf("state = %s, want IN_DOUBT: this reply shape cannot prove "+
					"Razorpay did not process the refund", got)
			}
			if enc := g.Encumbered(); enc != 20000 {
				t.Fatalf("encumbered = %d, want 20000: budget must stay held", enc)
			}
			if retry := g.Decide(policy.RefundTool, map[string]any{
				"payment_id": "pay_SYN0001", "amount": int64(20000)}, now); retry.Allowed {
				t.Fatal("the action was reusable after an ambiguous reply")
			}
		})
	}
}

// The hardest case: the call was forwarded and no reply ever arrived.
func TestSeveredReplyLeavesTheActionInDoubtWithBudgetHeld(t *testing.T) {
	db := filepath.Join(t.TempDir(), "guard.db")
	doc := `{"mandate_id":"mnd_test","expires_at":"` +
		now.Add(time.Hour).Format(time.RFC3339) + `",
		"allowed_tools":["create_refund"],
		"authorized_refund_actions":[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":60000}],
		"global":{"max_cumulative_paise":100000,"max_calls_per_minute":10}}`
	m, err := mandate.Load([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	st, err := storage.Open(db, m.MandateID)
	if err != nil {
		t.Fatal(err)
	}
	g := policy.NewWithStore(m, st)
	r, child, _ := newRelay(t, g)

	feed(t, r, `{"jsonrpc":"2.0","id":13,"method":"tools/call","params":`+
		`{"name":"create_refund","arguments":{"payment_id":"pay_SYN0001","amount":60000}}}`)
	if len(child.Lines()) != 1 {
		t.Fatal("approved refund was not forwarded")
	}

	// Child dies before replying.
	pending := r.CloseInflight()
	if len(pending) != 1 || pending[0] != "rfa_001" {
		t.Fatalf("CloseInflight returned %v, want [rfa_001]", pending)
	}
	if g.State("rfa_001") != lifecycle.InDoubt {
		t.Fatalf("state = %s, want IN_DOUBT", g.State("rfa_001"))
	}
	if enc := g.Encumbered(); enc != 60000 {
		t.Fatalf("encumbered = %d, want 60000: budget must stay held", enc)
	}
	st.Close()

	// Restart: the durable row is still IN_DOUBT and still locked.
	st2, err := storage.Open(db, m.MandateID)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if _, err := st2.RecoverStartup(); err != nil {
		t.Fatal(err)
	}
	snap, err := st2.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	g2 := policy.NewWithStore(m, st2)
	g2.Restore(snap.States, snap.Amounts)
	if g2.State("rfa_001") != lifecycle.InDoubt {
		t.Fatalf("after restart state = %s, want IN_DOUBT", g2.State("rfa_001"))
	}
	if g2.Encumbered() != 60000 {
		t.Fatalf("after restart encumbered = %d, want 60000", g2.Encumbered())
	}
}

// Malformed input must not be passed through to the child unexamined.
func TestUnparseableInputIsNotForwarded(t *testing.T) {
	g := newGuard(t, `[]`)
	r, child, agent := newRelay(t, g)
	feed(t, r, `{"jsonrpc":"2.0","id":1,"method":"tools/call"`)
	if got := child.String(); got != "" {
		t.Fatalf("unparseable bytes were forwarded to the child: %s", got)
	}
	if !strings.Contains(agent.String(), "could not parse") {
		t.Fatalf("agent got no parse error: %s", agent.String())
	}
}
