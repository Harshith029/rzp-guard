//go:build !testhook

package main

import "errors"

// initEphemeral is absent from shipped builds.
func initEphemeral(_ any) error {
	return errors.New("init-ephemeral is a test-hook command and is not in this build")
}

const ephemeralHelp = ""
