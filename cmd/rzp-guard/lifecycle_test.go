//go:build testhook

// Process-lifecycle tests. Build-tagged so they compile only alongside the
// test-hook child; the shipped binary has no arbitrary-child path.
//
// These exist because a previous revision ran PumpAgent on the main goroutine.
// Cleanup fired correctly on child death and on SIGTERM, but main stayed
// blocked reading agent stdin, so the process outlived its own shutdown. State
// cleanup beginning is not the same as the process lifecycle being controlled,
// and only a test that HOLDS STDIN OPEN can tell the two apart.
//
// Measured against the pre-fix binary under this exact shape: 30s, bounded only
// by the test's own feeder. With a real client holding the stream it would be
// unbounded.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// buildTestHook builds the test-hook binary once per test.
func buildTestHook(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("needs sh for the stub child; canonical runner is the golang container")
	}
	bin := filepath.Join(t.TempDir(), "rzp-guard-testhook")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-tags", "testhook", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build test-hook binary: %v\n%s", err, out)
	}
	return bin
}

func writeMandate(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mandate.json")
	doc := `{
	  "mandate_id": "mnd_lifecycle",
	  "expires_at": "2030-01-01T00:00:00Z",
	  "allowed_tools": ["fetch_payment", "create_refund"],
	  "authorized_refund_actions": [
	    {"action_id": "rfa_life_001", "payment_id": "pay_SYN00000000001", "amount_paise": 50000}
	  ],
	  "global": {"max_cumulative_paise": 200000, "max_calls_per_minute": 10}
	}`
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// startWithHeldStdin launches the guard with a stdin pipe the test NEVER
// closes, so the only way the process can exit is a boundary other than agent
// stdin EOF.
func startWithHeldStdin(t *testing.T, bin, childCmd string, feed string) (*exec.Cmd, *os.File) {
	t.Helper()
	cmd := exec.Command(bin,
		"-mandate", writeMandate(t),
		"-state", filepath.Join(t.TempDir(), "state.db"))
	cmd.Env = append(os.Environ(),
		"RZP_GUARD_CHILD_CMD="+childCmd,
		"RAZORPAY_KEY_ID=rzp_test_stub",
		"RAZORPAY_KEY_SECRET=stub",
	)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdin = r
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	if feed != "" {
		if _, err := w.WriteString(feed); err != nil {
			t.Fatal(err)
		}
	}
	return cmd, w // w deliberately left open
}

func waitWithin(t *testing.T, cmd *exec.Cmd, d time.Duration) (error, bool) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err, true
	case <-time.After(d):
		_ = cmd.Process.Kill()
		<-done
		return nil, false
	}
}

const refundLine = `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":` +
	`{"name":"create_refund","arguments":{"payment_id":"pay_SYN00000000001","amount":50000}}}` + "\n"

// A dead child with an open agent stream must not leave a hung proxy.
func TestChildExitTerminatesTheProcessWithStdinHeldOpen(t *testing.T) {
	bin := buildTestHook(t)
	cmd, stdin := startWithHeldStdin(t, bin, "head -c 120 > /dev/null; exit 0", refundLine)
	defer stdin.Close()

	_, exited := waitWithin(t, cmd, 15*time.Second)
	if !exited {
		t.Fatal("guard did not exit after the child died, with agent stdin still open: " +
			"cleanup ran but the process lifecycle is not controlled")
	}
}

// The same applies after a termination signal.
func TestSignalTerminatesTheProcessWithStdinHeldOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Interrupt cannot be delivered to another process on Windows")
	}
	bin := buildTestHook(t)
	cmd, stdin := startWithHeldStdin(t, bin, "cat > /dev/null", "")
	defer stdin.Close()

	time.Sleep(1500 * time.Millisecond) // let the child come up
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if _, exited := waitWithin(t, cmd, 15*time.Second); !exited {
		t.Fatal("guard did not exit after SIGINT with agent stdin still open")
	}
}

// A child that fails on its own must not be reported as a clean run. Discarding
// child.Wait()'s error let the CLI exit zero after the pinned container crashed
// or had its credentials rejected.
func TestChildFailurePropagatesNonZeroExit(t *testing.T) {
	bin := buildTestHook(t)
	cmd, stdin := startWithHeldStdin(t, bin, "echo boom >&2; exit 3", "")
	defer stdin.Close()

	err, exited := waitWithin(t, cmd, 15*time.Second)
	if !exited {
		t.Fatal("guard did not exit after the child failed")
	}
	if err == nil {
		t.Fatal("guard exited zero after the child failed with status 3")
	}
}

// A child that fails DURING the drain window must not be written off as a clean
// parent-initiated shutdown.
//
// When the agent closes stdin first, the guard closes the child's stdin and
// drains for replies still in flight. It classifies that route as
// parent-initiated, which previously suppressed the child's exit status — so a
// container whose credentials were rejected could exit non-zero in that window
// and the CLI would still report success.
func TestChildFailureDuringDrainIsNotSuppressed(t *testing.T) {
	bin := buildTestHook(t)

	// Agent stdin closes immediately (empty feed, pipe closed), then the child
	// exits non-zero shortly after — inside the drain window.
	cmd := exec.Command(bin,
		"-mandate", writeMandate(t),
		"-state", filepath.Join(t.TempDir(), "state.db"))
	cmd.Env = append(os.Environ(),
		"RZP_GUARD_CHILD_CMD=sleep 1; echo boom >&2; exit 3",
		"RAZORPAY_KEY_ID=rzp_test_stub",
		"RAZORPAY_KEY_SECRET=stub",
	)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdin = r
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	_ = w.Close() // agent EOF straight away

	err2, exited := waitWithin(t, cmd, 20*time.Second)
	if !exited {
		t.Fatal("guard did not exit")
	}
	if err2 == nil {
		t.Fatal("child exited 3 during the drain window and the guard still " +
			"reported a clean run")
	}
}
