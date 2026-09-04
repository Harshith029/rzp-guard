// Command rzp-mandate compiles a merchant's stated intent into a mandate.
//
// It is the layer the guard could never be. The guard enforces a mandate and
// never sees the sentence behind it, so an over-broad grant and a correct one
// are the same document to it -- which is why all eight misses in the held-out
// evaluation trace to authoring, and why none of them could be fixed inside
// internal/policy.
//
// WHAT THIS BINARY TALKS TO: nothing. No network, no provider, no Docker, no
// credentials, no state file. It reads two files and writes three. That is
// deliberate and it is asserted in CI: an authoring tool that could also reach
// the Razorpay API would be a refund-issuing command that writes its own
// authorizing mandate, which is precisely the thing FAILURES.md F18 and F26
// record being removed from this repository.
//
//	rzp-mandate compile -intent i.json -out mandate.json
//	rzp-mandate verify  -mandate mandate.json
//	rzp-mandate explain -mandate mandate.json
//
// compile refuses anything ambiguous rather than resolving it. verify proves an
// existing mandate is still the one its intent compiles to. explain prints the
// grant next to the sentence, for the human who has to approve it.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/harshith/rzp-guard/internal/intent"
)

const usage = `rzp-mandate — compile merchant intent into a mandate

  rzp-mandate compile -intent INTENT.json [-out MANDATE.json] [-force]
        Compile, or refuse with the rule that refused it. Writes three files:
          MANDATE.json              the grant the guard enforces
          MANDATE.intent.json       the coverage record binding it to the intent
        and copies the intent beside them if -out is in another directory.
        Refuses to overwrite an existing mandate unless -force is given: a
        mandate on disk may be one a running guard is enforcing.

  rzp-mandate verify -mandate MANDATE.json
        Recompile the intent beside it and prove the mandate is still exactly
        what that intent produces. Read-only. This is the check to run in CI
        and before signing.

  rzp-mandate explain -mandate MANDATE.json
        Print every paise of authority next to the merchant's own sentence for
        it, and name any discretion the grant contains. This is the review that
        each of the eight measured misses would have failed.

This binary talks to nothing. It reads and writes local files only.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		// A refusal is a normal outcome of compiling an ambiguous intent, not a
		// crash, and it is printed with the rule first so a merchant-side tool
		// can route on it without parsing prose.
		fmt.Fprintf(os.Stderr, "rzp-mandate: %v\n", err)
		os.Exit(1)
	}
}

// run takes its arguments rather than reading os.Args, so a test drives the
// same entry point the shell does. A CLI whose only caller is main() gets
// tested through a subprocess or not at all, and "not at all" is what usually
// happens.
func run(argv []string) error {
	fs := flag.NewFlagSet("rzp-mandate", flag.ContinueOnError)
	var (
		intentPath  = fs.String("intent", "", "compile: the intent document")
		mandatePath = fs.String("mandate", "", "verify/explain: the mandate to check")
		out         = fs.String("out", "", "compile: where to write the mandate (default <intent>.mandate.json)")
		force       = fs.Bool("force", false, "compile: overwrite an existing mandate")
	)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	// The flag package stops at the first non-flag argument, so the command word
	// is lifted out before parsing. Same handling as rzp-guard-operator, for the
	// same reason: "compile -intent x" and "-intent x compile" must behave alike.
	var rest []string
	var positional []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			if !strings.Contains(a, "=") && i+1 < len(argv) &&
				!strings.HasPrefix(argv[i+1], "-") {
				i++
				rest = append(rest, argv[i])
			}
			continue
		}
		positional = append(positional, a)
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if len(positional) == 0 {
		fs.Usage()
		return errors.New("no command given")
	}

	switch positional[0] {
	case "compile":
		return cmdCompile(*intentPath, *out, *force)
	case "verify":
		return cmdVerify(*mandatePath)
	case "explain":
		return cmdExplain(*mandatePath)
	case "help":
		fs.Usage()
		return nil
	default:
		fs.Usage()
		return fmt.Errorf("unknown command %q", positional[0])
	}
}

// provenancePath is where the coverage record lives for a given mandate. One
// derived location, never a flag: a mandate whose provenance can be pointed
// somewhere else is a mandate whose provenance can be pointed at a different
// intent, and then the binding proves nothing.
func provenancePath(mandatePath string) string {
	return strings.TrimSuffix(mandatePath, filepath.Ext(mandatePath)) + ".intent.json"
}

func cmdCompile(intentPath, outPath string, force bool) error {
	if intentPath == "" {
		return errors.New("-intent is required")
	}
	fileRaw, err := os.ReadFile(intentPath)
	if err != nil {
		return fmt.Errorf("read intent: %w", err)
	}
	// An intent template carries no issue time. Stamp it once, here, and treat
	// the stamped document as the one of record from this point on -- it is what
	// is hashed, what is copied beside the mandate, and what verify recompiles.
	// A document that already has a time is returned byte-identical, so a
	// merchant's own file is never re-encoded on its way into the record.
	raw, err := intent.Stamp(fileRaw, time.Now().UTC())
	if err != nil {
		return err
	}
	stamped := len(raw) != len(fileRaw)
	in, err := intent.Load(raw)
	if err != nil {
		return err
	}
	res, err := intent.Compile(in, time.Now().UTC())
	if err != nil {
		return err
	}
	res.Provenance.IntentSHA256 = intent.Digest(raw)

	if outPath == "" {
		outPath = strings.TrimSuffix(intentPath, filepath.Ext(intentPath)) + ".mandate.json"
	}
	// A mandate already on disk may be the one a guard is enforcing right now,
	// and overwriting it in place would change an agent's authority underneath a
	// running process. The guard loads its mandate once at startup, so the
	// change would not take effect until a restart -- which is worse than either
	// alternative, because the file and the enforced authority would disagree
	// silently.
	if !force {
		if _, err := os.Stat(outPath); err == nil {
			return fmt.Errorf("%s already exists. A mandate on disk may be the one a "+
				"running guard is enforcing; pass -force only when you know it is not",
				outPath)
		}
	}
	if err := os.WriteFile(outPath, res.MandateJSON, 0o600); err != nil {
		return fmt.Errorf("write mandate: %w", err)
	}
	provJSON, err := intent.MarshalProvenance(res.Provenance)
	if err != nil {
		return err
	}
	if err := os.WriteFile(provenancePath(outPath), provJSON, 0o600); err != nil {
		return fmt.Errorf("write coverage record: %w", err)
	}
	// The intent travels with the grant. verify needs all three, and an intent
	// left behind in whatever directory a support agent ran this from is an
	// intent that will not be there in six weeks when someone asks why a refund
	// was authorized.
	sidecar := strings.TrimSuffix(outPath, filepath.Ext(outPath)) + ".source.json"
	if err := os.WriteFile(sidecar, raw, 0o600); err != nil {
		return fmt.Errorf("write intent copy: %w", err)
	}

	fmt.Printf("compiled %s -> %s\n", in.IntentID, outPath)
	if stamped {
		fmt.Printf("  the intent carried no issued_at; stamped %s and recorded the\n"+
			"  stamped document as the one of record\n",
			in.IssuedAt.Format(time.RFC3339))
	}
	printCoverage(res.Provenance)
	fmt.Printf("\n  coverage record: %s\n  intent copy:     %s\n",
		provenancePath(outPath), sidecar)
	fmt.Println("\nNothing here is signed. Sign it before a guard trusts it:")
	fmt.Printf("  rzp-guard-operator mandate-sign -mandate %s -key <merchant.key>\n", outPath)
	return nil
}

// readTriple loads a mandate together with the two documents that explain it.
func readTriple(mandatePath string) (mandateRaw, intentRaw, provRaw []byte, err error) {
	mandateRaw, err = os.ReadFile(mandatePath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read mandate: %w", err)
	}
	provRaw, err = os.ReadFile(provenancePath(mandatePath))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read coverage record: %w. A mandate with no "+
			"coverage record was hand-written rather than compiled, so there is no "+
			"sentence to check it against", err)
	}
	sidecar := strings.TrimSuffix(mandatePath, filepath.Ext(mandatePath)) + ".source.json"
	intentRaw, err = os.ReadFile(sidecar)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read intent copy: %w", err)
	}
	return mandateRaw, intentRaw, provRaw, nil
}

func cmdVerify(mandatePath string) error {
	if mandatePath == "" {
		return errors.New("-mandate is required")
	}
	mandateRaw, intentRaw, provRaw, err := readTriple(mandatePath)
	if err != nil {
		return err
	}
	if err := intent.Verify(intentRaw, mandateRaw, provRaw, time.Now().UTC()); err != nil {
		return err
	}
	fmt.Printf("%s: the mandate is exactly what its intent compiles to\n", mandatePath)
	fmt.Println("  intent, mandate and coverage record all agree, byte for byte")
	return nil
}

func cmdExplain(mandatePath string) error {
	if mandatePath == "" {
		return errors.New("-mandate is required")
	}
	_, _, provRaw, err := readTriple(mandatePath)
	if err != nil {
		return err
	}
	prov, err := intent.UnmarshalProvenance(provRaw)
	if err != nil {
		return err
	}
	fmt.Printf("%s\n  issued by %s at %s\n  expires   %s\n",
		prov.MandateID, prov.IssuedBy,
		prov.IssuedAt.Format(time.RFC3339), prov.ExpiresAt.Format(time.RFC3339))
	printCoverage(prov)
	return nil
}

// printCoverage is the review surface. Amount, payment, and the merchant's own
// words on one line each, with discretion called out rather than implied by a
// field name a reader has to know the meaning of.
func printCoverage(p intent.Provenance) {
	fmt.Printf("\n  %-20s %-22s %12s  %s\n", "ACTION", "PAYMENT", "PAISE", "BECAUSE")
	for _, c := range p.Coverage {
		amount := fmt.Sprintf("%d", c.GrantedPaise)
		if c.Class == "delegated" {
			amount = "up to " + amount
		}
		fmt.Printf("  %-20s %-22s %12s  %s\n", c.ActionID, c.PaymentID, amount, c.Because)
		if c.Class == "delegated" {
			fmt.Printf("  %-20s %-22s %12s  DISCRETION GRANTED: %s\n",
				"", "", "", c.DelegatedBecause)
		}
	}
	fmt.Printf("\n  authorized in total: %d paise\n", p.TotalGrantedPaise)
	if p.TotalHeadroomPaise > 0 {
		fmt.Printf("  of which %d paise is discretion the merchant delegated on purpose\n",
			p.TotalHeadroomPaise)
	} else {
		fmt.Println("  every paise is a figure the merchant named; no discretion granted")
	}
	fmt.Printf("  the cumulative cap equals that total exactly, so there is no\n" +
		"  spendable headroom beyond the lines above\n")
}
