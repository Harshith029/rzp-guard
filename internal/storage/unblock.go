package storage

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/harshith/rzp-guard/internal/opauth"
	"github.com/harshith/rzp-guard/internal/opgrant"
)

// Denial is one refused refund, as an operator sees it.
type Denial struct {
	ID          int64
	MandateID   string
	Tool        string
	Rule        string
	PaymentID   string
	AmountPaise int64
	Reason      string
	FirstAt     string
	LastAt      string
	Occurrences int
	Resolution  string
}

// Denial resolutions. OPEN is the default and the only one the guard ever sees;
// the other two are decisions a person made.
const (
	DenialOpen     = "OPEN"
	DenialApproved = "APPROVED"
	DenialDeclined = "DECLINED"
)

// ErrNoSuchDenial means the id an operator quoted is not in the queue.
var ErrNoSuchDenial = errors.New("no such denial")

// RecordDenial appends a refused refund to the queue, or counts a repeat.
//
// DEDUPLICATED on (mandate, rule, payment, amount) because an agent retrying a
// refused call in a loop is the normal case. One row per attempt would turn the
// queue into the same unreadable stream stderr already is, and the queue only
// earns its place by being short enough that somebody reads it.
//
// FAILURE HERE IS NOT FATAL TO THE CALLER, and must not be. The refusal has
// already happened and nothing was forwarded; losing the queue entry costs
// visibility, not safety. A guard that died because it could not write a record
// of something it correctly refused would be trading the money path against the
// reporting path, which is the wrong direction.
func (s *Store) RecordDenial(tool, rule, paymentID string, amountPaise int64, reason string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(
		`INSERT INTO denial (mandate_id, tool, rule, payment_id, amount_paise,
		                     reason, first_at, last_at, occurrences, resolution)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, 'OPEN')
		 ON CONFLICT(mandate_id, rule, payment_id, amount_paise) DO UPDATE SET
		   last_at = excluded.last_at,
		   occurrences = denial.occurrences + 1,
		   reason = excluded.reason,
		   -- A repeat of something a human already DECLINED stays declined. An
		   -- agent cannot reopen a decision by asking again, which is exactly what
		   -- an agent under prompt injection would do.
		   resolution = CASE denial.resolution
		                  WHEN 'DECLINED' THEN 'DECLINED'
		                  WHEN 'APPROVED' THEN 'APPROVED'
		                  ELSE 'OPEN' END`,
		s.mandateID, tool, rule, paymentID, amountPaise, reason, now, now)
	if err != nil {
		return fmt.Errorf("storage: record denial: %w", err)
	}
	return nil
}

// Denials lists the queue. allMandates crosses the scope boundary on purpose:
// one operator on a host should see every merchant's blocked refunds in one
// place, which is the reason a state file may hold several mandates at all.
func (s *Store) Denials(resolution string, allMandates bool) ([]Denial, error) {
	q := `SELECT id, mandate_id, tool, rule, payment_id, amount_paise, reason,
	             first_at, last_at, occurrences, resolution
	        FROM denial WHERE 1=1`
	var args []any
	if !allMandates {
		q += ` AND mandate_id = ?`
		args = append(args, s.mandateID)
	}
	if resolution != "" {
		q += ` AND resolution = ?`
		args = append(args, resolution)
	}
	q += ` ORDER BY last_at DESC, id DESC`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: denials: %w", err)
	}
	defer rows.Close()
	var out []Denial
	for rows.Next() {
		var d Denial
		if err := rows.Scan(&d.ID, &d.MandateID, &d.Tool, &d.Rule, &d.PaymentID,
			&d.AmountPaise, &d.Reason, &d.FirstAt, &d.LastAt, &d.Occurrences,
			&d.Resolution); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) denial(tx *sql.Tx, id int64) (Denial, error) {
	var d Denial
	err := tx.QueryRow(
		`SELECT id, mandate_id, tool, rule, payment_id, amount_paise, reason,
		        first_at, last_at, occurrences, resolution
		   FROM denial WHERE id = ?`, id).
		Scan(&d.ID, &d.MandateID, &d.Tool, &d.Rule, &d.PaymentID, &d.AmountPaise,
			&d.Reason, &d.FirstAt, &d.LastAt, &d.Occurrences, &d.Resolution)
	if errors.Is(err, sql.ErrNoRows) {
		return Denial{}, fmt.Errorf("%w: %d", ErrNoSuchDenial, id)
	}
	if err != nil {
		return Denial{}, fmt.Errorf("storage: read denial %d: %w", id, err)
	}
	return d, nil
}

