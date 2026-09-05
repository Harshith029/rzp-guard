package intent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/harshith/rzp-guard/internal/mandate"
)

// Coverage is the per-line record of what the compiled mandate actually grants
// against what the intent asked for.
//
// It is the artefact the evaluation was missing. Arm E measured coverage by
// hand -- a rater comparing a mandate against a sentence and calling it exact,
// under or over -- and all eight misses were over. This computes the same
// judgement mechanically, at authoring time, from the two documents themselves.
type Coverage struct {
	ItemID    string `json:"item_id"`
	PaymentID string `json:"payment_id"`
	ActionID  string `json:"action_id"`

	// AskedPaise is the figure the intent line states. For a delegated line
	// there is no such figure and this is zero; Class says which case it is.
	AskedPaise   int64 `json:"asked_paise"`
	GrantedPaise int64 `json:"granted_paise"`

	// Class is "exact" or "delegated". There is deliberately no "over": an
	// over-granting compile is refused, not labelled.
	Class string `json:"class"`

	// HeadroomPaise is what the agent may move beyond any named figure. It is
	// zero for every exact line, by construction, and Compile fails if it is
	// not. For a delegated line it is the whole ceiling, and it is printed
	// rather than buried, because a merchant approving discretion should see
	// its size.
	HeadroomPaise int64 `json:"headroom_paise"`

	// Because is the merchant's own sentence, carried through unparsed.
	Because string `json:"because"`

	// DelegatedBecause is why the merchant chose not to name the figure.
	DelegatedBecause string `json:"delegated_because,omitempty"`
}

