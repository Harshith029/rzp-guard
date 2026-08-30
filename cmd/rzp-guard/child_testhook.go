//go:build testhook

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
// strictChildEnv, when set, restricts this build to executing ONE named binary
// instead of an arbitrary shell command.
//
// RZP_GUARD_CHILD_CMD runs through `sh -c`, which is the point: the recovery
// gate needs a child that reads N bytes and exits, and the lifecycle tests need
// one that hangs. Those are legitimate and they need a shell.
//
// It is also, in a red-team context, arbitrary command execution. External
// review observed that telling a reviewer "use mcp-stub" is advisory while this
// variable accepts anything -- someone could run docker, or curl, without
// intending to. So the isolation lane sets RZP_GUARD_CHILD_STRICT=1 and this
// build then accepts only the stub path, refusing the shell entirely.
//
// The guarantee is deliberately enforced HERE rather than only in run.sh: a
// harness that can be bypassed by invoking the binary directly is the same
// class of advisory boundary the review was complaining about.
const strictChildEnv = "RZP_GUARD_CHILD_STRICT"

// strictChildPath is the only executable a strict test-hook build will run.
const strictChildPath = "./.gotmp/mcp-stub"

func newChild(ctx context.Context, keyID, keySecret string) (*exec.Cmd, error) {
	if os.Getenv(strictChildEnv) != "" {
		// No shell, no argument string, no interpretation: exec one path.
		if _, err := os.Stat(strictChildPath); err != nil {
			return nil, fmt.Errorf("%s is set, so the only permitted child is %s, "+
				"and it is not built: %w (build it with "+
				"`go build -o %s ./cmd/mcp-stub`)",
				strictChildEnv, strictChildPath, err, strictChildPath)
		}
		fmt.Fprintln(os.Stderr,
			"rzp-guard: TEST-HOOK BUILD, STRICT -- child is "+strictChildPath+
				" and RZP_GUARD_CHILD_CMD is ignored")
		c := exec.CommandContext(ctx, strictChildPath)
		c.Env = envWithoutRazorpayKeys()
		return c, nil
	}

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
