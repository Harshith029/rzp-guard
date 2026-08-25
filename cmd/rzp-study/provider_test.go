package main

import "testing"

func ids(names ...string) []modelInfo {
	out := make([]modelInfo, 0, len(names))
	for _, n := range names {
		out = append(out, modelInfo{ID: n})
	}
	return out
}

// The tier preference is a PRE-REGISTERED decision (PROTOCOL.md 4): the
// everyday-production tier, because that is what a real support deployment
// runs. Encoding it in a test is what stops it drifting into "whichever tier
// produced the nicer number".
func TestPickModelPrefersTheProductionTier(t *testing.T) {
	for _, tc := range []struct {
		name string
		list []modelInfo
		want string
	}{
		{
			"bedrock cross-region inference profiles",
			ids("us.openai.gpt-5.6-sol", "us.openai.gpt-5.6-terra",
				"us.openai.gpt-5.6-luna", "us.openai.gpt-5.5", "us.openai.gpt-5.4"),
			"us.openai.gpt-5.6-terra",
		},
		{
			"bedrock raw model ids",
			ids("openai.gpt-5.6-sol", "openai.gpt-5.6-terra", "openai.gpt-5.5"),
			"openai.gpt-5.6-terra",
		},
		{
			"direct openai",
			ids("gpt-5.6-terra", "gpt-5.6-sol", "gpt-5.5", "gpt-4o-mini", "gpt-5.6-luna"),
			"gpt-5.6-terra",
		},
		{
			"newer production tier beats older production tier",
			ids("openai.gpt-5.5-terra", "openai.gpt-5.6-terra"),
			"openai.gpt-5.6-terra",
		},
		{
			"an untiered flagship is eligible when no tiered one exists",
			ids("openai.gpt-5.5", "openai.gpt-5.4"),
			"openai.gpt-5.5",
		},
		{
			"a production tier outranks a higher-versioned reasoning tier",
			ids("openai.gpt-5.6-sol", "openai.gpt-5.5-terra"),
			"openai.gpt-5.5-terra",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := pickModel(tc.list)
			if got != tc.want {
				t.Fatalf("picked %q, want %q", got, tc.want)
			}
		})
	}
}

// Failing closed matters more than picking something. If only excluded tiers
// are visible, resolve-model must refuse and print the list so the rule can be
// amended from real ids -- pre-trace and committed -- rather than silently
// running against a model the protocol never named.
func TestPickModelFailsClosedOnExcludedTiersOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		list []modelInfo
	}{
		{"reasoning and fast tiers only", ids("openai.gpt-5.6-sol", "openai.gpt-5.6-luna")},
		{"size variants only", ids("gpt-5.6-mini", "gpt-5.6-nano")},
		{"an unrecognised tier", ids("openai.gpt-5.7-aurora")},
		{"nothing usable", ids("text-embedding-3-large", "whisper-1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := pickModel(tc.list); got != "" {
				t.Fatalf("picked %q; an ineligible list must yield no choice", got)
			}
		})
	}
}

// Bedrock's two OpenAI-compatible routes take DIFFERENT identifier shapes, and
// sending the wrong one is the most likely first-call failure. Catching it
// before the request is made turns an opaque 400 into a sentence.
func TestValidateModelIDMatchesTheRoute(t *testing.T) {
	runtime := &provider{Name: providerBedrock,
		BaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com/openai/v1"}
	mantle := &provider{Name: providerBedrock,
		BaseURL: "https://bedrock-mantle.us-east-1.api.aws/openai/v1"}
	direct := &provider{Name: providerOpenAI, BaseURL: openAIBase}

	for _, tc := range []struct {
		name    string
		p       *provider
		id      string
		wantErr bool
	}{
		{"runtime accepts a geography profile", runtime, "us.openai.gpt-5.6-terra", false},
		{"runtime accepts a global profile", runtime, "global.openai.gpt-5.6-terra", false},
		{"runtime REJECTS a raw model id", runtime, "openai.gpt-5.6-terra", true},
		{"runtime rejects a bare openai id", runtime, "gpt-5.6-terra", true},
		{"mantle accepts a raw model id", mantle, "openai.gpt-5.6-terra", false},
		{"mantle rejects an unprefixed id", mantle, "gpt-5.6-terra", true},
		{"direct openai accepts a bare id", direct, "gpt-5.6-terra", false},
		{"empty is always rejected", direct, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateModelID(tc.p, tc.id)
			if tc.wantErr && err == nil {
				t.Fatalf("%q against %s: expected an error, got none", tc.id, tc.p.BaseURL)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("%q against %s: unexpected error %v", tc.id, tc.p.BaseURL, err)
			}
		})
	}
}

// The endpoint must come from the committed freeze. If it fell back to ambient
// environment, a run could point somewhere the protocol never named and the
// traces would carry no evidence of it.
func TestProviderFromFrozenRequiresARecordedEndpoint(t *testing.T) {
	if _, err := providerFromFrozen(&frozenModel{Model: "us.openai.gpt-5.6-terra"}); err == nil {
		t.Fatal("a freeze with no provider/base_url must be refused")
	}
	p, err := providerFromFrozen(&frozenModel{
		Provider: providerBedrock,
		BaseURL:  "https://bedrock-runtime.ap-south-1.amazonaws.com/openai/v1",
		Model:    "us.openai.gpt-5.6-terra",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.KeyEnv != bedrockKeyEnv {
		t.Fatalf("bedrock freeze resolved to key env %q, want %q", p.KeyEnv, bedrockKeyEnv)
	}
	if p.BaseURL != "https://bedrock-runtime.ap-south-1.amazonaws.com/openai/v1" {
		t.Fatalf("base URL was not taken from the freeze: %q", p.BaseURL)
	}
}