// Provenance binds a mandate to the intent it was compiled from.
//
// It travels beside the mandate as <mandate>.intent.json and is what makes the
// grant reviewable AFTER the fact: an auditor holding both files can recompute
// this record and see that the authority matches the sentence. Without it, a
// mandate on disk is a list of amounts with no stated reason, which is exactly
// the artefact the raters found impossible to judge quickly.
type Provenance struct {
	IntentID   string     `json:"intent_id"`
	MandateID  string     `json:"mandate_id"`
	IssuedBy   string     `json:"issued_by"`
	IssuedAt   time.Time  `json:"issued_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	CompiledBy string     `json:"compiled_by"`
	Coverage   []Coverage `json:"coverage"`

	// TotalAskedPaise and TotalGrantedPaise are equal for an intent with no
	// delegated lines. When they differ, the difference is the discretion the
	// merchant granted on purpose, and TotalHeadroomPaise names it.
	TotalAskedPaise    int64 `json:"total_asked_paise"`
	TotalGrantedPaise  int64 `json:"total_granted_paise"`
	TotalHeadroomPaise int64 `json:"total_headroom_paise"`

	// IntentSHA256 and MandateSHA256 are over the exact bytes of each document.
	// Verify recomputes both, so an edit to either file after compilation is
	// detectable without trusting a timestamp.
	IntentSHA256  string `json:"intent_sha256"`
	MandateSHA256 string `json:"mandate_sha256"`
}

// Result is everything a compile produces.
type Result struct {
	Mandate     *mandate.Mandate
	MandateJSON []byte
	Provenance  Provenance
}

// CompiledBy is stamped into the provenance record so a reader knows which
// compiler wrote a grant. Bumped when the compilation rules change in a way
// that could produce a different mandate from the same intent.
const CompiledBy = "rzp-mandate/1"

// Compile turns an intent into a mandate, or refuses it.
//
// ORDER OF CHECKS IS DELIBERATE, and it is the mirror image of policy.Decide:
// document-wide rules first, then per-line resolution, then the cross-line
// arithmetic, then -- last -- the coverage assertion that re-derives the whole
// grant and compares it to what is about to be emitted. The final check does
// not trust the loop above it. That is the point of having it.
//
// now bounds nothing about the authority itself; it exists so a mandate that
// would be born already expired is refused here rather than discovered by an
// agent whose every call is denied.
func Compile(in *Intent, now time.Time) (*Result, error) {
	if in == nil {
		return nil, refuse(MalformedIntent, "", "no intent")
	}
	if !intentIDPattern.MatchString(in.IntentID) {
		return nil, refuse(MalformedIntent, "",
			"intent_id %q must match %s: it becomes part of the mandate id",
			in.IntentID, intentIDPattern)
	}
	if strings.TrimSpace(in.IssuedBy) == "" {
		return nil, refuse(MalformedIntent, "",
			"issued_by is required: an authority with no author cannot be reviewed")
	}
	if in.IssuedAt.IsZero() {
		return nil, refuse(MalformedIntent, "", "issued_at is required")
	}
	if len(in.Items) == 0 {
		return nil, refuse(EmptyIntent, "",
			"an intent with no items compiles to a mandate that authorizes nothing. "+
				"That is not a safe default, it is a mistake in the document")
	}
	if in.MaxCallsPerMinute <= 0 {
		return nil, refuse(MalformedIntent, "",
			"max_calls_per_minute must be positive; the guard requires a rate bound "+
				"and this compiler will not invent one")
	}

	// The window. Both ends matter: too long is authority nobody revisits, and
	// zero is a mandate that denies everything and looks like a guard fault.
	window := in.ValidFor.Std()
	switch {
	case window < MinWindow:
		return nil, refuse(UnboundedWindow, "",
			"valid_for is %s; a mandate shorter than %s is expired or nearly so "+
				"before an agent can use it, which presents as a guard defect rather "+
				"than an authoring one", window, MinWindow)
	case window > MaxWindow:
		return nil, refuse(UnboundedWindow, "",
			"valid_for is %s, beyond the %s ceiling. An expiry is authority in the "+
				"time dimension: a mandate alive next week authorizes an agent that "+
				"is prompt-injected next week", window, MaxWindow)
	}
	expiresAt := in.IssuedAt.Add(window).UTC()
	if !now.Before(expiresAt) {
		return nil, refuse(UnboundedWindow, "",
			"issued_at %s plus valid_for %s expires at %s, which is not in the "+
				"future. Compiling it would produce a mandate that denies every call",
			in.IssuedAt.Format(time.RFC3339), window, expiresAt.Format(time.RFC3339))
	}

	// Tools. A mandate can only narrow the guard's surface, but it can widen the
	// agent's reach relative to the merchant's sentence, and this is where that
	// is stopped. create_refund is added by the compiler, never requested: an
	// intent with items IS a refund request, and making the merchant also list
	// the tool would be a second place to get it wrong.
	tools := make([]string, 0, len(in.ReadTools)+1)
	seenTool := map[string]struct{}{}
	for _, t := range in.ReadTools {
		if _, ok := readOnlyTools[t]; !ok {
			return nil, refuse(ToolWidening, "",
				"read_tools may only contain read-only tools; %q is not one of them. "+
					"If it is a tool the guard now supports, adding it here is a "+
					"decision for a human", t)
		}
		if _, dup := seenTool[t]; dup {
			return nil, refuse(MalformedIntent, "", "read_tools lists %q twice", t)
		}
		seenTool[t] = struct{}{}
		tools = append(tools, t)
	}
	sort.Strings(tools)
	tools = append(tools, "create_refund")

	// Per-line resolution.
	lines := make([]line, 0, len(in.Items))
	seenItem := make(map[string]struct{}, len(in.Items))
	perPayment := map[string]int64{}
	captured := map[string]int64{}
	for _, it := range in.Items {
		if !itemIDPattern.MatchString(it.ItemID) {
			return nil, refuse(MalformedIntent, it.ItemID,
				"item_id %q must match %s: it becomes the action id, which is a "+
					"provider-side correlation key", it.ItemID, itemIDPattern)
		}
		if _, dup := seenItem[it.ItemID]; dup {
			return nil, refuse(DuplicateItem, it.ItemID,
				"item_id appears twice. Two lines with one id compile to one action, "+
					"so one of the merchant's two requests would silently vanish")
		}
		seenItem[it.ItemID] = struct{}{}
		if strings.TrimSpace(it.PaymentID) == "" {
			return nil, refuse(MalformedIntent, it.ItemID, "payment_id is required")
		}
		if strings.TrimSpace(it.Because) == "" {
			return nil, refuse(MalformedIntent, it.ItemID,
				"because is required: a line with no stated reason cannot be reviewed, "+
					"and an unreviewable grant is how an over-broad one survives")
		}

		ln, err := resolve(it)
		if err != nil {
			return nil, err
		}
		if ln.ceiling < mandate.MinRefundPaise {
			return nil, refuse(BelowFloor, it.ItemID,
				"%d paise is below Razorpay's %d paise floor, so the guard could never "+
					"forward it; the line would be authority that cannot be used",
				ln.ceiling, mandate.MinRefundPaise)
		}

		// Captured-amount consistency, per payment and in aggregate. Checked
		// against the FIRST captured figure seen for a payment: two lines that
		// disagree about what a payment took are a document a human must fix.
		if it.CapturedPaise != nil {
			if prev, ok := captured[it.PaymentID]; ok && prev != *it.CapturedPaise {
				return nil, refuse(MalformedIntent, it.ItemID,
					"two lines give different captured_paise for %s (%d and %d)",
					it.PaymentID, prev, *it.CapturedPaise)
			}
			captured[it.PaymentID] = *it.CapturedPaise
			if ln.ceiling > *it.CapturedPaise {
				return nil, refuse(ExceedsCaptured, it.ItemID,
					"%d paise against a payment that captured %d. A refund larger than "+
						"its payment is not something a merchant means",
					ln.ceiling, *it.CapturedPaise)
			}
		}
		perPayment[it.PaymentID] += ln.ceiling
		lines = append(lines, ln)
	}

	for pid, total := range perPayment {
		if cap, ok := captured[pid]; ok && total > cap {
			return nil, refuse(ExceedsCaptured, "",
				"the lines against %s total %d paise, more than the %d it captured. "+
					"Individually each fits; together they do not, and the guard "+
					"enforces them individually", pid, total, cap)
		}
	}

	// The merchant's own checksum, if they wrote one.
	var asked, granted, headroom int64
	for _, ln := range lines {
		granted += ln.ceiling
		if ln.delegated {
			headroom += ln.ceiling
		} else {
			asked += ln.ceiling
		}
	}
	if in.TotalPaise != nil && *in.TotalPaise != granted {
		return nil, refuse(TotalDisagrees, "",
			"total_paise says %d, the lines sum to %d. The difference is %d paise. "+
				"This compiler does not reconcile them: a merchant who wrote the "+
				"total twice and got two answers has found a mistake, and finding it "+
				"here is the entire value of writing it twice",
			*in.TotalPaise, granted, *in.TotalPaise-granted)
	}

	// Build the mandate. Every field is DERIVED. Nothing the intent says about
	// the cumulative cap, the expiry date or the action ids is accepted, because
	// each of those is a place a hand-written mandate can quietly exceed the
	// sentence it came from.
	m := &mandate.Mandate{
		MandateID:    "mnd_" + in.IntentID,
		ExpiresAt:    expiresAt,
		AllowedTools: tools,
		Limits: mandate.Limits{
			// EXACTLY the sum of the ceilings. Not rounded up, not padded.
			//
			// The demo mandate in examples/ carries a 200000 cap over a single
			// 50000 action -- 150000 paise of authority no sentence asked for.
			// It is harmless there because no other action exists to spend it,
			// and it is exactly the habit that produces a real over-grant when
			// one does.
			MaxCumulativePaise: granted,
			MaxCallsPerMinute:  in.MaxCallsPerMinute,
		},
	}
	for _, ln := range lines {
		a := mandate.Action{ActionID: ln.item.ItemID, PaymentID: ln.item.PaymentID}
		v := ln.ceiling
		if ln.delegated {
			a.MaxAmountPaise = &v
		} else {
			a.AmountPaise = &v
		}
		m.AuthorizedRefundActions = append(m.AuthorizedRefundActions, a)
	}

	// Emit, then RE-PARSE through the guard's own loader.
	//
	// The compiler is not the authority on whether a mandate is valid; the guard
	// is. A document this package considers correct but mandate.Load rejects is
	// a document that would be discovered at 3am by a guard refusing to start,
	// and it is free to discover it here instead.
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("intent: encode mandate: %w", err)
	}
	raw = append(raw, '\n')
	loaded, err := mandate.Load(raw)
	if err != nil {
		return nil, fmt.Errorf("intent: the compiled mandate does not load: %w", err)
	}

	prov := Provenance{
		IntentID:           in.IntentID,
		MandateID:          m.MandateID,
		IssuedBy:           in.IssuedBy,
		IssuedAt:           in.IssuedAt.UTC(),
		ExpiresAt:          expiresAt,
		CompiledBy:         CompiledBy,
		TotalAskedPaise:    asked,
		TotalGrantedPaise:  granted,
		TotalHeadroomPaise: headroom,
	}
	for _, ln := range lines {
		c := Coverage{
			ItemID:       ln.item.ItemID,
			PaymentID:    ln.item.PaymentID,
			ActionID:     ln.item.ItemID,
			GrantedPaise: ln.ceiling,
			Because:      ln.item.Because,
			Class:        "exact",
			AskedPaise:   ln.ceiling,
		}
		if ln.delegated {
			c.Class = "delegated"
			c.AskedPaise = 0
			c.HeadroomPaise = ln.ceiling
			c.DelegatedBecause = ln.item.Refund.DelegatedBecause
		}
		prov.Coverage = append(prov.Coverage, c)
	}

	// THE COVERAGE ASSERTION.
	//
	// Everything above could be wrong. This re-derives the grant from the
	// coverage record and compares it against the mandate that is about to be
	// written -- action by action, amount by amount, cap included. It is the
	// check that turns the invariant in the package comment from a claim into a
	// property, and it is deliberately written against the OUTPUT rather than
	// the intermediate `lines` slice, so a bug in the loop above cannot pass it.
	if err := assertCoverage(loaded, prov); err != nil {
		return nil, err
	}

	prov.IntentSHA256 = "" // filled by the caller, which holds the intent bytes
	prov.MandateSHA256 = digest(raw)

	return &Result{Mandate: loaded, MandateJSON: raw, Provenance: prov}, nil
}

// assertCoverage proves the emitted mandate grants exactly the coverage record
// and nothing else.
//
// Three separate ways to over-authorize, three checks:
//
//	an action nothing in the record accounts for  -> COVERAGE_OVER
//	an action granting more than its line asked   -> COVERAGE_OVER
//	a cumulative cap above the sum of the actions -> COVERAGE_OVER
//
// and the mirror of the second and third, which is the false-positive
// direction: a grant SMALLER than the sentence blocks a legitimate refund, so
// it is refused too rather than shipped as the safe error.
func assertCoverage(m *mandate.Mandate, p Provenance) error {
	byAction := make(map[string]Coverage, len(p.Coverage))
	for _, c := range p.Coverage {
		byAction[c.ActionID] = c
	}
	if len(byAction) != len(p.Coverage) {
		return refuse(CoverageOver, "",
			"the coverage record names an action twice; it cannot account for the grant")
	}
	if len(m.AuthorizedRefundActions) != len(p.Coverage) {
		return refuse(CoverageOver, "",
			"the mandate carries %d actions and the coverage record accounts for %d. "+
				"An action nothing accounts for is authority no sentence asked for",
			len(m.AuthorizedRefundActions), len(p.Coverage))
	}

	var sum int64
	for _, a := range m.AuthorizedRefundActions {
		c, ok := byAction[a.ActionID]
		if !ok {
			return refuse(CoverageOver, a.ActionID,
				"the mandate authorizes an action the coverage record does not account for")
		}
		if a.PaymentID != c.PaymentID {
			return refuse(CoverageOver, a.ActionID,
				"the mandate points this action at %s, the record at %s",
				a.PaymentID, c.PaymentID)
		}
		switch {
		case a.IsBounded() && c.Class != "delegated":
			return refuse(CoverageOver, a.ActionID,
				"compiled to a BOUNDED action from a line that named an exact figure. "+
					"A bound admits any amount up to it, so this grants discretion the "+
					"merchant did not")
		case !a.IsBounded() && c.Class == "delegated":
			return refuse(CoverageUnder, a.ActionID,
				"compiled to an exact action from a delegated line; the merchant said "+
					"they did not know the figure, and this pins one")
		}
		if got := a.Ceiling(); got != c.GrantedPaise {
			return refuse(CoverageOver, a.ActionID,
				"the mandate authorizes %d paise, the record accounts for %d", got, c.GrantedPaise)
		}
		if c.Class == "exact" && c.GrantedPaise != c.AskedPaise {
			return refuse(CoverageOver, a.ActionID,
				"granted %d paise against an asked figure of %d", c.GrantedPaise, c.AskedPaise)
		}
		sum += a.Ceiling()
	}

	switch {
	case m.Limits.MaxCumulativePaise > sum:
		return refuse(CoverageOver, "",
			"the cumulative cap is %d paise over a mandate whose actions total %d. "+
				"The %d paise of difference is spendable authority that no line of "+
				"the intent asked for",
			m.Limits.MaxCumulativePaise, sum, m.Limits.MaxCumulativePaise-sum)
	case m.Limits.MaxCumulativePaise < sum:
		return refuse(CoverageUnder, "",
			"the cumulative cap is %d paise under a mandate whose actions total %d, "+
				"so the last %d paise of the merchant's own request would be refused",
			m.Limits.MaxCumulativePaise, sum, sum-m.Limits.MaxCumulativePaise)
	}
	if sum != p.TotalGrantedPaise {
		return refuse(CoverageOver, "",
			"the mandate grants %d paise and the record totals %d", sum, p.TotalGrantedPaise)
	}
	return nil
}

// Verify recomputes a provenance record from the intent and mandate bytes it
// names, and reports any disagreement.
//
// This is the after-the-fact half. Compile guarantees a correct grant at the
// moment of writing; nothing stops someone editing the mandate afterwards. The
// digests catch that, and re-running the compile catches an intent edited to
// justify a mandate that was widened first.
func Verify(intentRaw, mandateRaw, provRaw []byte, now time.Time) error {
	var stored Provenance
	if err := json.Unmarshal(provRaw, &stored); err != nil {
		return fmt.Errorf("provenance: %w", err)
	}
	if got := digest(intentRaw); got != stored.IntentSHA256 {
		return fmt.Errorf("the intent file has changed since the mandate was "+
			"compiled from it (sha256 %s, record says %s). The grant on disk is no "+
			"longer explained by the sentence beside it", got, stored.IntentSHA256)
	}
	if got := digest(mandateRaw); got != stored.MandateSHA256 {
		return fmt.Errorf("the mandate file has changed since it was compiled "+
			"(sha256 %s, record says %s). Whatever it now authorizes, it is not what "+
			"this intent asked for", got, stored.MandateSHA256)
	}

	in, err := Load(intentRaw)
	if err != nil {
		return err
	}
	// Recompile at the intent's own issue time. Using the wall clock would make
	// verification of a correctly-issued, now-expired grant fail as though the
	// document were wrong -- and an expired mandate is exactly the kind an
	// auditor reviews.
	res, err := Compile(in, in.IssuedAt.Add(MinWindow))
	if err != nil {
		return fmt.Errorf("the intent no longer compiles: %w", err)
	}
	if !bytesEqual(res.MandateJSON, mandateRaw) {
		return fmt.Errorf("recompiling %s does not reproduce the mandate beside it; "+
			"the mandate was edited by hand or written by a different compiler",
			stored.IntentID)
	}
	return nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Digest is the sha256 the provenance record uses, exported so the CLI records
// the intent's digest over the exact bytes it read from disk rather than over a
// re-encoding of the parsed document.
func Digest(b []byte) string { return digest(b) }

// MarshalProvenance renders a coverage record for the sidecar file.
func MarshalProvenance(p Provenance) ([]byte, error) {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("intent: encode coverage record: %w", err)
	}
	return append(b, '\n'), nil
}

// UnmarshalProvenance reads one back.
func UnmarshalProvenance(b []byte) (Provenance, error) {
	var p Provenance
	if err := json.Unmarshal(b, &p); err != nil {
		return Provenance{}, fmt.Errorf("intent: coverage record: %w", err)
	}
	return p, nil
}

// Stamp fills in a missing issued_at and returns the canonical bytes of the
// resulting document.
//
// WHY THIS EXISTS. Compilation must be deterministic -- the same intent has to
// produce the same mandate bytes, or a detached signature over one would not
// verify against the other, and Verify could not recompile. That forces expiry
// to derive from issued_at rather than the wall clock, which in turn makes
// issued_at mandatory.
//
// But an intent TEMPLATE has no issue time, and a merchant should not have to
// hand-edit a timestamp into a document before every use. So the moment of
// issue is stamped once, here, and the STAMPED document becomes the one of
// record: it is what the digest covers and what travels beside the mandate.
// Nothing downstream sees a document without a time on it.
//
// An intent that already carries issued_at is returned untouched, bytes and
// all. A merchant's own document is never re-encoded on its way into the
// record -- the file they signed off on is the file that is hashed.
func Stamp(raw []byte, now time.Time) ([]byte, error) {
	in, err := Load(raw)
	if err != nil {
		return nil, err
	}
	if !in.IssuedAt.IsZero() {
		return raw, nil
	}
	in.IssuedAt = now.UTC().Truncate(time.Second)
	out, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("intent: stamp: %w", err)
	}
	return append(out, '\n'), nil
}