// newGrantID mints an id in the shape opgrant.IDPattern requires.
func newGrantID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("storage: grant id: %w", err)
	}
	return "opg_" + hex.EncodeToString(b[:]), nil
}

// grantable is the complete set of refusals an operator grant can correct.
//
// IT IS AN ALLOWLIST, so a refusal rule added to the policy later is not
// grantable until somebody decides it should be. That is the safe default: the
// cost of a missing entry is a workflow that refuses, and the cost of an extra
// one is below.
//
// It mirrors policy's overridableRules -- the set the guard will actually let a
// grant override -- and TestGrantableRulesMatchWhatTheGuardOverrides in
// internal/policy fails if the two disagree. They must agree in both
// directions, because the failure is different each way:
//
//	grantable here, not overridable there: the grant is issued, the denial is
//	marked APPROVED and audited as OPERATOR_GRANTED, and the guard still
//	refuses. The operator believes they unblocked a refund that is still
//	blocked. And the grant stays LIVE: it matches on payment and amount alone,
//	so a later refusal of an overridable kind for the same payment and amount
//	can spend it. The mandate allows the clean retry; a replay of it is then
//	refused as ACTION_CONSUMED, finds the leftover grant, and goes out as a
//	second refund.
//
//	overridable there, not grantable here: a refusal the guard would let a
//	human correct cannot be corrected. Safe, but a broken workflow.
var grantable = map[string]struct{}{
	"NO_AUTHORIZED_ACTION":  {},
	"AMOUNT_NOT_AUTHORIZED": {},
	"ACTION_CONSUMED":       {},
}

// notGrantableBecause tells the operator what to do instead. It is advice, not
// policy: a rule missing from here is still refused, with a generic reason.
var notGrantableBecause = map[string]string{
	"RATE_LIMIT_EXCEEDED": "it is the merchant's own rate limit. The same refund " +
		"passes once the window clears, and a grant cannot raise the ceiling",
	"CUMULATIVE_CAP_EXCEEDED": "it is the merchant's own cumulative cap. Only a " +
		"new mandate from the merchant can raise it",
	"MANDATE_EXPIRED": "the whole mandate lapsed. The answer is a new mandate " +
		"from the merchant, not a patch from support",
	"MALFORMED_ARGUMENTS": "the guard could not read the request, and approving " +
		"it would authorize something nobody has read",
	"TOOL_NOT_SUPPORTED": "the tool surface is a property of this build, not an " +
		"incident decision",
	"TOOL_NOT_ALLOWED": "the tool surface is the merchant's grant, not an " +
		"incident decision",
}

