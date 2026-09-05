package main

import (
	"fmt"
	"os"
	"strings"
)

// PRODUCTION MODE, which exists to close the one security finding the audit
// still lists as OPEN at HIGH:
//
//	"Mandate signing is opt-in. Unsigned, anyone who can write the file grants
//	 authority. OPEN -- documented, warns loudly."
//
// A warning is not a control. The guard prints one line to stderr and then
// enforces a mandate it has no reason to believe the merchant issued. On a
// developer's machine that is the right trade -- requiring a signing key to run
// ./run.sh demo would make the thing unrunnable, and an unrunnable safety
// component is not safer. In production it is not a trade at all.
//
// So the setting is a MODE rather than a flag per property. Individual flags
// are what produce a deployment with four of five safeguards on: each is a
// separate decision somebody has to remember, and the one nobody remembers is
// the one that matters. A mode is one decision with a stated meaning, and it
// FAILS CLOSED -- refusing to start rather than degrading -- because a guard
// that starts without the protections its operator asked for is worse than one
// that does not start, and it is worse quietly.
type guardMode string

const (
	// modeDev is the default and is exactly the behaviour this build has always
	// had. Named, so "development" is a choice in the log rather than an absence.
	modeDev guardMode = "development"

	// modeProduction requires every optional protection to be present.
	modeProduction guardMode = "production"
)

// parseMode reads the mode from the flag, falling back to the environment.
//
// The environment matters more than it looks: a deployment sets RZP_GUARD_MODE
// once in a unit file or a container spec, where it survives somebody editing
// the command line to debug something at 3am. A flag alone is a protection that
// disappears the first time an argument list is retyped under pressure.
func parseMode(flagValue string) (guardMode, error) {
	v := strings.TrimSpace(flagValue)
	if v == "" {
		v = strings.TrimSpace(os.Getenv("RZP_GUARD_MODE"))
	}
	switch strings.ToLower(v) {
	case "", "dev", "development":
		return modeDev, nil
	case "prod", "production":
		return modeProduction, nil
	default:
		return "", fmt.Errorf("-mode %q is not one of development | production", flagValue)
	}
}

// productionRequirements is what production mode insists on.
//
// Each entry names the property, whether it is satisfied, and WHAT GOES WRONG
// without it -- because the error a person meets at deploy time should explain
// the risk rather than quote a flag name back at them.
type productionRequirement struct {
	name string
	ok   bool
	why  string
	fix  string
}

// enforceProduction refuses to start unless every requirement is met.
//
// It reports ALL failures at once. Refusing one at a time turns a single
// configuration pass into as many deploy attempts as there are missing flags,
// and the last thing a person does at the end of that loop is stop reading.
func enforceProduction(reqs []productionRequirement) error {
	var missing []productionRequirement
	for _, r := range reqs {
		if !r.ok {
			missing = append(missing, r)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "refusing to start in production mode: %d requirement(s) unmet.\n",
		len(missing))
	for _, r := range missing {
		fmt.Fprintf(&b, "\n  %s\n    why it matters: %s\n    fix: %s\n", r.name, r.why, r.fix)
	}
	b.WriteString("\nRun without -mode production to accept these gaps deliberately; " +
		"the guard will start and say what it is missing.")
	return fmt.Errorf("%s", b.String())
}
