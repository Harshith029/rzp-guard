// Package intent compiles a merchant's stated intent into a mandate, and
// refuses any intent it cannot realize exactly.
//
// WHY THIS PACKAGE EXISTS, IN ONE PARAGRAPH.
//
// The guard enforces a mandate. It never sees the sentence the mandate was
// written from, so it structurally cannot notice that the mandate authorizes
// MORE than the merchant asked for. Every miss in the held-out evaluation was
// exactly that: a mandate whose authority exceeded the merchant's sentence, and
// no change to policy.Decide could have caught one of them, because from the
// guard's side an over-broad grant and a correct grant are the same document.
// The fix has to live above the guard, at the moment the authority is written.
//
// THE INVARIANT THIS PACKAGE SELLS.
//
//	A compiled mandate authorizes exactly what the intent states, and never a
//	paise, a call, a tool or a second more.
//
// That is enforced, not asserted. Compile derives the cumulative cap from the
// items rather than accepting one, derives expiry from a bounded window rather
// than accepting a date, and narrows allowed_tools to the read tools the intent
// actually names. Then Cover re-derives the whole grant from the intent and
// compares it against the emitted mandate, so the check does not trust the code
// that produced the thing it is checking.
//
// WHAT IT REFUSES, AND WHY REFUSING IS THE PRODUCT.
//
// Ambiguity is not resolved here, ever. An intent that says "refund the damaged
// items" without figures, or whose stated total disagrees with its lines, or
// whose percentage does not land on a whole paise, is REFUSED with the rule
// that refused it. Guessing would reintroduce, one layer up, the exact failure
// this layer exists to remove -- and it would do it silently, because a guessed
// mandate looks identical to a correct one.
//
// DELEGATION IS ALLOWED, BUT ONLY OUT LOUD. A merchant may genuinely not know
// the figure ("refund the shipping, whatever it came to"). That is expressible
// as a delegated line, and it compiles to a bounded action -- but only with an
// explicit acknowledgement and a stated reason, and the coverage record names
// every paise of discretion granted. Discretion the merchant chose is not the
// failure; discretion nobody noticed is.
//
// THERE IS NO MODEL HERE. No parsing of free text, no extraction, no scoring.
// The intent document is structured because a structured intent is the only
// kind that can be checked; turning English into authority is precisely the
// step that cannot be made auditable, and this package refuses to be the place
// it happens.
package intent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// Refusal rule identifiers. Every refusal names one, so a merchant-side tool
// can route the failure and a test can assert the reason rather than a
// substring of prose.
const (
	EmptyIntent       = "EMPTY_INTENT"
	AmbiguousAmount   = "AMBIGUOUS_AMOUNT"
	UndeclaredBound   = "UNDECLARED_BOUND"
	TotalDisagrees    = "TOTAL_DISAGREES"
	FractionalPercent = "FRACTIONAL_PERCENT"
	ExceedsCaptured   = "EXCEEDS_CAPTURED"
	DuplicateItem     = "DUPLICATE_ITEM"
	UnboundedWindow   = "UNBOUNDED_WINDOW"
	ToolWidening      = "TOOL_WIDENING"
	BelowFloor        = "BELOW_FLOOR"
	MalformedIntent   = "MALFORMED_INTENT"
	CoverageOver      = "COVERAGE_OVER"
	CoverageUnder     = "COVERAGE_UNDER"
)

// MaxWindow is the longest life a compiled mandate may have.
//
// An expiry is authority in the time dimension: a mandate valid for a month
// authorizes an agent that is prompt-injected three weeks from now. No intent
// in the evaluation corpus implies more than a working day, so the ceiling is a
// day, and an intent that wants longer has to argue it to a human rather than
// to this compiler.
const MaxWindow = 24 * time.Hour

// MinWindow stops a zero or negative window compiling into a mandate that is
// already expired when it is written -- authority that looks granted and
// refuses everything, which reads as a guard defect rather than an authoring one.
const MinWindow = time.Minute

// readOnlyTools are the only non-refund tools an intent may request.
//
// It is a SUBSET of the guard's supported surface, restated here rather than
// imported, because this package must not widen automatically when that one
// grows: a new tool appearing in the guard is a decision for a human, not a
// transitive effect on every mandate this compiler will ever write.
var readOnlyTools = map[string]struct{}{
	"fetch_payment":                      {},
	"fetch_all_payments":                 {},
	"fetch_order":                        {},
	"fetch_order_payments":               {},
	"fetch_refund":                       {},
	"fetch_all_refunds":                  {},
	"fetch_multiple_refunds_for_payment": {},
	"fetch_specific_refund_for_payment":  {},
}

