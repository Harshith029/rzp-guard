//go:build testhook && !redteam

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// PinnedImage and Toolsets are unused in this build; the test-hook child is a
// local stub. They exist so -version compiles and, more importantly, so a
// test-hook binary SAYS SO when asked what it is. A build that cannot be
// distinguished from the shipped one at runtime is a build that ends up in the
// wrong place.
const PinnedImage = "(test-hook build: no container)"
const Toolsets = "(test-hook build: local stub child, NEVER for Razorpay)"

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
// This build takes an arbitrary shell command, and that is the point: the
// recovery gate needs a child that reads N bytes and exits, the lifecycle tests
// need one that hangs, and neither is expressible without a shell.
//
// It is also arbitrary command execution, so it must never be what an external
// reviewer runs. An earlier attempt gated it behind RZP_GUARD_CHILD_STRICT --
// an ENVIRONMENT switch, which anything running inside the lane could simply
// unset. Review pointed that out and was right. The guarantee now comes from
// WHICH FILE COMPILED: `-tags redteam` selects child_redteam.go instead, and
// that file contains no shell, no environment lookup and no path resolution.
//
// Build tags are not a security boundary against someone choosing their own
// build. They are a boundary against a reviewer, a script or a harness reaching
// this code by accident, which is the actual threat here.
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
