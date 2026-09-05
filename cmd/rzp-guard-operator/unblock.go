package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/harshith/rzp-guard/internal/opauth"
	"github.com/harshith/rzp-guard/internal/opgrant"
	"github.com/harshith/rzp-guard/internal/storage"
)

// THE FALSE-POSITIVE PATH.
//
// The guard's published false-positive rate is 0.455: it refuses roughly
// forty-five percent of the legitimate refunds it sees. The project's own cost
// model prices those refusals on the assumption that a human unblocks them, and
// section 7 of study/FP-COST.md says plainly that nothing implements it. This
// file is that half.
//
// It is three commands and one rule about who may run them.
//
//	queue     what the guard refused, deduplicated, oldest decision first
//	approve   issue a single-use grant for one of those refusals
//	decline   record that a human looked and agreed with the refusal
//
// decline is not decoration. Without it, a queue can only distinguish
// "approved" from "not yet read", and the difference between a backlog nobody
// is working and a backlog that has been worked correctly is the whole
// operational question.

// cmdQueue shows what the guard refused.
func cmdQueue(store *storage.Store, all bool, resolution string, asJSON bool) error {
	rows, err := store.Denials(resolution, all)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(rows)
	}
	if len(rows) == 0 {
		if resolution == storage.DenialOpen {
			fmt.Println("No refused refunds are waiting for a decision.")
			fmt.Println("  (this is the queue the false-positive cost model assumes " +
				"somebody is working; an empty one means either the guard refused " +
				"nothing or nobody has run an agent against it)")
		} else {
			fmt.Println("Nothing recorded.")
		}
		return nil
	}

	fmt.Printf("%-6s %-24s %-22s %10s  %-22s %5s  %s\n",
		"ID", "RULE", "PAYMENT", "PAISE", "LAST SEEN", "TRIES", "STATE")
	for _, d := range rows {
		// Agent-controlled content. The payment id came straight off the wire and
		// the reason embeds it; both are clipped here and neither is interpreted.
		fmt.Printf("%-6d %-24s %-22s %10d  %-22s %5d  %s\n",
			d.ID, clipField(d.Rule, 24), clipField(d.PaymentID, 22), d.AmountPaise,
			clipField(d.LastAt, 22), d.Occurrences, d.Resolution)
		if all {
			fmt.Printf("       mandate: %s\n", clipField(d.MandateID, 64))
		}
		fmt.Printf("       %s\n", clipField(d.Reason, 150))
	}
	fmt.Printf("\n%d refusal(s). To unblock one:\n", len(rows))
	fmt.Println("  rzp-guard-operator -mandate M -state S approve <id> \\")
	fmt.Println("      -operator <who> -reason <what you checked>")
	fmt.Println("To record that a refusal was correct:")
	fmt.Println("  rzp-guard-operator -mandate M -state S decline <id> \\")
	fmt.Println("      -operator <who> -reason <why>")
	return nil
}

// cmdApprove issues a single-use grant for one refused refund.
//
// It takes the payment and the amount FROM THE RECORDED REFUSAL, never from a
// flag. That is what keeps this from being a general "authorize any refund"
// command: an operator can only approve something the guard actually refused,
// so a stolen operator token cannot mint authority for a payment the agent
// never touched. It is also why there is no -amount flag to get wrong at 3am.
func cmdApprove(store *storage.Store, g opauth.Grant, idArg, reason string,
	ttl time.Duration, lease storage.Lease) error {

	id, err := parseDenialID(idArg)
	if err != nil {
		return err
	}
	if reason == "" {
		return errors.New("-reason is required: it is what a later reviewer reads " +
			"to decide whether this grant should have been issued")
	}
	grant, err := store.IssueGrant(g, id, ttl, reason)
	if err != nil {
		return err
	}

	fmt.Printf("Issued %s\n", grant.Describe())
	fmt.Printf("  it authorizes exactly %d paise on %s, once, and expires %s from now.\n",
		grant.AmountPaise, grant.PaymentID,
		time.Until(grant.ExpiresAt).Truncate(time.Second))
	if lease.Live {
		fmt.Printf("  The guard (pid %d on %s) reads grants from the state file on the\n"+
			"  refusal path, so it will honour this within about a second. The agent\n"+
			"  should retry the same refund.\n", lease.PID, lease.Host)
	} else {
		fmt.Println("  No guard currently holds this mandate. The grant is durable and " +
			"will be honoured by the next one to start, if it has not expired by then.")
	}
	fmt.Println("\n  It cannot exceed the merchant's cumulative cap or rate limit: a")
	fmt.Println("  grant is reserved through the same ledger as any mandate action, so")
	fmt.Println("  those ceilings still refuse it. Raising them needs the merchant.")
	return nil
}

// cmdDecline records that a human read a refusal and agreed with it.
func cmdDecline(store *storage.Store, g opauth.Grant, idArg, reason string) error {
	id, err := parseDenialID(idArg)
	if err != nil {
		return err
	}
	if reason == "" {
		return errors.New("-reason is required")
	}
	if err := store.DeclineDenial(g, id, reason); err != nil {
		return err
	}
	fmt.Printf("Denial %d marked DECLINED. The guard's refusal stands, and the "+
		"record now says a person agreed with it rather than that nobody looked.\n", id)
	return nil
}

func parseDenialID(s string) (int64, error) {
	if s == "" {
		return 0, errors.New("which denial? Run `queue` for the ids")
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%q is not a denial id; run `queue` for the ids", s)
	}
	return id, nil
}

// clipField bounds any field that can carry agent-supplied content, so a
// hostile argument cannot destroy the layout of the one screen an operator
// reads during an incident. Same reasoning as clip() in the guard's decision
// log: JSON encoding stops syntax injection, it does not make content
// trustworthy.
func clipField(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// parseTTL reads the requested grant lifetime, bounded at both ends.
func parseTTL(s string) (time.Duration, error) {
	if s == "" {
		return opgrant.DefaultTTL, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("-ttl %q is not a duration such as \"15m\": %w", s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("-ttl %s would be expired on arrival", d)
	}
	if d > opgrant.MaxTTL {
		return 0, fmt.Errorf("-ttl %s is beyond the %s ceiling; a grant that "+
			"outlives the incident is standing authority nobody revisits, and it is "+
			"invisible in the mandate a reviewer reads", d, opgrant.MaxTTL)
	}
	return d, nil
}