// GrantableRules returns the refusal rules an operator grant can correct,
// sorted, so the policy package can prove it agrees with this list.
func GrantableRules() []string {
	out := make([]string, 0, len(grantable))
	for r := range grantable {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// IssueGrant mints a single-use authorization for one refused refund.
//
// IT REQUIRES AN opauth.Grant, which only opauth can mint and only after
// verifying the operator token against the Argon2id verifier. That is the same
// door as resolving an IN_DOUBT refund, and it is why THE GUARD HAS NO PATH
// INTO THIS TABLE: nothing on the request-handling side can construct the
// argument this function demands.
//
// The grant and its audit record land in ONE transaction with the denial's
// resolution. An issued grant with no audit row would be authority nobody can
// attribute; a resolved denial with no grant would be an operator believing
// they unblocked a refund that is still blocked.
//
// TTL is bounded here rather than trusted from the caller, because the caller
// is a flag on a command line during an incident.
func (s *Store) IssueGrant(g opauth.Grant, denialID int64, ttl time.Duration,
	reason string) (opgrant.Grant, error) {

	if !g.Valid() {
		return opgrant.Grant{}, errors.New("issuing an authorization requires a " +
			"verified operator credential")
	}
	if g.Subject() == "" || reason == "" {
		return opgrant.Grant{}, errors.New("operator identity and reason are required: " +
			"a grant nobody can be attributed to is not a decision")
	}
	if ttl <= 0 {
		ttl = opgrant.DefaultTTL
	}
	if ttl > opgrant.MaxTTL {
		return opgrant.Grant{}, fmt.Errorf("a %s grant is beyond the %s ceiling",
			ttl, opgrant.MaxTTL)
	}

	id, err := newGrantID()
	if err != nil {
		return opgrant.Grant{}, err
	}
	now := time.Now().UTC()

	tx, err := s.db.Begin()
	if err != nil {
		return opgrant.Grant{}, fmt.Errorf("storage: issue grant: %w", err)
	}
	defer tx.Rollback()

	d, err := s.denial(tx, denialID)
	if err != nil {
		return opgrant.Grant{}, err
	}
	// A grant is issued AGAINST A RECORDED REFUSAL, never freehand.
	//
	// That is what keeps this from being a general "authorize any refund"
	// command. The payment and the amount come from what the guard actually
	// refused, not from what the operator types, so an operator cannot approve a
	// refund nobody asked for -- and a compromised operator account cannot mint
	// authority for a payment the agent never touched.
	if d.Resolution == DenialDeclined {
		return opgrant.Grant{}, fmt.Errorf("denial %d was already declined; "+
			"re-approving it would erase that decision without a record", denialID)
	}
	// Whether a grant can mean anything is a property of WHY the guard refused.
	//
	// This used to hold by accident. The refusal sites that cannot be overridden
	// happened not to record a payment and a positive amount, so opgrant.Validate
	// rejected the grant -- for a different reason, and only because of which
	// fields a refusal happened to carry. A refusal that recorded both, which is
	// exactly what an operator reading the queue wants to see, would have made it
	// approvable, and FAILURES.md F53 shows that producing two refunds from one
	// authorization. Checked by name, so it no longer depends on the fields.
	if _, ok := grantable[d.Rule]; !ok {
		why := notGrantableBecause[d.Rule]
		if why == "" {
			why = "no operator grant can override that refusal"
		}
		return opgrant.Grant{}, fmt.Errorf("denial %d was refused as %s, which an "+
			"operator grant cannot correct: %s. Decline it instead, so the queue "+
			"records that it was seen", denialID, d.Rule, why)
	}
	if d.MandateID != s.mandateID {
		return opgrant.Grant{}, fmt.Errorf("denial %d belongs to %s, not %s; "+
			"approve it with that mandate so the grant lands in the right ledger",
			denialID, d.MandateID, s.mandateID)
	}

	out := opgrant.Grant{
		GrantID:   id,
		MandateID: s.mandateID,
		DenialID:  denialID,
		PaymentID: d.PaymentID,
		// EXACTLY what was refused. Not rounded, not adjusted, not read from a
		// flag: the operator is approving a specific refusal, and the figure is a
		// property of that refusal.
		AmountPaise: d.AmountPaise,
		IssuedAt:    now,
		ExpiresAt:   now.Add(ttl),
		Actor:       g.Subject(),
		Reason:      reason,
	}
	if err := out.Validate(now); err != nil {
		return opgrant.Grant{}, err
	}

	if _, err := tx.Exec(
		`INSERT INTO operator_grant (grant_id, mandate_id, denial_id, payment_id,
		         amount_paise, issued_at, expires_at, expires_ns, actor, reason)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		out.GrantID, out.MandateID, out.DenialID, out.PaymentID, out.AmountPaise,
		out.IssuedAt.Format(time.RFC3339Nano), out.ExpiresAt.Format(time.RFC3339Nano),
		out.ExpiresAt.UnixNano(), out.Actor, out.Reason); err != nil {
		return opgrant.Grant{}, fmt.Errorf("storage: issue grant: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE denial SET resolution = ? WHERE id = ?`, DenialApproved, denialID); err != nil {
		return opgrant.Grant{}, fmt.Errorf("storage: resolve denial: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO audit (at, actor, action_id, mandate_id, from_state, to_state,
		                    refund_landed, reason)
		 VALUES (?, ?, ?, ?, 'REFUSED', 'OPERATOR_GRANTED', 0, ?)`,
		now.Format(time.RFC3339Nano), out.Actor, out.GrantID, s.mandateID,
		fmt.Sprintf("denial %d (%s, %d paise on %s): %s",
			denialID, d.Rule, d.AmountPaise, d.PaymentID, reason)); err != nil {
		return opgrant.Grant{}, fmt.Errorf("storage: audit grant: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return opgrant.Grant{}, fmt.Errorf("storage: issue grant: %w", err)
	}
	return out, nil
}

// DeclineDenial records that a human looked at a refusal and agreed with it.
//
// This is not decoration. A queue where the only recorded outcome is "approved"
// cannot distinguish a refusal nobody has read from one somebody judged
// correct, and the difference between those two is the entire operational
// question: is the backlog growing because the guard is wrong, or because
// nobody is looking?
func (s *Store) DeclineDenial(g opauth.Grant, denialID int64, reason string) error {
	if !g.Valid() {
		return errors.New("recording a decision requires a verified operator credential")
	}
	if g.Subject() == "" || reason == "" {
		return errors.New("operator identity and reason are required")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("storage: decline: %w", err)
	}
	defer tx.Rollback()

	d, err := s.denial(tx, denialID)
	if err != nil {
		return err
	}
	if d.Resolution == DenialApproved {
		return fmt.Errorf("denial %d was already approved and a grant issued; "+
			"declining it now would not withdraw that grant, which is why this "+
			"refuses rather than pretending to", denialID)
	}
	if _, err := tx.Exec(
		`UPDATE denial SET resolution = ? WHERE id = ?`, DenialDeclined, denialID); err != nil {
		return fmt.Errorf("storage: decline: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO audit (at, actor, action_id, mandate_id, from_state, to_state,
		                    refund_landed, reason)
		 VALUES (?, ?, ?, ?, 'REFUSED', 'OPERATOR_DECLINED', 0, ?)`,
		now, g.Subject(), fmt.Sprintf("denial:%d", denialID), s.mandateID,
		fmt.Sprintf("denial %d (%s, %d paise on %s): %s",
			denialID, d.Rule, d.AmountPaise, d.PaymentID, reason)); err != nil {
		return fmt.Errorf("storage: audit decline: %w", err)
	}
	return tx.Commit()
}

// LiveGrants returns every unexpired grant for this mandate.
//
// READ FROM THE DATABASE, NOT FROM A STARTUP SNAPSHOT. That is the property
// that makes the whole workflow work: a guard which started an hour ago sees a
// grant issued a moment ago, so an operator does not have to restart the proxy
// to unblock a refund. It is also why issuing a grant is safe to do beside a
// live guard while resolving an IN_DOUBT action is not -- one is state the guard
// re-reads, the other is state it caches.
//
// Expired rows are filtered in SQL rather than in Go so the caller cannot
// forget, and they are deliberately NOT deleted: an expired grant is a record
// of a decision somebody made, and the audit trail is the point.
func (s *Store) LiveGrants(mandateID string, now time.Time) ([]opgrant.Grant, error) {
	rows, err := s.db.Query(
		`SELECT grant_id, mandate_id, denial_id, payment_id, amount_paise,
		        issued_at, expires_at, actor, reason
		   FROM operator_grant
		  WHERE mandate_id = ? AND expires_ns > ?
		  ORDER BY issued_at`, mandateID, now.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("storage: live grants: %w", err)
	}
	defer rows.Close()

	var out []opgrant.Grant
	for rows.Next() {
		var g opgrant.Grant
		var issued, expires string
		if err := rows.Scan(&g.GrantID, &g.MandateID, &g.DenialID, &g.PaymentID,
			&g.AmountPaise, &issued, &expires, &g.Actor, &g.Reason); err != nil {
			return nil, err
		}
		g.IssuedAt, _ = time.Parse(time.RFC3339Nano, issued)
		g.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires)
		out = append(out, g)
	}
	return out, rows.Err()
}
