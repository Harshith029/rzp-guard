// Package mandate models the merchant-issued capability list.
//
// The mandate is the authorization boundary. It is loaded from a path supplied
// at process launch, before any agent connects; nothing arriving over JSON-RPC
// can set, replace, extend or reload it.
//
// Authorization is per DISCRETE refund action, not a policy range. An action
// grants one refund of a specific amount (or up to a bound the merchant
// deliberately chose) against one payment, and is consumed when used. Two
// legitimate partial refunds of equal value are two actions, which is why they
// both pass.
package mandate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	// Razorpay's create_refund schema enforces a 100 paise floor. Verified at
	// runtime: evidence/tools_list.json shows amount {"minimum":100}.
	MinRefundPaise int64 = 100

	// Razorpay documents a >=10 character floor for refund idempotency keys,
	// restricted to letters, digits, underscore and hyphen.
	receiptMinLen  = 10
	receiptPrefix  = "rzpg_"
	receiptHashLen = 12
)

var (
	actionIDPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{3,64}$`)
	receiptPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	mandateIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{3,64}$`)
)

// Action is one discrete, single-use authorization to refund a specific payment.
type Action struct {
	ActionID  string `json:"action_id"`
	PaymentID string `json:"payment_id"`

	// Exactly one of these. Exact is the default a merchant should use. A bound
	// is opt-in and means the merchant deliberately delegated the figure, so an
	// amount inside it is authorized BY THE MERCHANT'S OWN CHOICE.
	AmountPaise    *int64 `json:"amount_paise,omitempty"`
	MaxAmountPaise *int64 `json:"max_amount_paise,omitempty"`
}

// IsBounded reports whether the merchant delegated the figure.
func (a Action) IsBounded() bool { return a.MaxAmountPaise != nil }

// Admits reports whether this action authorizes a refund of exactly this amount.
func (a Action) Admits(amountPaise int64) bool {
	if a.AmountPaise != nil {
		return amountPaise == *a.AmountPaise
	}
	return amountPaise >= MinRefundPaise && amountPaise <= *a.MaxAmountPaise
}

// Ceiling is the worst-case value this action can consume, for reservation.
func (a Action) Ceiling() int64 {
	if a.AmountPaise != nil {
		return *a.AmountPaise
	}
	return *a.MaxAmountPaise
}

func (a Action) Describe() string {
	if a.AmountPaise != nil {
		return fmt.Sprintf("%s (exactly %d paise on %s)", a.ActionID, *a.AmountPaise, a.PaymentID)
	}
	return fmt.Sprintf("%s (up to %d paise on %s)", a.ActionID, *a.MaxAmountPaise, a.PaymentID)
}

func (a Action) validate() error {
	if !actionIDPattern.MatchString(a.ActionID) {
		return fmt.Errorf("action_id %q must match %s: it becomes a provider-side "+
			"correlation key, so spaces and punctuation are not acceptable",
			a.ActionID, actionIDPattern)
	}
	if strings.TrimSpace(a.PaymentID) == "" {
		return fmt.Errorf("action %s: payment_id is required", a.ActionID)
	}
	if (a.AmountPaise == nil) == (a.MaxAmountPaise == nil) {
		return fmt.Errorf("action %s: set exactly one of amount_paise (exact, "+
			"preferred) or max_amount_paise (bounded, opt-in)", a.ActionID)
	}
	if v := a.Ceiling(); v < MinRefundPaise {
		return fmt.Errorf("action %s: amount %d is below Razorpay's %d paise floor, "+
			"so it could never be forwarded", a.ActionID, v, MinRefundPaise)
	}
	return nil
}

// Limits are session-wide ceilings.
type Limits struct {
	MaxCumulativePaise int64 `json:"max_cumulative_paise"`
	MaxCallsPerMinute  int   `json:"max_calls_per_minute"`
}

// Mandate is the complete grant for one proxy session.
type Mandate struct {
	MandateID               string    `json:"mandate_id"`
	ExpiresAt               time.Time `json:"expires_at"`
	AllowedTools            []string  `json:"allowed_tools"`
	AuthorizedRefundActions []Action  `json:"authorized_refund_actions"`
	Limits                  Limits    `json:"global"`
}

// Load parses and validates a mandate document. Anything malformed is an error;
// a mandate that will not parse must never be treated as a permissive one.
func Load(raw []byte) (*Mandate, error) {
	var m Mandate
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("mandate: %w", err)
	}
	// A mandate file holds exactly one document. Decode stops at the end of the
	// first value, so a file carrying a second one would have it silently
	// ignored -- and the authority a reviewer reads would not be the authority
	// the guard enforces. Signature verification covers the whole file and would
	// catch this, but signing is opt-in, so the parser cannot rely on it.
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("mandate: file carries more than one JSON value. " +
			"A mandate is one document; a second would be ignored, so what is " +
			"reviewed would not be what is enforced")
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("mandate: %w", err)
	}
	return &m, nil
}

func (m *Mandate) validate() error {
	if !mandateIDPattern.MatchString(m.MandateID) {
		return fmt.Errorf("mandate_id %q must match %s", m.MandateID, mandateIDPattern)
	}
	if m.ExpiresAt.IsZero() {
		return fmt.Errorf("expires_at is required")
	}
	if m.Limits.MaxCallsPerMinute <= 0 {
		return fmt.Errorf("max_calls_per_minute must be positive")
	}
	if m.Limits.MaxCumulativePaise < 0 {
		return fmt.Errorf("max_cumulative_paise must not be negative")
	}
	seen := make(map[string]struct{}, len(m.AuthorizedRefundActions))
	for _, a := range m.AuthorizedRefundActions {
		if err := a.validate(); err != nil {
			return err
		}
		if _, dup := seen[a.ActionID]; dup {
			return fmt.Errorf("duplicate action_id %q: action ids key both "+
				"consumption and the injected receipt, so they must be unique", a.ActionID)
		}
		seen[a.ActionID] = struct{}{}
		// Fail at load time, not at forward time: a mandate that would generate
		// an invalid receipt must never reach the money path.
		if _, err := ReceiptFor(m.MandateID, a.ActionID); err != nil {
			return fmt.Errorf("action %s: %w", a.ActionID, err)
		}
	}
	return nil
}