var (
	itemIDPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{3,48}$`)
	intentIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{3,48}$`)
)

// Refund is what one line of an intent asks for. Exactly one figure may be set,
// and which one it is decides whether the merchant named the amount or
// deliberately delegated it.
type Refund struct {
	// ExactPaise is the figure the merchant named. The normal case, and the only
	// one that cannot over-authorize.
	ExactPaise *int64 `json:"exact_paise,omitempty"`

	// PercentOfCaptured is a figure the merchant named INDIRECTLY. It compiles
	// to an exact amount, or it is refused: a percentage that does not land on a
	// whole paise has no correct rounding, and picking one silently moves money
	// the merchant did not name.
	PercentOfCaptured *json.Number `json:"percent_of_captured,omitempty"`

	// UpToPaise is delegation: the merchant deliberately did not name the
	// figure. It requires Delegated and DelegatedBecause, and every paise of
	// headroom it grants is itemised in the coverage record.
	UpToPaise *int64 `json:"up_to_paise,omitempty"`

	Delegated        bool   `json:"delegated,omitempty"`
	DelegatedBecause string `json:"delegated_because,omitempty"`
}

// Item is one thing the merchant wants refunded.
type Item struct {
	ItemID    string `json:"item_id"`
	PaymentID string `json:"payment_id"`
	Refund    Refund `json:"refund"`

	// CapturedPaise, when given, is what the payment actually took. It is
	// OPTIONAL because the compiler must not require a provider call -- this
	// binary talks to nothing, which is what lets it run on a merchant's own
	// machine with no credentials. When present it is enforced: an intent may
	// not authorize a refund larger than the payment it refunds, and the sum of
	// an intent's lines against one payment may not exceed it either.
	CapturedPaise *int64 `json:"captured_paise,omitempty"`

	// Because is the merchant's own sentence for this line. It is never parsed.
	// It travels into the coverage record so the person approving a grant reads
	// the reason next to the paise -- the review each of the eight measured
	// misses would have failed.
	Because string `json:"because"`
}

// Intent is one merchant's complete instruction.
type Intent struct {
	IntentID string    `json:"intent_id"`
	IssuedBy string    `json:"issued_by"`
	IssuedAt time.Time `json:"issued_at"`

	// ValidFor is a DURATION, not a date. A date invites "end of month" and a
	// mandate that outlives the reason it was written for; a duration makes the
	// authority's lifetime the thing being decided.
	ValidFor Duration `json:"valid_for"`

	MaxCallsPerMinute int `json:"max_calls_per_minute"`

	// ReadTools are the read-only tools the agent needs for this job. Empty is
	// legal and means the agent may only refund.
	ReadTools []string `json:"read_tools,omitempty"`

	Items []Item `json:"items"`

	// TotalPaise, when given, is the merchant restating the total themselves. It
	// is a CHECKSUM, not an input: if it disagrees with the lines by any amount
	// the intent is refused rather than reconciled. A merchant who writes the
	// total twice and gets two answers has found a mistake, and this compiler's
	// job is to make sure it is found here rather than in a refund.
	TotalPaise *int64 `json:"total_paise,omitempty"`
}

// Duration is a Go duration string in JSON ("2h", "45m"). A string rather than
// nanoseconds because this document is read by the person who has to approve it.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("valid_for must be a duration string such as \"2h\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("valid_for %q is not a duration: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

// Error is a refusal, carrying the rule that produced it.
type Error struct {
	Rule   string
	ItemID string
	Detail string
}

func (e *Error) Error() string {
	if e.ItemID == "" {
		return fmt.Sprintf("%s: %s", e.Rule, e.Detail)
	}
	return fmt.Sprintf("%s [item %s]: %s", e.Rule, e.ItemID, e.Detail)
}

func refuse(rule, itemID, format string, a ...any) error {
	return &Error{Rule: rule, ItemID: itemID, Detail: fmt.Sprintf(format, a...)}
}

// RuleOf returns the refusal rule behind an error, or "" when it is not one.
func RuleOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Rule
	}
	return ""
}

// Load parses an intent document.
//
// Unknown fields are refused, and so is a second JSON value in the file, for
// the same reason mandate.Load refuses both: what a reviewer reads must be what
// the compiler compiles. A misspelled field that silently defaults is an
// authoring failure of exactly the class this package exists to remove.
func Load(raw []byte) (*Intent, error) {
	var in Intent
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return nil, refuse(MalformedIntent, "", "%v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, refuse(MalformedIntent, "",
			"the file carries more than one JSON value; an intent is one document, "+
				"and a second would be silently ignored")
	}
	return &in, nil
}

