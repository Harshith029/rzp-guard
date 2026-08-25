package main

import (
	"os"
	"testing"
)

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
//
// It applies only where an endpoint can be enumerated meaningfully -- see
// TestProxyCannotEnumerate.
func TestPickModelPrefersTheProductionTier(t *testing.T) {
	for _, tc := range []struct {
		name string
		list []modelInfo
		want string
	}{
		{"tiered flagships",
			ids("gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.4"),
			"gpt-5.6-terra"},
		{"mixed with size variants",
			ids("gpt-5.6-terra", "gpt-5.6-sol", "gpt-4o-mini", "gpt-5.6-luna"),
			"gpt-5.6-terra"},
		{"newer production tier wins",
			ids("gpt-5.5-terra", "gpt-5.6-terra"),
			"gpt-5.6-terra"},
		{"untiered flagship is eligible when no tiered one exists",
			ids("gpt-5.5", "gpt-5.4"),
			"gpt-5.5"},
		{"production tier outranks a higher-versioned reasoning tier",
			ids("gpt-5.6-sol", "gpt-5.5-terra"),
			"gpt-5.5-terra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := pickModel(tc.list); got != tc.want {
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
		{"reasoning and fast tiers only", ids("gpt-5.6-sol", "gpt-5.6-luna")},
		{"size variants only", ids("gpt-5.6-mini", "gpt-5.6-nano")},
		{"an unrecognised tier", ids("gpt-5.7-aurora")},
		{"nothing usable", ids("text-embedding-3-large", "whisper-1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := pickModel(tc.list); got != "" {
				t.Fatalf("picked %q; an ineligible list must yield no choice", got)
			}
		})
	}
}

// A third-party proxy's model list is a claim about routing, not a guarantee of
// what gets served -- this one returned grok-4.6 for a gpt-5.6 request. Running
// the mechanical selection rule against such a list would be theatre dressed as
// rigour, so the proxy must declare it cannot enumerate, forcing an
// operator-supplied id that is recorded as such and verified per response.
func TestProxyCannotEnumerate(t *testing.T) {
	proxy := &provider{Name: providerProxy, API: apiMessages}
	if proxy.enumerates() {
		t.Fatal("the proxy must not claim to enumerate: its list is not a guarantee, " +
			"and it has been observed substituting models")
	}
	direct := &provider{Name: providerOpenAI, API: apiResponses}
	if !direct.enumerates() {
		t.Fatal("the OpenAI provider should enumerate")
	}
}

func TestResolveProxyProvider(t *testing.T) {
	t.Setenv(proxyBaseEnv, "")
	p, err := resolveProvider(providerProxy)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.API != apiMessages {
		t.Fatalf("API = %q, want %q", p.API, apiMessages)
	}
	if p.BaseURL != proxyDefaultURL {
		t.Fatalf("BaseURL = %q, want the default %q", p.BaseURL, proxyDefaultURL)
	}
	// The proxy is NOT Anthropic; borrowing ANTHROPIC_API_KEY could bill or leak
	// the operator's own credential.
	if p.KeyEnv != proxyKeyEnv {
		t.Fatalf("KeyEnv = %q, want %q", p.KeyEnv, proxyKeyEnv)
	}
	if p.KeyEnv == "ANTHROPIC_API_KEY" {
		t.Fatal("the proxy must never read ANTHROPIC_API_KEY")
	}

	t.Setenv(proxyBaseEnv, "https://example.test/base/")
	p, err = resolveProvider(providerProxy)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.BaseURL != "https://example.test/base" {
		t.Fatalf("override not honoured or trailing slash kept: %q", p.BaseURL)
	}
}

func TestUnknownProviderRefused(t *testing.T) {
	if _, err := resolveProvider("bedrock"); err == nil {
		t.Fatal("an unsupported provider must be refused, not silently defaulted")
	}
}

// The endpoint AND the wire format must come from the committed freeze. If
// either fell back to ambient environment or a default, a run could speak a
// different protocol, or to a different service, than the protocol names -- and
// the traces would carry no evidence of it.
func TestProviderFromFrozenRequiresARecordedEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name string
		fm   frozenModel
	}{
		{"no provider", frozenModel{BaseURL: "https://x", API: apiMessages}},
		{"no base url", frozenModel{Provider: providerProxy, API: apiMessages}},
		{"no api", frozenModel{Provider: providerProxy, BaseURL: "https://x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := providerFromFrozen(&tc.fm); err == nil {
				t.Fatal("an incomplete freeze must be refused")
			}
		})
	}

	p, err := providerFromFrozen(&frozenModel{
		Provider: providerProxy,
		API:      apiMessages,
		BaseURL:  "https://recorded.example",
		Model:    "gpt-5.6-sol",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.BaseURL != "https://recorded.example" {
		t.Fatalf("base URL was not taken from the freeze: %q", p.BaseURL)
	}
	if p.API != apiMessages {
		t.Fatalf("API was not taken from the freeze: %q", p.API)
	}

	// An environment override must NOT be able to redirect a frozen run.
	os.Setenv(proxyBaseEnv, "https://attacker.example")
	defer os.Unsetenv(proxyBaseEnv)
	p2, err := providerFromFrozen(&frozenModel{
		Provider: providerProxy, API: apiMessages,
		BaseURL: "https://recorded.example", Model: "gpt-5.6-sol",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p2.BaseURL != "https://recorded.example" {
		t.Fatalf("environment overrode the committed endpoint: %q", p2.BaseURL)
	}
}

func TestValidateModelIDRejectsEmpty(t *testing.T) {
	p := &provider{Name: providerProxy, API: apiMessages}
	if err := validateModelID(p, ""); err == nil {
		t.Fatal("an empty model id must be rejected")
	}
	if err := validateModelID(p, "gpt-5.6-sol"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
