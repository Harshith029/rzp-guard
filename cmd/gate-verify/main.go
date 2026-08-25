// Command gate-verify turns the live gates into hard assertions.
//
// The gates previously printed their controls and exited 0 whenever the blocked
// call was absent from the child tee. That passes when the container is dead or
// the credentials are invalid -- exactly the cases the control exists to rule
// out. Every condition below is parsed as JSON and enforced.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type rpc struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"params"`
	Result *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
	Error json.RawMessage `json:"error"`
}

func readJSONL(path string) ([]rpc, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []rpc
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m rpc
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			return nil, fmt.Errorf("%s: unparseable line: %w", path, err)
		}
		out = append(out, m)
	}
	return out, sc.Err()
}

func id(m rpc) string { return strings.TrimSpace(string(m.ID)) }

func find(ms []rpc, want string) (rpc, bool) {
	for _, m := range ms {
		if id(m) == want {
			return m, true
		}
	}
	return rpc{}, false
}

func text(m rpc) string {
	if m.Result == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range m.Result.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

var failures []string

func check(ok bool, format string, args ...any) {
	status := "\033[32mPASS\033[0m"
	if !ok {
		status = "\033[31mFAIL\033[0m"
		failures = append(failures, fmt.Sprintf(format, args...))
	}
	fmt.Printf("  [%s] %s\n", status, fmt.Sprintf(format, args...))
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: gate-verify (block|recover|refund) <dir>")
		os.Exit(2)
	}
	mode, dir := os.Args[1], "evidence/live"
	if len(os.Args) > 2 {
		dir = os.Args[2]
	}
	switch mode {
	case "block":
		verifyBlock(dir)
	case "recover":
		verifyRecover(dir)
	case "refund":
		verifyRefund(dir)
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", mode)
		os.Exit(2)
	}
	if len(failures) > 0 {
		fmt.Printf("\n\033[31m%d assertion(s) failed\033[0m\n", len(failures))
		os.Exit(1)
	}
	fmt.Printf("\n\033[32mall assertions passed\033[0m\n")
}

func verifyBlock(dir string) {
	tee, err := readJSONL(dir + "/block_child_stdin.jsonl")
	must(err)
	out, err := readJSONL(dir + "/block_stdout.jsonl")
	must(err)

	fmt.Println("Live block gate")

	// 1. The blocked call never crossed the boundary.
	var forwarded []rpc
	moneyMoving := 0
	for _, m := range tee {
		if m.Method != "tools/call" {
			continue
		}
		forwarded = append(forwarded, m)
		switch m.Params.Name {
		case "create_refund", "create_instant_settlement", "initiate_payment",
			"capture_payment", "create_payment_link", "create_payment_link_upi",
			"payment_link_upi_create", "create_registration_link", "revoke_token":
			moneyMoving++
		}
	}
	check(moneyMoving == 0,
		"NO money-moving tools/call of any kind reached child stdin (found %d)", moneyMoving)

	// Bind the control to the EXACT request, not to "at least one call". A
	// weaker proxy could be satisfied by some other forwarded call while the
	// intended control never happened.
	check(len(forwarded) == 1,
		"exactly one tools/call was forwarded (found %d)", len(forwarded))
	if len(forwarded) == 1 {
		check(forwarded[0].Params.Name == "fetch_all_payments",
			"the forwarded call is the expected control read fetch_all_payments (got %q)",
			forwarded[0].Params.Name)
		check(id(forwarded[0]) == "4",
			"the forwarded control read carries id 4 (got %s)", id(forwarded[0]))
	}

	// 2. The guard answered the blocked id with a readable denial.
	blocked, ok := find(out, "3")
	check(ok, "guard produced a response for the blocked id 3")
	if ok {
		check(blocked.Result != nil && blocked.Result.IsError,
			"blocked response is flagged isError")
		t := text(blocked)
		check(strings.Contains(t, "BLOCKED by rzp-guard"),
			"blocked response names rzp-guard as the blocker")
		check(strings.Contains(t, "NO_AUTHORIZED_ACTION"),
			"blocked response carries the deciding rule NO_AUTHORIZED_ACTION")
	}

	// 2b. The gate must not leave a real recovery credential lying around. This
	//     tree is OneDrive-backed, so a gitignored file still syncs to the cloud;
	//     two such tokens were found sitting in evidence/live before this check.
	if _, err := os.Stat(dir + "/block_operator_token"); !os.IsNotExist(err) {
		check(false, "no recovery token was ever written into the repo tree (found %s)",
			dir+"/block_operator_token")
	} else {
		check(true, "no recovery token was ever written into the repo tree")
	}

	// 3. THE CONTROL. Without this the gate passes against a dead container or
	//    invalid credentials, which is precisely what it exists to rule out.
	control, ok := find(out, "4")
	check(ok, "CONTROL: real container produced a response for the allowed read id 4")
	if ok {
		check(len(control.Error) == 0,
			"CONTROL: read response carries no JSON-RPC error")
		check(control.Result != nil && !control.Result.IsError,
			"CONTROL: read response is a success, not a tool error")
		body := text(control)
		var entity map[string]any
		parsed := json.Unmarshal([]byte(body), &entity) == nil
		check(parsed, "CONTROL: read response body is a JSON entity from the Razorpay API")
		if parsed {
			_, hasEntity := entity["entity"]
			check(hasEntity,
				"CONTROL: response carries an \"entity\" field, so it came from the API "+
					"rather than being synthesised by the guard")
		}
		check(len(forwarded) == 1 && id(forwarded[0]) == "4",
			"CONTROL: the response on id 4 corresponds to the exact call forwarded")
	}
}