// line is one item after it has been resolved to a single figure. Keeping the
// figure separate from the item is what lets Cover re-derive the grant without
// re-running the resolution it is checking.
type line struct {
	item      Item
	ceiling   int64 // the most this line can ever move
	delegated bool
}

// resolve turns one item's Refund into a single figure, or refuses it.
func resolve(it Item) (line, error) {
	r := it.Refund
	set := 0
	if r.ExactPaise != nil {
		set++
	}
	if r.PercentOfCaptured != nil {
		set++
	}
	if r.UpToPaise != nil {
		set++
	}
	switch set {
	case 1:
	case 0:
		return line{}, refuse(AmbiguousAmount, it.ItemID,
			"no figure: set exactly one of exact_paise, percent_of_captured or "+
				"up_to_paise. An item with no figure cannot be compiled into "+
				"authority, and this compiler will not choose one for you")
	default:
		return line{}, refuse(AmbiguousAmount, it.ItemID,
			"%d figures given; exactly one of exact_paise, percent_of_captured or "+
				"up_to_paise may be set, because two figures is two intents", set)
	}

	switch {
	case r.ExactPaise != nil:
		return line{item: it, ceiling: *r.ExactPaise}, nil

	case r.PercentOfCaptured != nil:
		if it.CapturedPaise == nil {
			return line{}, refuse(AmbiguousAmount, it.ItemID,
				"percent_of_captured needs captured_paise on the same item; a "+
					"percentage of an unknown total is not a figure")
		}
		num, den, err := ratio(r.PercentOfCaptured.String())
		if err != nil {
			return line{}, refuse(MalformedIntent, it.ItemID, "percent_of_captured: %v", err)
		}
		// Integer arithmetic throughout. Doing this in float64 would put a
		// rounding decision inside a money path -- the very thing the refusal
		// below exists to prevent.
		product := *it.CapturedPaise * num
		divisor := den * 100
		if product%divisor != 0 {
			return line{}, refuse(FractionalPercent, it.ItemID,
				"%s%% of %d paise is %s paise, which is not whole. There is no "+
					"correct rounding of a merchant's figure, so state the exact "+
					"paise instead",
				r.PercentOfCaptured.String(), *it.CapturedPaise,
				exactQuotient(product, divisor))
		}
		return line{item: it, ceiling: product / divisor}, nil

	default: // UpToPaise
		if !r.Delegated {
			return line{}, refuse(UndeclaredBound, it.ItemID,
				"up_to_paise grants the agent discretion over the figure. That is "+
					"allowed, but it must be deliberate: set \"delegated\": true and "+
					"say why in delegated_because. Every measured miss in this "+
					"project's evaluation was a grant that authorized more than the "+
					"merchant asked for, and this is the field that admits to it")
		}
		if strings.TrimSpace(r.DelegatedBecause) == "" {
			return line{}, refuse(UndeclaredBound, it.ItemID,
				"delegated_because is required alongside delegated: the reason is "+
					"what a reviewer reads next to the headroom being granted")
		}
		return line{item: it, ceiling: *r.UpToPaise, delegated: true}, nil
	}
}

// ratio parses a decimal percentage into an exact numerator and denominator, so
// no float ever touches the calculation.
func ratio(s string) (num, den int64, err error) {
	if strings.HasPrefix(s, "-") {
		return 0, 0, fmt.Errorf("%q is negative", s)
	}
	if strings.ContainsAny(s, "eE") {
		return 0, 0, fmt.Errorf("%q uses exponent notation; write the figure plainly", s)
	}
	whole, frac, _ := strings.Cut(s, ".")
	den = 1
	for range frac {
		den *= 10
	}
	digits := whole + frac
	if digits == "" {
		return 0, 0, fmt.Errorf("%q is not a number", s)
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, 0, fmt.Errorf("%q is not a number", s)
		}
		num = num*10 + int64(c-'0')
	}
	return num, den, nil
}

// exactQuotient renders product/divisor as a decimal, for an error message
// only. Nothing it returns is ever fed back into the compile path.
func exactQuotient(product, divisor int64) string {
	whole := product / divisor
	rem := product % divisor
	if rem == 0 {
		return fmt.Sprintf("%d", whole)
	}
	return strings.TrimRight(fmt.Sprintf("%d.%06d", whole, (rem*1000000)/divisor), "0")
}
