//go:build testhook

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// PinnedImage is unused in this build; the test-hook child is a local stub.
const PinnedImage = "(test-hook build: no container)"

// newChild runs a local stub instead of the pinned container.
//
// This file is compiled ONLY under `-tags testhook`, so the shipped binary has
// no arbitrary-child-command path at all. It exists to test process-boundary
// recovery deterministically: against the real container Razorpay answers in
// well under a second, so a reply always wins the race against a kill and the
// death path is never exercised.
//
// The stub is deliberately given NO Razorpay credentials. It does not talk to
// Razorpay and has no business holding keys.
func newChild(ctx context.Context, keyID, keySecret string) (*exec.Cmd, error) {
	cmdline := os.Getenv("RZP_GUARD_CHILD_CMD")
	if cmdline == "" {
		return nil, fmt.Errorf("testhook build requires RZP_GUARD_CHILD_CMD")
	}
	fmt.Fprintln(os.Stderr,
		"rzp-guard: TEST-HOOK BUILD -- running a local stub child with NO credentials; "+
			"this binary must never be used against Razorpay")
	c := exec.CommandContext(ctx, "sh", "-c", cmdline)
	// envWithoutRazorpayKeys strips the secrets: the stub never receives them.
	c.Env = envWithoutRazorpayKeys()
	return c, nil
}
