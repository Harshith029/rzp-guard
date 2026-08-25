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
// Two providers, speaking two DIFFERENT wire formats -- this is not a base-URL
// switch:
//
//	proxy    Anthropic Messages API, third-party endpoint    apiMessages
//	openai   OpenAI Responses API, api.openai.com            apiResponses
//
// `proxy` is the default because it is what this project has credentials for.

const (
	providerProxy  = "proxy"
	providerOpenAI = "openai"

	apiMessages  = "messages"
	apiResponses = "responses"

	openAIBase   = "https://api.openai.com/v1"
	openAIKeyEnv = "OPENAI_API_KEY"

	// The proxy speaks the Anthropic Messages format but is NOT Anthropic. Its
	// key deliberately does not reuse ANTHROPIC_API_KEY: that variable belongs
	// to the operator's own tooling, and borrowing it risks billing or leaking
	// the wrong credential.
	proxyKeyEnv     = "NIHAL_CUSTOM_KEY"
	proxyBaseEnv    = "RZP_STUDY_PROXY_BASE"
	proxyDefaultURL = "https://api.a6api.com"

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
	case providerProxy:
		base := strings.TrimSpace(os.Getenv(proxyBaseEnv))
		if base == "" {
			base = proxyDefaultURL
		}
		return &provider{Name: providerProxy, API: apiMessages,
			BaseURL: strings.TrimRight(base, "/"), KeyEnv: proxyKeyEnv}, nil

	case providerOpenAI:
		return &provider{Name: providerOpenAI, API: apiResponses,
			BaseURL: openAIBase, KeyEnv: openAIKeyEnv}, nil

	default:
		return nil, fmt.Errorf("unknown provider %q; expected %s or %s",
			name, providerProxy, providerOpenAI)
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
	keyEnv := proxyKeyEnv
	if fm.Provider == providerOpenAI {
		keyEnv = openAIKeyEnv
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
// It cannot against the proxy. A third-party endpoint's model list is a claim
// about what it will route, not a guarantee of what it serves -- and this one
// demonstrably substitutes: asking for "gpt-5.6" returned "grok-4.6". Picking
// the "highest" id off such a list would be theatre. There the model is
// operator-supplied, recorded as such, and verified per response instead.
func (p *provider) enumerates() bool { return p.API == apiResponses }

// validateModelID rejects an identifier that cannot work, before any spend.
//
// For the proxy there is nothing useful to check up front -- any string may or
// may not route. The real control is downstream: every response's `model` field
// is compared with what was requested, and a substitution is an error, not a
// warning. See anthropic_messages.go.
func validateModelID(p *provider, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("empty model id")
	}
	return nil
}
