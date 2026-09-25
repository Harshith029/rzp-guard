package storage

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/harshith/rzp-guard/internal/opauth"
	"github.com/harshith/rzp-guard/internal/opgrant"
)

// operatorGrant mints a real opauth.Grant the only way one can be minted:
// through Authenticate against a verifier. There is deliberately no test
// shortcut around it -- the whole guarantee is that this argument cannot be
// constructed without the credential.
func operatorGrant(t *testing.T, subject string) opauth.Grant {
	t.Helper()
	token, err := opauth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := opauth.Verifier(token)
	if err != nil {
		t.Fatal(err)
	}
	g, err := opauth.Authenticate(subject, token, verifier)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func openQueue(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "queue.db"), "mnd_queue")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// An agent that retries a refused call in a loop is the normal case, not the
// exception. One row per attempt would make the queue as unreadable as the
// stderr stream it replaces, and a queue nobody reads is the same as no queue.
func TestARetriedRefusalIsOneQueueEntry(t *testing.T) {
	s := openQueue(t)
	for i := 0; i < 12; i++ {
		if err := s.RecordDenial("create_refund", "NO_AUTHORIZED_ACTION",
			"pay_SYN0001", 7500, "no authorized refund action exists"); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.Denials(DenialOpen, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("12 retries produced %d queue entries, want 1", len(rows))
	}
	if rows[0].Occurrences != 12 {
		t.Errorf("occurrences = %d, want 12: the retry count is how an operator "+
			"tells a stuck agent from a one-off", rows[0].Occurrences)
	}
	if rows[0].FirstAt == "" || rows[0].LastAt == "" {
		t.Error("the entry does not bracket when the retries happened")
	}
}

// Two different refusals are two entries. Collapsing them would hide one of two
// customers waiting.
func TestDistinctRefusalsAreDistinctEntries(t *testing.T) {
	s := openQueue(t)
	for _, d := range []struct {
		rule, payment string
		amount        int64
	}{
		{"NO_AUTHORIZED_ACTION", "pay_SYN0001", 7500},
		{"NO_AUTHORIZED_ACTION", "pay_SYN0002", 7500},
		{"AMOUNT_NOT_AUTHORIZED", "pay_SYN0001", 7500},
		{"NO_AUTHORIZED_ACTION", "pay_SYN0001", 9000},
	} {
		if err := s.RecordDenial("create_refund", d.rule, d.payment, d.amount, "x"); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := s.Denials(DenialOpen, false)
	if len(rows) != 4 {
		t.Fatalf("four distinct refusals produced %d entries, want 4", len(rows))
	}
}

// THE CENTRAL AUTHORIZATION PROPERTY. A grant cannot be minted without a
// verified operator credential. If this ever passes with an empty Grant, the
// guard itself could widen its own mandate.
func TestAGrantCannotBeIssuedWithoutAVerifiedOperator(t *testing.T) {
	s := openQueue(t)
	if err := s.RecordDenial("create_refund", "NO_AUTHORIZED_ACTION",
		"pay_SYN0001", 7500, "x"); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.Denials(DenialOpen, false)

	if _, err := s.IssueGrant(opauth.Grant{}, rows[0].ID, time.Minute, "because"); err == nil {
		t.Fatal("a grant was issued from a zero-value opauth.Grant; the operator " +
			"credential is not actually gating this")
	}
}

// A grant takes its payment and amount FROM THE RECORDED REFUSAL. That is what
// stops this being a general "authorize any refund" command: a stolen operator
// token cannot mint authority for a payment no agent ever asked about.
func TestAGrantMirrorsExactlyWhatWasRefused(t *testing.T) {
	s := openQueue(t)
	if err := s.RecordDenial("create_refund", "AMOUNT_NOT_AUTHORIZED",
		"pay_SYN0007", 18500, "18500 paise is not authorized"); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.Denials(DenialOpen, false)
	op := operatorGrant(t, "ops@merchant.example")

	g, err := s.IssueGrant(op, rows[0].ID, 20*time.Minute, "customer produced the receipt")
	if err != nil {
		t.Fatal(err)
	}
	if g.PaymentID != "pay_SYN0007" || g.AmountPaise != 18500 {
		t.Fatalf("grant is for %d paise on %s; the refusal was %d on %s",
			g.AmountPaise, g.PaymentID, 18500, "pay_SYN0007")
	}
	if !opgrant.IDPattern.MatchString(g.GrantID) {
		t.Errorf("grant id %q does not match the action-id shape it has to satisfy; "+
			"it becomes a ledger row and a provider-side receipt", g.GrantID)
	}
	if g.Actor != "ops@merchant.example" {
		t.Errorf("actor = %q, want the authenticated operator", g.Actor)
	}

	// The denial is resolved and the decision is audited, in the same
	// transaction. An issued grant with no audit row is authority nobody can
	// attribute.
	open, _ := s.Denials(DenialOpen, false)
	if len(open) != 0 {
		t.Errorf("%d refusals still OPEN after approval", len(open))
	}
	trail, err := s.AuditTrail()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, a := range trail {
		if a.To == "OPERATOR_GRANTED" && a.ActionID == g.GrantID {
			found = true
			if !strings.Contains(a.Reason, "customer produced the receipt") {
				t.Errorf("the audit row does not carry the operator's reason: %q", a.Reason)
			}
		}
	}
	if !found {
		t.Error("no audit row for an issued grant")
	}
}

// A grant that outlives the incident is standing authority nobody revisits, and
// it is invisible in the mandate a reviewer reads.
func TestAGrantCannotOutliveTheCeiling(t *testing.T) {
	s := openQueue(t)
	if err := s.RecordDenial("create_refund", "NO_AUTHORIZED_ACTION",
		"pay_SYN0001", 7500, "x"); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.Denials(DenialOpen, false)
	op := operatorGrant(t, "ops@merchant.example")

	if _, err := s.IssueGrant(op, rows[0].ID, opgrant.MaxTTL+time.Minute, "because"); err == nil {
		t.Fatalf("a grant longer than the %s ceiling was issued", opgrant.MaxTTL)
	}
}

// An agent cannot reopen a decision by asking again. A refusal a human has
// DECLINED stays declined however many times it is retried, which is exactly
// what an agent under prompt injection would do.
func TestARetryCannotReopenADeclinedRefusal(t *testing.T) {
	s := openQueue(t)
	if err := s.RecordDenial("create_refund", "NO_AUTHORIZED_ACTION",
		"pay_SYN0001", 7500, "x"); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.Denials(DenialOpen, false)
	op := operatorGrant(t, "ops@merchant.example")

	if err := s.DeclineDenial(op, rows[0].ID, "the customer already had this refunded"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := s.RecordDenial("create_refund", "NO_AUTHORIZED_ACTION",
			"pay_SYN0001", 7500, "x"); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := s.Denials("", false)
	if all[0].Resolution != DenialDeclined {
		t.Fatalf("resolution = %s after five retries, want DECLINED: an agent "+
			"reopened a decision a human made", all[0].Resolution)
	}
	if _, err := s.IssueGrant(op, rows[0].ID, time.Minute, "changed my mind"); err == nil {
		t.Fatal("a declined refusal was approved, erasing the earlier decision " +
			"without a record")
	}
}

// Expiry is enforced in SQL rather than left to the caller, so a caller cannot
// forget. Expired rows are kept, not deleted: they are the record of a decision
// somebody made.
func TestLiveGrantsExcludesExpiredOnesButKeepsTheRecord(t *testing.T) {
	s := openQueue(t)
	if err := s.RecordDenial("create_refund", "NO_AUTHORIZED_ACTION",
		"pay_SYN0001", 7500, "x"); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.Denials(DenialOpen, false)
	op := operatorGrant(t, "ops@merchant.example")
	g, err := s.IssueGrant(op, rows[0].ID, time.Minute, "because")
	if err != nil {
		t.Fatal(err)
	}

	live, err := s.LiveGrants("mnd_queue", g.IssuedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Fatalf("live grants = %d, want 1", len(live))
	}
	later, err := s.LiveGrants("mnd_queue", g.ExpiresAt.Add(time.Nanosecond))
	if err != nil {
		t.Fatal(err)
	}
	if len(later) != 0 {
		t.Fatalf("an expired grant is still live: %+v", later)
	}
	trail, _ := s.AuditTrail()
	if len(trail) == 0 {
		t.Error("the record of the decision vanished with the grant")
	}
}

// A grant lands in ONE mandate's ledger. Approving another mandate's refusal
// under this one would put authority in the wrong place, and a shared state
// file makes that a realistic mistake rather than an exotic one.
func TestAGrantCannotCrossMandates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	a, err := Open(path, "mnd_A")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path, "mnd_B")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if err := a.RecordDenial("create_refund", "NO_AUTHORIZED_ACTION",
		"pay_SYN0001", 7500, "x"); err != nil {
		t.Fatal(err)
	}
	rows, _ := a.Denials(DenialOpen, false)
	op := operatorGrant(t, "ops@merchant.example")

	if _, err := b.IssueGrant(op, rows[0].ID, time.Minute, "because"); err == nil {
		t.Fatal("mandate B issued a grant against mandate A's refusal")
	}
	// And the cross-mandate view still shows it, which is why sharing a file is
	// useful in the first place.
	crossed, err := b.Denials(DenialOpen, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(crossed) != 1 {
		t.Fatalf("the cross-mandate queue shows %d refusals, want 1", len(crossed))
	}
}

func TestApprovingAnUnknownDenialIsNamed(t *testing.T) {
	s := openQueue(t)
	op := operatorGrant(t, "ops@merchant.example")
	_, err := s.IssueGrant(op, 4711, time.Minute, "because")
	if !errors.Is(err, ErrNoSuchDenial) {
		t.Fatalf("got %v, want ErrNoSuchDenial", err)
	}
}

// A grant against a refusal no grant can override is refused at the door, by
// the refusal's rule -- not by whatever fields it happens to carry.
//
// THIS USED TO HOLD BY ACCIDENT. The non-overridable refusal sites happened not
// to record a payment and a positive amount, so opgrant.Validate rejected the
// grant for an unrelated reason. Every case below records BOTH, which is the
// condition that made the accident stop protecting anything: FAILURES.md F53
// shows a refusal shaped like this approved, left live, and spent later on a
// replay as a second refund.
func TestAGrantCannotBeIssuedAgainstARefusalNoGrantCanOverride(t *testing.T) {
	for _, rule := range []string{
		"RATE_LIMIT_EXCEEDED",
		"CUMULATIVE_CAP_EXCEEDED",
		"MANDATE_EXPIRED",
		"MALFORMED_ARGUMENTS",
		"TOOL_NOT_SUPPORTED",
		"TOOL_NOT_ALLOWED",
		// A rule that does not exist yet. The list is an allowlist, so a refusal
		// the policy grows later is not grantable until someone decides it is.
		"SOME_FUTURE_RULE",
	} {
		t.Run(rule, func(t *testing.T) {
			s := openQueue(t)
			if err := s.RecordDenial("create_refund", rule, "pay_SYN0009", 24000,
				"refused"); err != nil {
				t.Fatal(err)
			}
			rows, _ := s.Denials(DenialOpen, false)
			_, err := s.IssueGrant(operatorGrant(t, "ops@merchant.example"),
				rows[0].ID, 10*time.Minute, "looked like a false positive")
			if err == nil {
				t.Fatalf("a grant was issued against %s; the guard will not let it "+
					"override that refusal, so it can only sit live until a later "+
					"refusal of a different kind spends it", rule)
			}
			if !strings.Contains(err.Error(), rule) {
				t.Errorf("the refusal does not name the rule, so the operator cannot "+
					"tell why: %v", err)
			}
			// Nothing may have changed: still OPEN, no grant, no audit row
			// claiming a refund was unblocked.
			if open, _ := s.Denials(DenialOpen, false); len(open) != 1 {
				t.Errorf("the denial left the queue (%d open) although nothing was "+
					"approved", len(open))
			}
			trail, _ := s.AuditTrail()
			for _, a := range trail {
				if a.To == "OPERATOR_GRANTED" {
					t.Errorf("an OPERATOR_GRANTED audit row exists for a grant that " +
						"was refused")
				}
			}
		})
	}
}
