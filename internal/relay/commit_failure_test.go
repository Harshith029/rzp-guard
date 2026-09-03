package relay

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/harshith/rzp-guard/internal/lifecycle"
	"github.com/harshith/rzp-guard/internal/mandate"
	"github.com/harshith/rzp-guard/internal/policy"
)

// commitFailingStore persists everything except the move to COMMITTED, standing
// in for a disk that filled up between the reservation and the reply.
type commitFailingStore struct{ failed bool }

func (s *commitFailingStore) Reserve(actionID, receipt string, amountPaise int64) error { return nil }
func (s *commitFailingStore) ReserveMany(receipt string, rs []lifecycle.Reservation) error {
	return nil
}
func (s *commitFailingStore) SetState(actionID, from, to string) error {
	return s.SetStateMany([]string{actionID}, from, to)
}
func (s *commitFailingStore) SetStateMany(actionIDs []string, from, to string) error {
	if to == string(lifecycle.Committed) {
		s.failed = true
		return errors.New("no space left on device")
	}
	return nil
}

func guardWithStore(t *testing.T, actions string, st lifecycle.Persister) *policy.Guard {
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
	return policy.NewWithStore(m, st)
}

// A FAILED COMMIT MUST NOT BE SILENT.
//
// The refund landed -- the reply carried a matching refund entity -- but the
// durable write recording that fact failed. The ledger keeps the action
// RESERVED, so its budget stays encumbered against a refund that already
// happened, and the money cannot be re-authorized or reconciled.
//
// Before this test, resolve() discarded the error from CommitMany. Nothing
// alerted, and the only thing that would ever notice was RecoverStartup
// promoting RESERVED to IN_DOUBT on the next restart -- which could be days, or
// never on a long-running guard.
func TestFailedCommitAlertsInsteadOfStrandingTheAction(t *testing.T) {
	st := &commitFailingStore{}
	g := guardWithStore(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":20000}]`, st)
	r, _, _ := newRelay(t, g)
	rec := &alertRecorder{}
	r.SetAlerter(rec.fn)

	receipt, _ := mandate.ReceiptFor("mnd_test", "rfa_001")
	feed(t, r, `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":`+
		`{"name":"create_refund","arguments":{"payment_id":"pay_SYN0001","amount":20000}}}`)

	// The provider confirms the refund: matching payment, amount and receipt.
	if err := r.PumpChild(strings.NewReader(
		refundReply("11", "pay_SYN0001", 20000, receipt) + "\n")); err != nil {
		t.Fatal(err)
	}

	if !st.failed {
		t.Fatal("test precondition: the commit was never attempted")
	}
	if len(rec.ids) == 0 {
		t.Fatalf("a failed commit was silent. The refund landed, the action is still "+
			"RESERVED, its budget is encumbered, and nothing told an operator. "+
			"state = %s", g.State("rfa_001"))
	}
	if got := g.State("rfa_001"); got == lifecycle.Committed {
		t.Fatalf("state = COMMITTED although the durable write failed: memory and "+
			"the database now disagree about %s", "rfa_001")
	}
}
