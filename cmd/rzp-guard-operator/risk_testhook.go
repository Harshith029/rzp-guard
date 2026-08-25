//go:build testhook

package main

import "flag"

// acceptRiskFlag exists ONLY in test-hook builds.
func acceptRiskFlag() *bool {
	return flag.Bool("accept-delivery-risk", false,
		"TEST-HOOK ONLY: commit even though delivery cannot be proven durable")
}

const riskHelp = "\n(test-hook build: -accept-delivery-risk is available)\n"
