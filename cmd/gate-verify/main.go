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
		Name string `json:"name"`
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
		fmt.Fprintln(os.Stderr, "usage: gate-verify (block|recover) <dir>")
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
}

func must(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "gate-verify: %v\n", err)
		os.Exit(2)
	}
}
