package bootstrap

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/harshith/rzp-guard/internal/opauth"
	"github.com/harshith/rzp-guard/internal/policy"
	"github.com/harshith/rzp-guard/internal/storage"
)

// THE FALSE-POSITIVE LOOP, END TO END.
//
// study/FP-COST.md prices every wrongly-refused refund on the assumption that a
// human unblocks it, and its own section 7 says nothing implements that. This
// is the test that says it does now, and it deliberately runs the WHOLE path
// rather than any one half:
//
//	a real guard refuses a legitimate refund
//	the refusal lands in a durable queue
//	an operator attaches WHILE THE GUARD IS STILL RUNNING
//	they approve it with a verified credential
//	the same guard forwards the retry, without a restart
//
// The fourth line is the one that used to be impossible. The guard held a
// file-wide exclusive lock for its entire lifetime, so the operator tool could
// not open the state file at all, and the only way to change what the guard
// would allow was to stop it. Nobody stops a payment proxy to unstick one
// refund, so in practice nobody ever unblocked anything.

func operatorCredential(t *testing.T, store *storage.Store) opauth.Grant {
	t.Helper()
	token, err := opauth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := opauth.Verifier(token)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InitOperatorVerifier(verifier); err != nil {
		t.Fatal(err)
	}
	stored, configured, _, err := store.OperatorVerifier()
	if err != nil || !configured {
		t.Fatalf("verifier not configured: %v", err)
	}
	// Through Authenticate, the only way a Grant can be made. A test shortcut
	// here would test a door that does not exist.
	g, err := opauth.Authenticate("ops@merchant.example", token, stored)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestAWronglyRefusedRefundIsUnblockedWithoutRestartingTheGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	// The cap has headroom on purpose. A grant is reserved through the same
	// ledger as a mandate action, so it is bounded by the merchant's own
	// cumulative cap -- a separate test covers that refusal, and this one is
	// about the workflow rather than the ceiling.
	m := buildMandate(t, "mnd_unblock", 1, 50000)
	now := time.Now().UTC()

	boot, err := Open(path, m, now)
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Close()
	boot.Guard.SetGrantSource(boot.Store)

	// A legitimate refund the mandate does not cover. This is the shape of all
	// thirty measured false positives: the merchant meant it, the mandate was
	// written narrower than the sentence, and the guard cannot see the sentence.
	const wantedPayment = "pay_SYNunblock01"
	const wantedPaise = int64(18500)
	args := map[string]any{"payment_id": wantedPayment, "amount": wantedPaise}

	d := boot.Guard.Decide(policy.RefundTool, args, now)
	if d.Allowed {
		t.Fatal("precondition: the mandate was supposed to refuse this")
	}
	if d.PaymentID != wantedPayment || d.RequestedPaise != wantedPaise {
		t.Fatalf("the refusal does not carry what was refused in structured form "+
			"(%q/%d); the queue would have to parse it back out of English that "+
			"embeds an agent-controlled string", d.PaymentID, d.RequestedPaise)
	}

	// The guard records it. In the binary this happens in the decision sink; here
	// it is called directly so the test covers the storage contract rather than
	// main's wiring.
	if err := boot.Store.RecordDenial(d.Tool, d.Rule, d.PaymentID,
		d.RequestedPaise, d.Reason); err != nil {
		t.Fatalf("the refusal was not recorded for review: %v", err)
	}

	// THE OPERATOR ARRIVES WHILE THE GUARD IS STILL RUNNING.
	op, err := storage.Attach(path, m.MandateID)
	if err != nil {
		t.Fatalf("the operator could not attach to a state file a live guard holds. "+
			"This is the whole workflow: %v", err)
	}
	defer op.Close()

	lease, found, err := op.LeaseFor(m.MandateID)
	if err != nil || !found || !lease.Live {
		t.Fatalf("the operator cannot see that a guard is live (found=%v live=%v err=%v)",
			found, lease.Live, err)
	}

	queue, err := op.Denials(storage.DenialOpen, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 1 {
		t.Fatalf("the queue holds %d refusals, want the one that just happened", len(queue))
	}
	if queue[0].PaymentID != wantedPayment || queue[0].AmountPaise != wantedPaise {
		t.Fatalf("the queue entry describes %s/%d, not what was refused",
			queue[0].PaymentID, queue[0].AmountPaise)
	}

	cred := operatorCredential(t, op)
	grant, err := op.IssueGrant(cred, queue[0].ID, 10*time.Minute,
		"customer produced the order confirmation; the mandate was written short")
	if err != nil {
		t.Fatalf("issuing a grant beside a live guard failed: %v", err)
	}

	// THE SAME GUARD, NO RESTART. The poll interval is a second, so the retry is
	// dated past it -- an agent retrying immediately would simply be refused once
	// more and then succeed, which is the intended behaviour and not worth a
	// second of wall clock in a test.
	retry := now.Add(2 * time.Second)
	d2 := boot.Guard.Decide(policy.RefundTool, args, retry)
	if !d2.Allowed {
		t.Fatalf("the guard still refuses a refund a human explicitly approved: %s", d2.Reason)
	}
	if d2.Rule != policy.OperatorApproved {
		t.Errorf("rule = %s, want %s", d2.Rule, policy.OperatorApproved)
	}
	if d2.OperatorGrantID != grant.GrantID {
		t.Errorf("grant id = %q, want %q", d2.OperatorGrantID, grant.GrantID)
	}
	if amt, ok := d2.ForwardedAmountPaise(); !ok || amt != wantedPaise {
		t.Errorf("forwarded %d, want exactly the approved %d", amt, wantedPaise)
	}

	// And it went through the ordinary ledger: reserved, durable, single use.
	if st := boot.Guard.State(grant.GrantID); st != "RESERVED" {
		t.Errorf("the grant is %s, not RESERVED; it did not go through the same "+
			"lifecycle as a mandate action", st)
	}
	if err := boot.Guard.Commit(grant.GrantID); err != nil {
		t.Fatal(err)
	}
	if d3 := boot.Guard.Decide(policy.RefundTool, args, retry); d3.Allowed {
		t.Fatal("one approval authorized a second refund")
	}
}

// The operator's view has to survive a restart, or a queue is just a longer
// stderr. Everything the workflow records is durable: the refusal, the
// approval, the grant, and the audit row naming who approved it.
func TestTheQueueAndItsDecisionsSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	m := buildMandate(t, "mnd_durable", 1, 1000)
	now := time.Now().UTC()

	boot, err := Open(path, m, now)
	if err != nil {
		t.Fatal(err)
	}
	cred := operatorCredential(t, boot.Store)
	if err := boot.Store.RecordDenial("create_refund", "NO_AUTHORIZED_ACTION",
		"pay_SYNdurable01", 7500, "no authorized refund action exists"); err != nil {
		t.Fatal(err)
	}
	rows, _ := boot.Store.Denials(storage.DenialOpen, false)
	if _, err := boot.Store.IssueGrant(cred, rows[0].ID, 30*time.Minute,
		"agreed with the customer on the phone"); err != nil {
		t.Fatal(err)
	}
	boot.Close()

	again, err := Open(path, m, now)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()

	approved, err := again.Store.Denials(storage.DenialApproved, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(approved) != 1 {
		t.Fatalf("%d approved refusals after restart, want 1", len(approved))
	}
	live, err := again.Store.LiveGrants(m.MandateID, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Fatalf("%d live grants after restart, want 1: an operator's approval did "+
			"not survive the process that recorded it", len(live))
	}
	trail, err := again.Store.AuditTrail()
	if err != nil {
		t.Fatal(err)
	}
	var attributed bool
	for _, a := range trail {
		if a.To == "OPERATOR_GRANTED" && a.Actor == "ops@merchant.example" {
			attributed = true
		}
	}
	if !attributed {
		t.Error("no durable record of WHO approved the refund")
	}
}

// A guard whose mandate has no grants must behave exactly as it did before this
// feature existed. That is what every test predating the unblock workflow
// asserts, and what every deployment that never issues a grant relies on.
func TestAnEmptyQueueChangesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	m := buildMandate(t, "mnd_quiet", 2, 1000)
	now := time.Now().UTC()

	boot, err := Open(path, m, now)
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Close()
	boot.Guard.SetGrantSource(boot.Store)

	// The mandate's own action still passes, unchanged.
	ok := boot.Guard.Decide(policy.RefundTool,
		map[string]any{"payment_id": "pay_SYNmnd_quiet00", "amount": int64(1000)}, now)
	if !ok.Allowed || ok.Rule != policy.Allowed {
		t.Fatalf("an ordinary authorized refund was disturbed: rule=%s %s", ok.Rule, ok.Reason)
	}
	// And an unauthorized one is still refused by the mandate's own rule.
	no := boot.Guard.Decide(policy.RefundTool,
		map[string]any{"payment_id": "pay_SYNnothing", "amount": int64(1000)}, now)
	if no.Allowed || no.Rule != policy.NoAuthorizedAction {
		t.Fatalf("allowed=%v rule=%s", no.Allowed, no.Rule)
	}
}
