// Command rzp-demo shows the authorization boundary doing its job, in about
// fifteen seconds, with no credentials, no network and no money.
//
// WHY IT EXISTS. Until now the fastest way to watch this system refuse a refund
// was to read two minutes of test output. That is fine for a maintainer and bad
// for anyone with five minutes who wants to see the thing work.
//
// WHAT IS REAL HERE. The merchant's authorization is compiled by
// internal/mandate. Every decision comes from internal/policy through the
// production internal/relay, driven over the same JSON-RPC lines an agent
// writes. Consumption, the cumulative cap and the durable ledger are
// internal/storage through internal/bootstrap, against a real state file in a
// temporary directory. Nothing is stubbed except the far side.
//
// WHAT IS NOT REAL. There is no Razorpay MCP server on the other end -- the
// child's stdin is a buffer this program reads back, so "forwarded" means the
// bytes reached the boundary of the guard and no further. Nothing contacts a
// payment provider, and the payment identifiers are synthetic and
// non-resolvable. This demonstrates the authorization decision, not a refund.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/harshith/rzp-guard/internal/bootstrap"
	"github.com/harshith/rzp-guard/internal/mandate"
	"github.com/harshith/rzp-guard/internal/mandateauth"
	"github.com/harshith/rzp-guard/internal/policy"
	"github.com/harshith/rzp-guard/internal/relay"
)

const (
	payA = "pay_SYNDEMO001"
	payB = "pay_SYNDEMO002"
)

// The merchant's instruction, compiled: one payment, one exact amount, once.
const mandateDoc = `{
  "mandate_id": "mnd_demo",
  "expires_at": "2027-01-01T00:00:00Z",
  "allowed_tools": ["fetch_payment", "create_refund"],
  "authorized_refund_actions": [
    {"action_id": "rfa_atta", "payment_id": "` + payA + `", "amount_paise": 24000}
  ],
  "global": {"max_cumulative_paise": 24000, "max_calls_per_minute": 60}
}`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "rzp-demo:", err)
		os.Exit(1)
	}
}

type step struct {
	narrate string
	amount  int64
	payment string
}

func run() error {
	dir, err := os.MkdirTemp("", "rzp-demo-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	m, err := mandate.Load([]byte(mandateDoc))
	if err != nil {
		return err
	}
	boot, err := bootstrap.Open(filepath.Join(dir, "demo.db"), m, time.Now().UTC())
	if err != nil {
		return err
	}
	defer boot.Close()

	child := &bytes.Buffer{} // stands in for the Razorpay MCP server's stdin
	agent := &bytes.Buffer{}
	var last policy.Decision
	r := relay.New(boot.Guard, child, agent,
		func(d policy.Decision, _ json.RawMessage) { last = d })

	rule("THE MERCHANT AUTHORIZED, ONCE")
	fmt.Printf("  %s   exactly 24000 paise   single use\n\n", payA)
	fmt.Println("  Everything below is an AI agent asking. Nothing else changes.")
	fmt.Println()

	steps := []step{
		{"the refund the merchant actually authorized", 24000, payA},
		{"the same refund again -- a replay", 24000, payA},
		{"more than was authorized", 61500, payA},
		{"the right amount, a different payment", 24000, payB},
		{"less than authorized -- a partial refund", 12000, payA},
	}

	for i, s := range steps {
		before := child.Len()
		line := fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":`+
				`{"name":"create_refund","arguments":{"payment_id":%q,"amount":%d}}}`,
			i+1, s.payment, s.amount)
		if err := r.PumpAgent(strings.NewReader(line + "\n")); err != nil {
			return err
		}
		reached := child.Len() > before

		mark := "REFUSED "
		if reached {
			mark = "ALLOWED "
		}
		fmt.Printf("  %s  agent asks %-6d on %s\n", mark, s.amount, s.payment)
		fmt.Printf("            %s\n", s.narrate)
		fmt.Printf("            rule: %s\n", last.Rule)
		if reached != last.Allowed {
			return fmt.Errorf("decision said allowed=%v but %d bytes reached the "+
				"child: the demo is not showing what actually happened",
				last.Allowed, child.Len()-before)
		}
		fmt.Println()
	}

	rule("AND THE AUTHORIZATION ITSELF")
	if err := signingDemo(dir); err != nil {
		return err
	}

	rule("WHAT YOU JUST SAW")
	fmt.Println("  One authorization was spent once. Every other request was refused")
	fmt.Println("  before it reached the provider -- including the partial refund,")
	fmt.Println("  which is a REAL COST of exact matching and is priced in")
	fmt.Println("  study/FP-COST.md rather than hidden.")
	fmt.Println()
	fmt.Println("  The agent was never asked to behave. It could not exceed the")
	fmt.Println("  authority it was given, because the check is outside it.")
	fmt.Println()
	fmt.Println("  Not shown: a real provider. The far side is a buffer, the ids are")
	fmt.Println("  synthetic, and no money exists. This is the decision, not a refund.")
	return nil
}

// signingDemo shows the boundary above the guard: a mandate the merchant did
// not sign, or signed and someone then edited, is refused before it is parsed.
func signingDemo(dir string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "mandate.json")
	body := []byte(mandateDoc)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(mandateauth.SigPath(path),
		[]byte(mandateauth.Sign(body, priv)), 0o600); err != nil {
		return err
	}

	if _, err := mandateauth.Verify(path, body, pub); err != nil {
		return fmt.Errorf("a correctly signed mandate was refused: %w", err)
	}
	fmt.Println("  ACCEPTED  the merchant's signed authorization")
	fmt.Println("            ed25519 over the exact bytes, checked before parsing")
	fmt.Println()

	// Someone with write access raises the amount and does not re-sign.
	tampered := bytes.Replace(body, []byte(`"amount_paise": 24000`),
		[]byte(`"amount_paise": 9900000`), 1)
	if bytes.Equal(tampered, body) {
		return fmt.Errorf("the demo failed to alter the mandate, so it is not " +
			"demonstrating anything")
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		return err
	}
	err = func() error { _, e := mandateauth.Verify(path, tampered, pub); return e }()
	if err == nil {
		return fmt.Errorf("an altered mandate verified: the demo would be showing " +
			"a guarantee that does not hold")
	}
	fmt.Println("  REFUSED   the same authorization, amount raised to 9900000")
	fmt.Println("            the guard does not start; it does not enforce a")
	fmt.Println("            grant it cannot attribute to the merchant")
	fmt.Println()
	fmt.Println("            Opt-in: without -mandate-pubkey the guard trusts the")
	fmt.Println("            file and says so. And this authenticates the file,")
	fmt.Println("            not the human -- a stolen key still issues mandates.")
	fmt.Println()
	return nil
}

func rule(title string) {
	fmt.Println(strings.Repeat("=", 68))
	fmt.Println("  " + title)
	fmt.Println(strings.Repeat("=", 68))
	fmt.Println()
}
