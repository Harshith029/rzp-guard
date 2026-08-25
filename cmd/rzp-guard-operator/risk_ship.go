//go:build !testhook

package main

// acceptRiskFlag is ABSENT from shipped builds.
//
// A previous revision made the default fail closed and then shipped a flag that
// committed the credential anyway once delivery had been PROVEN undeliverable.
// An explicit acknowledgement does not make the resulting state recoverable --
// it is the same permanently unrecoverable state file, reached deliberately.
// "UNSUPPORTED FOR DEPLOYMENT" is a label, not a control.
func acceptRiskFlag() *bool {
	no := false
	return &no
}

const riskHelp = ""
