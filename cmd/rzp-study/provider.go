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
// committed; every run then uses what was recorded. Without that, a run could
// silently point at a different service than the one the protocol names, and
// the traces would carry no evidence of it.
//
// Two providers are supported. Both speak the same OpenAI Responses API and
// both authenticate with a bearer token, so the only real differences are the
// base URL, which environment variable holds the credential, and the shape of
// the model identifier.

const (
	providerOpenAI  = "openai"
	providerBedrock = "bedrock"

	openAIBase       = "https://api.openai.com/v1"
	openAIKeyEnv     = "OPENAI_API_KEY"
	bedrockKeyEnv    = "AWS_BEARER_TOKEN_BEDROCK"
	bedrockRegionEnv = "AWS_REGION"

	responsesPath = "/responses"
	modelsPath    = "/models"
)

type provider struct {
	Name    string
	BaseURL string
	KeyEnv  string
}

// bedrockBase builds the cross-Region runtime endpoint.
//
// Amazon exposes two OpenAI-compatible routes and they are NOT interchangeable:
//
//	bedrock-mantle.<region>.api.aws/openai/v1        in-Region, raw model id
//	                                                 (openai.gpt-5.6-terra)
//	bedrock-runtime.<region>.amazonaws.com/openai/v1 cross-Region, and the model
//	                                                 MUST be an inference profile
//	                                                 id (us.openai.gpt-5.6-terra)
//
// The runtime route is the default here because a single-Region endpoint makes
// a 45-trace run hostage to capacity in one Region, and a study that dies
// halfway through is a study that gets re-run until it finishes -- which is the
// freedom the pre-registration exists to remove. Passing the wrong id shape for
// the chosen route is the most likely first-call failure, so validateModelID
// checks it explicitly rather than letting a 400 explain it.
func bedrockBase(region string) string {
	return fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com/openai/v1", region)
}

func resolveProvider(name string) (*provider, error) {
	switch name {
	case providerOpenAI:
		return &provider{Name: providerOpenAI, BaseURL: openAIBase, KeyEnv: openAIKeyEnv}, nil

	case providerBedrock:
		region := strings.TrimSpace(os.Getenv(bedrockRegionEnv))
		if region == "" {
			return nil, fmt.Errorf("%s is not set; Bedrock endpoints are per-Region "+
				"and guessing one would silently change where traces ran", bedrockRegionEnv)
		}
		return &provider{
			Name:    providerBedrock,
			BaseURL: bedrockBase(region),
			KeyEnv:  bedrockKeyEnv,
		}, nil

	default:
		return nil, fmt.Errorf("unknown provider %q; expected %s or %s",
			name, providerOpenAI, providerBedrock)
	}
}

// providerFromFrozen reconstructs the provider recorded at freeze time. The base
// URL comes from the committed file, never from the environment, so a run cannot
// be redirected after the fact.
func providerFromFrozen(fm *frozenModel) (*provider, error) {
	if fm.Provider == "" || fm.BaseURL == "" {
		return nil, fmt.Errorf("study/model.frozen.json records no provider/base_url; " +
			"re-run resolve-model and commit the result")
	}
	keyEnv := openAIKeyEnv
	if fm.Provider == providerBedrock {
		keyEnv = bedrockKeyEnv
	}
	return &provider{Name: fm.Provider, BaseURL: fm.BaseURL, KeyEnv: keyEnv}, nil
}

func (p *provider) credential() (string, error) {
	k := strings.TrimSpace(os.Getenv(p.KeyEnv))
	if k == "" {
		return "", fmt.Errorf("%s is not set (provider %q)", p.KeyEnv, p.Name)
	}
	return k, nil
}

// validateModelID rejects an identifier that cannot work against the recorded
// endpoint, before any token is spent.
func validateModelID(p *provider, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("empty model id")
	}
	if p.Name != providerBedrock {
		return nil
	}
	if strings.Contains(p.BaseURL, "bedrock-runtime") {
		// Geographic or global inference profile, e.g. us.openai.gpt-5.6-terra.
		if !strings.Contains(id, ".openai.") {
			return fmt.Errorf("model %q is not an inference profile id; the "+
				"bedrock-runtime route requires a geography prefix such as "+
				"us.openai.… or global.openai.…", id)
		}
		return nil
	}
	if !strings.HasPrefix(id, "openai.") {
		return fmt.Errorf("model %q does not look like a Bedrock OpenAI model id "+
			"(expected an openai. prefix)", id)
	}
	return nil
}