// IsExpired reports whether the grant has lapsed.
func (m *Mandate) IsExpired(now time.Time) bool { return !now.Before(m.ExpiresAt) }

// PermitsTool implements default-deny: anything not listed is denied,
// including tools this build has never heard of.
func (m *Mandate) PermitsTool(tool string) bool {
	for _, t := range m.AllowedTools {
		if t == tool {
			return true
		}
	}
	return false
}

// Find returns the actions authorizing a given payment.
func (m *Mandate) Find(paymentID string) []Action {
	var out []Action
	for _, a := range m.AuthorizedRefundActions {
		if a.PaymentID == paymentID {
			out = append(out, a)
		}
	}
	return out
}

// Literals are values the merchant named, used to label arguments USER_MANDATED.
func (m *Mandate) Literals() map[string]struct{} {
	out := map[string]struct{}{m.MandateID: {}}
	for _, a := range m.AuthorizedRefundActions {
		out[a.PaymentID] = struct{}{}
		out[a.ActionID] = struct{}{}
		out[fmt.Sprintf("%d", a.Ceiling())] = struct{}{}
	}
	return out
}

// ReceiptFor derives the deterministic idempotency receipt for an action.
//
// It hashes mandate_id + action_id rather than concatenating them, which buys
// three properties the prototype's naive "rzpg_"+action_id lacked:
//
//   - guaranteed charset, whatever the action id looks like;
//   - guaranteed length floor, even for a short action id (the prototype
//     produced the 6-character "rzpg_a" against a documented 10 minimum);
//   - collision resistance across mandates, not merely uniqueness within one
//     in-memory mandate, which matters because this is a provider-side
//     correlation key.
//
// It is a 48-bit TRUNCATED hash: collision-resistant, NOT guaranteed unique.
// Actual uniqueness is enforced by a UNIQUE constraint in storage, which
// rejects a collision rather than preventing one.
//
// It stays deterministic: the same mandate and action always yield the same
// receipt.
//
// A one-off Test Mode observation (evidence/g16/RECEIPT_IDEMPOTENCY.md) suggests
// Razorpay also rejects a duplicate receipt. That observation is NOT treated as
// a verified layer: no raw response was captured, and reproducing it would
// require sending a refund around the guard, which this repository deliberately
// cannot do. Nothing here depends on it.
//
// WHAT REPLAY PROTECTION ACTUALLY RESTS ON: the durable action ledger, which
// refuses a replay locally with ACTION_CONSUMED and forwards nothing. That is
// proven by committed evidence.
//
// If the provider-side behaviour is real, it would only help when a retry
// reproduces the IDENTICAL receipt -- which requires the identical mandate_id
// and action_id. That holds when a mandate file is reloaded, and fails entirely
// for anything minting fresh action ids per run.
//
// The 48-bit truncation fails SAFE either way. A collision between two distinct
// actions can only cause a refund to be refused, never duplicated.
//
// Live acceptance of a fresh receipt is asserted by the captured G1.6 evidence
// (./run.sh verify-refund-evidence).
func ReceiptFor(mandateID, actionID string) (string, error) {
	sum := sha256.Sum256([]byte(mandateID + "/" + actionID))
	r := receiptPrefix + hex.EncodeToString(sum[:])[:receiptHashLen]
	if len(r) < receiptMinLen {
		return "", fmt.Errorf("generated receipt %q is %d chars, below the %d minimum",
			r, len(r), receiptMinLen)
	}
	if !receiptPattern.MatchString(r) {
		return "", fmt.Errorf("generated receipt %q contains characters outside "+
			"[A-Za-z0-9_-]", r)
	}
	return r, nil
}

// ReceiptForSet derives the receipt for a call that consumes SEVERAL actions.
//
// One forwarded refund may consume more than one action: a merchant who
// authorized 18500 and 19000 separately has authorized 37500, and an agent
// issuing it as a single call is asking for exactly what was granted.
//
// The set is sorted so the receipt does not depend on match order, and joined
// with "+", which cannot occur inside an action_id (actionIDPattern permits
// only [A-Za-z0-9_-]). That separator choice is what makes the mapping
// injective: no set of ids can collide with a single id, and no two distinct
// sets can produce the same string.
//
// A ONE-ELEMENT SET DELIBERATELY YIELDS THE PLAIN ReceiptFor VALUE. Every
// receipt this project has ever issued, including the ones in the frozen study
// traces, stays bit-identical -- so combining changes behaviour only where it
// actually fires.
func ReceiptForSet(mandateID string, actionIDs []string) (string, error) {
	switch len(actionIDs) {
	case 0:
		return "", fmt.Errorf("receipt: no actions given")
	case 1:
		return ReceiptFor(mandateID, actionIDs[0])
	}
	ids := append([]string(nil), actionIDs...)
	sort.Strings(ids)
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			return "", fmt.Errorf("receipt: %s appears twice in one call", ids[i])
		}
	}
	return ReceiptFor(mandateID, strings.Join(ids, "+"))
}