func verifyRecover(dir string) {
	tee, err := readJSONL(dir + "/recover_child_stdin.jsonl")
	must(err)
	out, err := readJSONL(dir + "/recover_stdout.jsonl")
	must(err)
	restart, err := readJSONL(dir + "/recover_restart.jsonl")
	must(err)
	stderrBytes, err := os.ReadFile(dir + "/recover_stderr.txt")
	must(err)

	fmt.Println("Process recovery gate")

	refunds := 0
	for _, m := range tee {
		if m.Method == "tools/call" && m.Params.Name == "create_refund" {
			refunds++
		}
	}
	check(refunds == 1, "the approved refund WAS forwarded to the child (%d)", refunds)

	_, replied := find(out, "9")
	check(!replied, "the child never answered it, so the outcome is genuinely ambiguous")

	check(strings.Contains(string(stderrBytes), "marked IN_DOUBT"),
		"cleanup ran and marked the reservation IN_DOUBT")
	check(strings.Contains(string(stderrBytes), "child stdout closed") ||
		strings.Contains(string(stderrBytes), "agent stdin closed") ||
		strings.Contains(string(stderrBytes), "interrupted"),
		"cleanup names the process boundary that fired it")

	// After a restart the lock must still hold.
	found := false
	for _, m := range restart {
		t := text(m)
		if strings.Contains(t, "ACTION_CONSUMED") && strings.Contains(t, "IN_DOUBT") {
			found = true
		}
	}
	check(found, "after restart a fresh process still refuses the replay (ACTION_CONSUMED, IN_DOUBT)")

	if _, err := os.Stat(dir + "/recover_operator_token"); !os.IsNotExist(err) {
		check(false, "no recovery token was ever written into the repo tree (found %s)",
			dir+"/recover_operator_token")
	} else {
		check(true, "no recovery token was ever written into the repo tree")
	}
}

