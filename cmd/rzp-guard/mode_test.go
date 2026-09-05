package main

import (
	"strings"
	"testing"
)

// PRODUCTION MODE exists because the audit's one still-OPEN HIGH finding was
// mitigated by a warning, and a warning is not a control. These pin the two
// properties that make it one: it refuses rather than degrades, and it says
// everything that is wrong at once.

func TestProductionRefusesRatherThanDegrades(t *testing.T) {
	err := enforceProduction([]productionRequirement{
		{name: "signed mandate", ok: false, why: "anyone who can write the file grants authority", fix: "-mandate-pubkey"},
		{name: "decision log", ok: true, why: "x", fix: "y"},
	})
	if err == nil {
		t.Fatal("production mode started with an unsigned mandate. A guard that " +
			"starts without the protections its operator asked for is worse than " +
			"one that does not start, and it is worse quietly")
	}
	if !strings.Contains(err.Error(), "signed mandate") {
		t.Errorf("the refusal does not name what is missing: %v", err)
	}
	// The satisfied requirement must not appear -- an error that lists things
	// that are fine is one people stop reading.
	if strings.Contains(err.Error(), "decision log") {
		t.Errorf("the refusal names a requirement that was met: %v", err)
	}
}

// All at once. Refusing one at a time turns one configuration pass into as many
// deploy attempts as there are missing flags, and the last thing anyone does at
// the end of that loop is stop reading.
func TestProductionReportsEveryFailureTogether(t *testing.T) {
	err := enforceProduction([]productionRequirement{
		{name: "alpha", ok: false, why: "a", fix: "A"},
		{name: "beta", ok: false, why: "b", fix: "B"},
		{name: "gamma", ok: false, why: "c", fix: "C"},
	})
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"alpha", "beta", "gamma", "A", "B", "C"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal omits %q, so fixing it takes an extra deploy: %v", want, err)
		}
	}
}

func TestAllRequirementsMetStartsCleanly(t *testing.T) {
	if err := enforceProduction([]productionRequirement{
		{name: "alpha", ok: true}, {name: "beta", ok: true},
	}); err != nil {
		t.Fatalf("a fully configured production deployment was refused: %v", err)
	}
}

// The mode is read from the environment as well as the flag, and that matters:
// a deployment sets it once in a unit file or a container spec, where it
// survives somebody editing the command line to debug something at 3am.
func TestModeComesFromTheFlagOrTheEnvironment(t *testing.T) {
	t.Setenv("RZP_GUARD_MODE", "")
	for _, tc := range []struct {
		flag, env string
		want      guardMode
	}{
		{"", "", modeDev},
		{"production", "", modeProduction},
		{"prod", "", modeProduction},
		{"", "production", modeProduction},
		{"", "development", modeDev},
		// The flag wins when both are given: it is the more specific statement.
		{"development", "production", modeDev},
	} {
		t.Setenv("RZP_GUARD_MODE", tc.env)
		got, err := parseMode(tc.flag)
		if err != nil {
			t.Fatalf("parseMode(%q) with env %q: %v", tc.flag, tc.env, err)
		}
		if got != tc.want {
			t.Errorf("parseMode(%q) env %q = %q, want %q", tc.flag, tc.env, got, tc.want)
		}
	}
}

// A misspelled mode must fail loudly. Silently falling back to development is
// how a deployment that asked for production does not get it.
func TestAnUnknownModeIsRefused(t *testing.T) {
	t.Setenv("RZP_GUARD_MODE", "")
	if _, err := parseMode("prodution"); err == nil {
		t.Fatal("a misspelled mode silently became development")
	}
}
