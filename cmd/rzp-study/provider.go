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
// ONE PROVIDER, AND IT MUST BE A DIRECT ACCOUNT.
//
// A third-party API proxy was tried and REJECTED (PROTOCOL.md 4.4). It
// substituted models silently -- a request for gpt-5.6 was served by grok-4.6,
// deterministically, while gpt-5.6 sat in its own advertised catalogue -- named
// no operator, published no retention policy, and offered no way to verify what
// actually answered.
//
// That is not merely a caveat on one metric. Every quantity this study reports
// has a denominator counting calls the agent ACTUALLY EMITTED, which is a
// sample from one model's behaviour. A backend free to swap the model is free
// to move every one of those numbers while the guard stands still. So an
// intermediary is not acceptable instrumentation here, and no proxy client
// remains in this repository.

const (
	providerOpenAI = "openai"

	apiResponses = "responses"

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
	default:
		return nil, fmt.Errorf("unknown provider %q; only %q is supported. "+
			"A third-party proxy was rejected as instrumentation (PROTOCOL.md 4.4): "+
			"it substituted models silently, so it could move every measured rate "+
			"without the guard changing at all", name, providerOpenAI)
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
	if fm.Provider != providerOpenAI {
		return nil, fmt.Errorf("model freeze names provider %q, which is not supported; "+
			"re-resolve against a direct provider account", fm.Provider)
	}
	return &provider{Name: fm.Provider, API: fm.API,
		BaseURL: fm.BaseURL, KeyEnv: openAIKeyEnv}, nil
}

func (p *provider) credential() (string, error) {
	k := strings.TrimSpace(os.Getenv(p.KeyEnv))
	if k == "" {
		return "", fmt.Errorf("%s is not set (provider %q)", p.KeyEnv, p.Name)
	}
	return k, nil
}

// enumerates reports whether the mechanical selection rule can be applied.
func (p *provider) enumerates() bool { return p.API == apiResponses }

func validateModelID(p *provider, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("empty model id")
	}
	return nil
}
