package main

import (
	"fmt"
	"os"
	"strings"
)

// Provider wiring for the Phase 4b study.
//
// The endpoint is part of the FROZEN record, not ambient environment. It is
// resolved once by resolve-model, written into study/model.frozen.json, and
// committed; every run then reads it back from there. Without that, a run could
// silently point at a different service than the protocol names and the traces
// would carry no evidence of it.
//
// TWO PROVIDERS, AND ONE OF THEM IS UNTRUSTED BY CONSTRUCTION.
//
//	openai   OpenAI Responses API, direct account        apiResponses
//	proxy    Anthropic Messages format, intermediary     apiMessages
//
// The proxy is used because it is the only credential available, and its
// limitations are handled by SCOPING THE CLAIM rather than by pretending they
// are absent (PROTOCOL.md 4.5).
//
// What is actually wrong with it, measured: requesting gpt-5.6 returned
// grok-4.6, deterministically, while gpt-5.6 sat in its own advertised
// catalogue. It names no operator, publishes no retention policy, and cannot be
// audited from outside.
//
// Why that matters to EVERY number, not just the model-specific one: each
// denominator counts calls the agent ACTUALLY EMITTED, so a change of model
// changes the call distribution and moves the measured rates while the guard
// stands still.
//
// The controls that make it usable at all, none of which establish trust:
//
//	per turn     the served model must equal the requested one, or hard error
//	per turn     the response id is recorded
//	per study    every trace must report the SAME served model
//	in the write-up  the generator is named as unverified, and every emitted
//	                 call is published so the distribution is observable data
//	                 rather than something inferred from a model label

const (
	providerOpenAI = "openai"
	providerProxy  = "proxy"

	apiResponses = "responses"
	apiMessages  = "messages"

	// The proxy speaks the Anthropic Messages format but is NOT Anthropic. Its
	// key deliberately does not reuse ANTHROPIC_API_KEY: that variable belongs
	// to the operator's own tooling, and borrowing it risks billing or leaking
	// the wrong credential.
	proxyKeyEnv     = "RZP_STUDY_PROXY_API_KEY"
	proxyBaseEnv    = "RZP_STUDY_PROXY_BASE"
	proxyDefaultURL = "https://api.a6api.com"

	openAIBase   = "https://api.openai.com/v1"
	openAIKeyEnv = "OPENAI_API_KEY"

	responsesPath = "/responses"
	modelsPath    = "/models"
)

type provider struct {
	Name    string
	API     string
	BaseURL string
	KeyEnv  string
}

func resolveProvider(name string) (*provider, error) {
	switch name {
	case providerOpenAI:
		return &provider{Name: providerOpenAI, API: apiResponses,
			BaseURL: openAIBase, KeyEnv: openAIKeyEnv}, nil
	case providerProxy:
		base := strings.TrimSpace(os.Getenv(proxyBaseEnv))
		if base == "" {
			base = proxyDefaultURL
		}
		return &provider{Name: providerProxy, API: apiMessages,
			BaseURL: strings.TrimRight(base, "/"), KeyEnv: proxyKeyEnv}, nil
	default:
		return nil, fmt.Errorf("unknown provider %q; expected %s or %s",
			name, providerOpenAI, providerProxy)
	}
}

// providerFromFrozen reconstructs the provider recorded at freeze time. Base
// URL and API come from the committed file, never from the environment, so a
// run cannot be redirected after the fact.
func providerFromFrozen(fm *frozenModel) (*provider, error) {
	if fm.Provider == "" || fm.BaseURL == "" || fm.API == "" {
		return nil, fmt.Errorf("study/model.frozen.json records no provider/base_url/api; " +
			"re-run resolve-model and commit the result")
	}
	keyEnv := openAIKeyEnv
	if fm.Provider == providerProxy {
		keyEnv = proxyKeyEnv
	}
	return &provider{Name: fm.Provider, API: fm.API,
		BaseURL: fm.BaseURL, KeyEnv: keyEnv}, nil
}

func (p *provider) credential() (string, error) {
	k := strings.TrimSpace(os.Getenv(p.KeyEnv))
	if k == "" {
		return "", fmt.Errorf("%s is not set (provider %q)", p.KeyEnv, p.Name)
	}
	return k, nil
}

// enumerates reports whether the mechanical selection rule can be applied.
//
// Not against the proxy: its catalogue is a claim about routing, not a
// guarantee of what is served, and it advertised gpt-5.6 while serving
// grok-4.6. Running a "highest version" rule over such a list would be theatre.
// There the model is operator-supplied, recorded as such, and checked against
// every response instead.
func (p *provider) enumerates() bool { return p.API == apiResponses }

func validateModelID(p *provider, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("empty model id")
	}
	return nil
}
