package openaiwire

import (
	"errors"
	"fmt"

	"github.com/rajeev-chaurasia/pen-stock/internal/config"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// Default endpoints. An operator supplied base_url always wins; these
// only spare the common case from spelling out a URL that never moves.
const (
	openAIBaseURL     = "https://api.openai.com/v1"
	groqBaseURL       = "https://api.groq.com/openai/v1"
	cerebrasBaseURL   = "https://api.cerebras.ai/v1"
	mistralBaseURL    = "https://api.mistral.ai/v1"
	openRouterBaseURL = "https://openrouter.ai/api/v1"
)

// OpenRouter asks integrators to identify themselves on every call. The
// pair is public attribution, not credentials, so it is a constant.
const (
	headerReferer     = "HTTP-Referer"
	headerTitle       = "X-Title"
	openRouterReferer = "https://github.com/rajeev-chaurasia/pen-stock"
	openRouterTitle   = "Penstock"
)

// errNoBaseURL reports a kind that has no endpoint to guess and did not
// get one from the operator.
var errNoBaseURL = errors.New("base_url is required")

// profile is everything that differs between backends speaking the
// OpenAI wire. Anything not named here behaves identically across them,
// which is why they share one adapter instead of one copy each.
type profile struct {
	// defaultBaseURL applies when the operator configured none. Empty
	// means there is nothing sane to guess and base_url is mandatory.
	defaultBaseURL string

	// headers ride on every request, underneath auth and content type.
	headers map[string]string

	// streamUsage reports whether the backend honors OpenAI's
	// stream_options.include_usage on a streaming request.
	streamUsage bool
}

// profiles is every kind this adapter serves. Adding a vendor that
// speaks the OpenAI wire is a line here, not a new file.
var profiles = map[config.ProviderKind]profile{
	config.KindOpenAI:   {defaultBaseURL: openAIBaseURL, streamUsage: true},
	config.KindGroq:     {defaultBaseURL: groqBaseURL, streamUsage: true},
	config.KindCerebras: {defaultBaseURL: cerebrasBaseURL, streamUsage: true},

	// Mistral does not implement stream_options, so usage on a streamed
	// Mistral call is simply not on offer and asking would risk a 400.
	config.KindMistral: {defaultBaseURL: mistralBaseURL},

	config.KindOpenRouter: {
		defaultBaseURL: openRouterBaseURL,
		headers:        map[string]string{headerReferer: openRouterReferer, headerTitle: openRouterTitle},
		streamUsage:    true,
	},

	// openai_compat covers llmsim, vLLM and anything else self hosted:
	// no address worth guessing, and no promise that an optional field
	// will be tolerated, so nothing is added to its requests either.
	config.KindOpenAICompat: {},
}

func init() {
	for kind := range profiles {
		providers.RegisterKind(kind, builderFor(kind))
	}
}

// builderFor binds one kind's profile into the Builder the factory calls.
func builderFor(kind config.ProviderKind) providers.Builder {
	prof := profiles[kind]
	return func(cfg config.ProviderConfig) (providers.Provider, error) {
		baseURL := cfg.BaseURL
		if baseURL == "" {
			baseURL = prof.defaultBaseURL
		}
		if baseURL == "" {
			return nil, fmt.Errorf("kind %q has no default endpoint: %w", kind, errNoBaseURL)
		}

		// The kind's default is a safe guess about a vendor. An operator
		// running their own backend knows better than the guess, and
		// without usage a streamed request cannot be billed at all.
		effective := prof
		if cfg.StreamUsage != nil {
			effective.streamUsage = *cfg.StreamUsage
		}
		return newWithProfile(cfg.Name, baseURL, cfg.APIKey, effective, nil), nil
	}
}
