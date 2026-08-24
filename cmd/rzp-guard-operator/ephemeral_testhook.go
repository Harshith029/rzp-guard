//go:build testhook

package main

import (
	"fmt"

	"github.com/harshith/rzp-guard/internal/opauth"
	"github.com/harshith/rzp-guard/internal/storage"
)

// initEphemeral provisions a verifier for a token that is generated, used to
// derive the verifier, and then DISCARDED without ever being written anywhere.
//
// The live gates only need the guard to start; they never resolve anything. The
// previous approach wrote a real recovery token into evidence/live/ and deleted
// it afterwards -- but this tree is OneDrive-backed, so sync software could
// upload it during that window, and asserting the file is absent at the end
// proves nothing about whether it was ever copied. The fix is not to create a
// usable recovery secret in the repo tree at all.
//
// The resulting state file is deliberately UNRECOVERABLE: no human holds a
// token for it. That is correct for a throwaway fixture and wrong for anything
// else, which is why this lives behind -tags testhook.
func initEphemeral(s any) error {
	store, ok := s.(*storage.Store)
	if !ok {
		return fmt.Errorf("init-ephemeral: unexpected store type %T", s)
	}
	token, err := opauth.NewToken()
	if err != nil {
		return err
	}
	verifier, err := opauth.Verifier(token)
	if err != nil {
		return err
	}
	if err := store.InitOperatorVerifier(verifier); err != nil {
		return err
	}
	fmt.Println("Ephemeral credential provisioned; the token was discarded and this " +
		"state file cannot be recovered. Test fixtures only.")
	return nil
}

const ephemeralHelp = "\n(test-hook build: init-ephemeral is available)\n"
