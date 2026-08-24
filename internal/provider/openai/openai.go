// Package openai binds OpenAI's own API as a route target.
//
// The wire format is the one `openaicompat` already speaks, so this package is
// deliberately thin: it supplies the endpoints, the profiles, and the identity,
// and reuses the decoder that was written against a recorded capture.
//
// It is a provider in its own right rather than a profile of the compatible
// adapter, because target identity is provider plus surface plus model (§4).
// "Reached through a format OpenAI also invented" is a fact about this
// codebase's implementation, not about where the request goes, what it costs,
// or whose key pays for it.
package openai

import (
	"fmt"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/provider/openaicompat"
)

const Name = "openai"

// The two serving surfaces differ in every way the surface field exists to
// capture: a different endpoint, a different credential, and a different
// billing model. They are not interchangeable and are not the same target.
const (
	// FirstParty is the developer API, paid per token against an API key.
	FirstParty = "first-party"

	// Subscription is the backend behind a ChatGPT plan, reached with an OAuth
	// token and billed as a flat subscription rather than per token.
	Subscription = "subscription"
)

const (
	FirstPartyBaseURL   = "https://api.openai.com/v1"
	SubscriptionBaseURL = "https://chatgpt.com/backend-api/codex"
)

// SubscriptionOAuth is the client this build presents when signing in to a
// ChatGPT plan.
//
// The client ID is the one OpenAI's own Codex CLI registers. Switchboard is not
// affiliated with or endorsed by OpenAI, and this is not a flow OpenAI has
// published for third-party clients; it works because the authorization server
// accepts the registration, not because anyone was given permission to use it.
// The consequences land on the account that signs in, and OpenAI's Terms of Use
// govern that account regardless of what this program presents itself as.
//
// It is overridable. Anything set under [auth.openai.oauth] wins, so a user who
// registers their own client uses theirs.
var SubscriptionOAuth = credential.OAuthSettings{
	ClientID:     "app_EMoamEEZ73f0CkXaXp7hrann",
	AuthorizeURL: "https://auth.openai.com/oauth/authorize",
	TokenURL:     "https://auth.openai.com/oauth/token",
	Scopes:       []string{"openid", "profile", "email", "offline_access"},

	// The registration pins this exactly: a different port, a different path,
	// or the literal 127.0.0.1 in place of "localhost" is rejected with an
	// authentication error that names none of those things.
	RedirectURI: "http://localhost:1455/auth/callback",
}

// profiles record what each endpoint actually does.
//
// The developer API profile has not been run against the live service, so it
// claims the floor:
// tools, because that is what the adapter is for, and nothing else. Reasoning
// is left unsupported so that asking for it is a capability error the caller
// sees rather than a parameter silently dropped, which would return a cheaper,
// worse answer looking like a correct one. Both get filled in from a capture.
var profiles = map[string]openaicompat.Profile{
	FirstParty: {
		Provider:    Name,
		BaseURL:     FirstPartyBaseURL,
		Tools:       true,
		StreamUsage: true,
	},
}

// New builds a client for a serving surface. A surface is a wire/capability
// profile, not a label: an unknown value is refused instead of silently
// becoming the developer API, and subscription is served only by NewResponses.
//
// Subscription is not served here and cannot be: that endpoint speaks the
// Responses API, which is a different wire format. NewResponses serves it.
func New(surface string, opts ...openaicompat.Option) (*openaicompat.Client, error) {
	profile, ok := profiles[surface]
	if !ok {
		return nil, fmt.Errorf("OpenAI serving surface %q is not a tested chat-completions profile", surface)
	}
	return openaicompat.NewFor(surface, profile, opts...), nil
}

// SubscriptionNotes records what the endpoint wants, captured from it rather
// than from documentation. ResponsesClient is written against these, and they
// are kept as the account of where each behaviour came from.
//
// The cache surface is the reason this is worth building: it reports written
// and cached tokens separately, and unlike any other target here it accepts a
// caller-supplied cache routing key with a stated retention. §6.2 wants exactly
// that affinity key and no target has offered one before.
const SubscriptionNotes = `
  endpoint   POST /backend-api/codex/responses?client_version=<x.y.z>
             a version below some floor returns an empty model list rather
             than an error, which reads as "no models" instead of "too old"
  discovery  GET /backend-api/codex/models?client_version=<x.y.z>
             returns slugs with per-model capabilities: input modalities,
             verbosity support, and tool shapes
  body       input is a list of typed message objects, not a string
             store must be false or the request is refused
  stream     Responses API SSE: response.created, response.output_text.delta,
             response.output_item.done, response.completed
  usage      input_tokens, output_tokens, and separately
             input_tokens_details.{cached_tokens, cache_write_tokens},
             plus output_tokens_details.reasoning_tokens
  cache      prompt_cache_key is a caller-supplied routing key and
             prompt_cache_retention was 24h`

// DefaultBaseURL reports where a surface is reached before any configured
// override, so a caller can tell whether one is in effect without knowing the
// address.
func DefaultBaseURL(surface string) string {
	switch surface {
	case FirstParty:
		return FirstPartyBaseURL
	case Subscription:
		return SubscriptionBaseURL
	default:
		return ""
	}
}

// DefaultOAuth returns the bundled client for a surface, which exists only for
// the subscription one. The developer API takes an API key and has no login
// flow to offer.
func DefaultOAuth(surface string) credential.OAuthSettings {
	if surface == Subscription {
		return SubscriptionOAuth
	}
	return credential.OAuthSettings{}
}

func Target(model string) provider.RouteTarget {
	return provider.RouteTarget{Provider: Name, Surface: FirstParty, ModelID: model}
}

func SubscriptionTarget(model string) provider.RouteTarget {
	return provider.RouteTarget{Provider: Name, Surface: Subscription, ModelID: model}
}