// verifyRefund is the allow-path gate (G1.6). The block gate proves an
// unauthorized refund never reaches the provider; this proves the converse --
// that an AUTHORIZED one does, that the guard's own idempotency token survives
// the round trip, and that the action is then consumed.
//
// It needs a real captured Test Mode payment, so unlike the block gate it
// cannot run against a synthetic id.
func verifyRefund(dir string) {
	tee, err := readJSONL(dir + "/refund_child_stdin.jsonl")
	must(err)
	out, err := readJSONL(dir + "/refund_stdout.jsonl")
	must(err)
	replayTee, err := readJSONL(dir + "/replay_child_stdin.jsonl")
	must(err)
	replayOut, err := readJSONL(dir + "/replay_stdout.jsonl")
	must(err)

	fmt.Println("Live allow-path gate (G1.6)")

	// What the guard actually handed the provider.
	var fwd []rpc
	for _, m := range tee {
		if m.Method == "tools/call" && m.Params.Name == "create_refund" {
			fwd = append(fwd, m)
		}
	}
	check(len(fwd) == 1, "exactly one create_refund reached the child (saw %d)", len(fwd))
	if len(fwd) != 1 {
		return
	}

	var args struct {
		PaymentID string      `json:"payment_id"`
		Amount    json.Number `json:"amount"`
		Receipt   string      `json:"receipt"`
	}
	must(json.Unmarshal(fwd[0].Params.Arguments, &args))

	// The receipt is injected by the guard. The agent never sends one, so its
	// presence in the forwarded call is what binds this dispatch to one
	// authorized action.
	check(strings.HasPrefix(args.Receipt, "rzpg_"), "guard injected a receipt (%q)", args.Receipt)
	check(len(args.Receipt) >= len("rzpg_")+12,
		"injected receipt clears the 12-hex-digit floor (%q, %d chars after prefix)",
		args.Receipt, len(args.Receipt)-len("rzpg_"))

	// create_refund declares amount as type:number, so a fractional value is
	// accepted by the schema. The guard must forward a canonical integer.
	if _, ferr := args.Amount.Int64(); ferr != nil {
		check(false, "forwarded amount is a canonical integer (got %q)", args.Amount.String())
	} else {
		check(!strings.ContainsAny(args.Amount.String(), ".eE"),
			"forwarded amount is a canonical integer (%s paise)", args.Amount.String())
	}

	// The provider's answer.
	var resp rpc
	for _, m := range out {
		if m.Result != nil && strings.Contains(text(m), "\"entity\":\"refund\"") {
			resp = m
		}
	}
	if resp.Result == nil {
		check(false, "provider returned a refund entity")
		return
	}
	check(!resp.Result.IsError, "provider accepted the authorized refund (not an error)")

	var ent struct {
		ID        string      `json:"id"`
		Entity    string      `json:"entity"`
		PaymentID string      `json:"payment_id"`
		Amount    json.Number `json:"amount"`
		Receipt   string      `json:"receipt"`
		Status    string      `json:"status"`
	}
	must(json.Unmarshal([]byte(text(resp)), &ent))

	check(strings.HasPrefix(ent.ID, "rfnd_"),
		"provider assigned a real refund id (%s)", ent.ID)
	check(ent.PaymentID == args.PaymentID,
		"refund is against the authorized payment (%s)", ent.PaymentID)
	check(ent.Amount.String() == args.Amount.String(),
		"provider refunded exactly the authorized amount (%s paise)", ent.Amount.String())

	// The round trip that matters: the token the guard minted comes back
	// unchanged, so the provider-side record is bound to our authorization.
	check(ent.Receipt == args.Receipt,
		"guard's receipt survived the round trip unchanged (%s)", ent.Receipt)

	// Honest scope: a synchronous reply cannot prove settlement.
	check(ent.Status != "",
		"envelope carries a status (%q) -- COMMITTED means the entity was created, not that money settled",
		ent.Status)

	// The action is single-use. A second identical request must die at the guard.
	blocked := false
	for _, m := range replayOut {
		if strings.Contains(text(m), "ACTION_CONSUMED") {
			blocked = true
		}
	}
	check(blocked, "replay of the same authorized action is refused (ACTION_CONSUMED)")

	replayFwd := 0
	aliveID := ""
	for _, m := range replayTee {
		if m.Method != "tools/call" {
			continue
		}
		if m.Params.Name == "create_refund" {
			replayFwd++
			continue
		}
		// The alive control is a NAMED read, correlated by request id below.
		//
		// An earlier version counted any non-refund tools/call and looked for a
		// payment entity anywhere in the output. Those two halves were never
		// tied together, so renaming the forwarded tool to garbage still passed:
		// the unrelated reply kept the second half true. Mutation testing the
		// gate itself is what surfaced it.
		if m.Params.Name == "fetch_payment" {
			aliveID = id(m)
		}
	}
	check(replayFwd == 0, "the replay reached the provider zero times (saw %d)", replayFwd)

	// Without this, "nothing was forwarded" would also pass against a dead
	// container or bad credentials -- the very cases the gate exists to exclude.
	check(aliveID != "", "alive-control: a permitted fetch_payment did reach the child")
	if aliveID == "" {
		return
	}
	reply, ok := find(replayOut, aliveID)
	check(ok && reply.Result != nil && !reply.Result.IsError &&
		strings.Contains(text(reply), "\"entity\":\"payment\""),
		"alive-control: request %s came back as a real payment entity from the provider", aliveID)
}

func must(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "gate-verify: %v\n", err)
		os.Exit(2)
	}
}
