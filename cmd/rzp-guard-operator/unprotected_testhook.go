//go:build testhook

package main

import "flag"

// allowUnprotectedFlag exists ONLY in test-hook builds. The shipped operator has
// no way to bypass the 0600 verification.
func allowUnprotectedFlag() *bool {
	return flag.Bool("allow-unprotected-out", false,
		"TEST-HOOK ONLY: permit -out where the platform cannot apply 0600")
}

const unprotectedHelp = "\n(test-hook build: -allow-unprotected-out is available)\n"
