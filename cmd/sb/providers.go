package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/provider/anthropic"
	"github.com/switchboard-code/switchboard/internal/provider/kimi"
	"github.com/switchboard-code/switchboard/internal/provider/ollama"
	"github.com/switchboard-code/switchboard/internal/provider/openai"
	"github.com/switchboard-code/switchboard/internal/provider/openaicompat"
)

// providers binds a route target to the adapter that can serve it.
//
// Every target the loop can run passes through here, so a tier naming a
// provider this build has no adapter for fails at startup with a list of what
// it does have, rather than partway through the first turn.
type providers struct {
	clientsMu sync.Mutex
	ollama    *ollama.Client

	// compat is keyed by serving surface, which for this adapter is also the
	// profile name: two profiles are two different servers with different
	// capabilities, so they cannot share a client.
	compat map[string]*openaicompat.Client

	// openai is keyed by surface: the developer API and the subscription
	// backend are different endpoints with different credentials.
	openai map[string]*openaicompat.Client

	anthropic *anthropic.Client
	kimi      *anthropic.Client

	// responses serves the subscription surface, which speaks a third wire
	// format and cannot share the compatible client.
	responses *openai.ResponsesClient

	config *config.Config

	// host is the Ollama address the flag asked for, kept so a rebuild after
	// a settings change resolves it against the same precedence as startup.
	// Guarded by clientsMu, which it is an input to.
	host string

	// probes remembers what each target's own probe attested, because a
	// capability the server reported live outranks the catalog's default for
	// its surface — the local surface says vision: false, and the server
	// knows which pulled model actually takes images. Guarded: probes run
	// from the UI's goroutines as well as assembly.
	mu     sync.Mutex
	probes map[provider.RouteTargetID]provider.ProbeResult

	// efforts remembers the reasoning levels a model's own server stated,
	// keyed free of inference parameters: /think rebinds the target under a
	// new parameterized identity when the effort changes, and the list the
	// server gave for the model does not move with it.
	efforts map[string][]string

	// windows is the same store for the attested context window, for the
	// same reason: auto-compaction cannot arm against a number that
	// evaporates when an effort change rebinds the target.
	windows map[string]probedWindow
}

// probedWindow is an attested context window and whether the server enforces
// it, because only an enforced window outranks the user's declared one.
type probedWindow struct {
	tokens   int
	enforced bool
}

func newProviders(host string, cfg *config.Config) *providers {
	return &providers{
		ollama:  ollama.New(ollama.WithBaseURL(ollamaHost(host, cfg))),
		compat:  map[string]*openaicompat.Client{},
		openai:  map[string]*openaicompat.Client{},
		config:  cfg,
		host:    host,
		probes:  map[provider.RouteTargetID]provider.ProbeResult{},
		efforts: map[string][]string{},
		windows: map[string]probedWindow{},
	}
}

// bareTargetKey identifies a model on a surface with the request's inference
// parameters stripped, which is the identity a stated effort list or context
// window attaches to.
func bareTargetKey(target provider.RouteTarget) string {
	bare := provider.RouteTarget{Provider: target.Provider, Surface: target.Surface, ModelID: target.ModelID}
	return string(bare.ID())
}

// ollamaHost resolves which server the local adapter talks to: the flag, then
// the config, then whatever the client itself reads from the environment. The
// flag wins because it was typed for this run; an empty result is deliberate
// and means "leave the environment alone".
func ollamaHost(flagHost string, cfg *config.Config) string {
	if strings.TrimSpace(flagHost) != "" {
		return flagHost
	}
	if cfg != nil {
		return cfg.ProviderFor(ollama.Name).BaseURL
	}
	return ""
}

// reset drops every cached client so the next request is built against the
// settings as they stand now. A base URL entered in /setup or a key stored
// mid-session reaches an adapter that was already constructed only if the
// construction happens again; without this, the checklist would report a
// change that the next probe does not act on.
func (p *providers) reset() {
	if p == nil {
		return
	}
	p.clientsMu.Lock()
	defer p.clientsMu.Unlock()
	p.resetLocked()
}

// resetLocked is reset's body for a caller that already holds clientsMu.
func (p *providers) resetLocked() {
	p.ollama = ollama.New(ollama.WithBaseURL(ollamaHost(p.host, p.config)))
	p.compat = map[string]*openaicompat.Client{}
	p.openai = map[string]*openaicompat.Client{}
	p.anthropic = nil
	p.kimi = nil
	p.responses = nil
}

// adoptOllamaHost takes an address the user chose during the session. It
// supersedes the launch flag rather than losing to it: both are the same
// person naming the same server, and this one was said later.
func (p *providers) adoptOllamaHost(raw string) {
	if p == nil {
		return
	}
	p.clientsMu.Lock()
	defer p.clientsMu.Unlock()
	p.host = raw
	p.resetLocked()
}

// localServer is the Ollama client as it stands now. Callers read it through
// here rather than through the field, because reset replaces it.
func (p *providers) localServer() *ollama.Client {
	p.clientsMu.Lock()
	defer p.clientsMu.Unlock()
	return p.ollama
}

// probedVision reports whether this target's live probe attested image
// input, and whether the target has been probed at all. Unknown is unknown:
// the caller falls back to the catalog, whose entries carry their own
// verification dates.
func (p *providers) probedVision(target provider.RouteTarget) (attested, known bool) {
	probe, known := p.probedCapabilities(target)
	return probe.Vision, known
}

// probedCapabilities returns the immutable result of the latest live probe.
// Routing reads this snapshot rather than holding the registry lock while it
// scores candidates.
func (p *providers) probedCapabilities(target provider.RouteTarget) (provider.ProbeResult, bool) {
	if p == nil {
		return provider.ProbeResult{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	probe, ok := p.probes[target.ID()]
	return probe, ok
}

// probedContextWindow reports the window this target's server attested, or
// zero when it said nothing, and whether the server enforces that number.
// A live answer outranks a catalog default for the same reason a probed
// capability does: the catalog describes a surface, and the server knows
// which model is loaded on it. The read is parameter-independent: /think
// rebinds the target under a new identity, and the window does not move with
// the effort.
func (p *providers) probedContextWindow(target provider.RouteTarget) (int, bool) {
	if p == nil {
		return 0, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	w := p.windows[bareTargetKey(target)]
	return w.tokens, w.enforced
}

// probedEffortLevels reports the reasoning efforts this target's server
// listed for the model, in the server's order. The second return is false
// where no probe carried a list, which is the caller's cue to fall back to
// the catalog's surface floor rather than treat silence as a miss.
func (p *providers) probedEffortLevels(target provider.RouteTarget) ([]string, bool) {
	if p == nil {
		return nil, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	levels, ok := p.efforts[bareTargetKey(target)]
	return levels, ok
}

// baseURL is the configured address for a target's surface, or empty for the
// adapter's default. It is per surface rather than per provider because one
// provider can front several servers at once (§4).
func (p *providers) baseURL(target provider.RouteTarget) string {
	return p.config.ProviderForTarget(target.Provider, target.Surface).BaseURL
}

func (p *providers) get(ctx context.Context, target provider.RouteTarget) (provider.Provider, error) {
	// Provider construction is lazy and probes may overlap (routing, advisor,
	// and escalation all run asynchronously). Serialize map access and first
	// construction so callers see one coherent client without concurrent map
	// writes or duplicate OAuth-backed adapters.
	p.clientsMu.Lock()
	defer p.clientsMu.Unlock()
	switch target.Provider {
	case ollama.Name:
		return p.ollama, nil

	case anthropic.Name:
		if p.anthropic != nil {
			return p.anthropic, nil
		}
		key, err := p.credential(ctx, target)
		if err != nil {
			return nil, err
		}
		p.anthropic = anthropic.New(
			anthropic.WithAPIKey(key),
			anthropic.WithBaseURL(p.baseURL(target)),
		)
		return p.anthropic, nil

	case kimi.Name:
		if p.kimi != nil {
			return p.kimi, nil
		}
		key, err := p.credential(ctx, target)
		if err != nil {
			return nil, err
		}
		p.kimi = kimi.New(key, anthropic.WithBaseURL(p.baseURL(target)))
		return p.kimi, nil

	case openai.Name:
		if target.Surface == openai.Subscription {
			// A different wire format, so a different client. The compatible
			// one cannot serve this endpoint at all.
			if p.responses != nil {
				return p.responses, nil
			}
			token, err := p.credential(ctx, target)
			if err != nil {
				return nil, err
			}
			p.responses = openai.NewResponses(
				openai.WithResponsesToken(token),
				openai.WithResponsesBaseURL(p.baseURL(target)),
			)
			return p.responses, nil
		}
		if c, ok := p.openai[target.Surface]; ok {
			return c, nil
		}
		opts, err := p.authOptions(ctx, target)
		if err != nil {
			return nil, err
		}
		if base := p.baseURL(target); base != "" {
			opts = append(opts, openaicompat.WithBaseURL(base))
		}
		c := openai.New(target.Surface, opts...)
		p.openai[target.Surface] = c
		return c, nil

	case openaicompat.Name:
		if c, ok := p.compat[target.Surface]; ok {
			return c, nil
		}
		opts, err := p.authOptions(ctx, target)
		if err != nil {
			return nil, err
		}
		switch {
		case p.baseURL(target) != "":
			opts = append(opts, openaicompat.WithBaseURL(p.baseURL(target)))
		case target.Surface == "ollama":
			// The same server, reached through its compatibility endpoint. The
			// host was already resolved from the flag and the environment for
			// the native adapter; resolving it twice invites the two to
			// disagree about which server the user meant.
			opts = append(opts, openaicompat.WithBaseURL(p.ollama.BaseURL()+"/v1"))
		}
		c, newErr := openaicompat.New(target.Surface, opts...)
		if newErr != nil {
			return nil, fmt.Errorf(
				"target %s names serving surface %q, which is not a profile this build has tested: %w",
				target.Display(), target.Surface, newErr)
		}
		p.compat[target.Surface] = c
		return c, nil
	}

	return nil, fmt.Errorf(
		"target %s names provider %q; this build has adapters for %s, %s, %s, %s, and %s",
		target.Display(), target.Provider,
		anthropic.Name, kimi.Name, ollama.Name, openai.Name, openaicompat.Name)
}

// authOptions resolves the credential for a target, if there is one to find.
//
// A missing credential is not an error here. Every profile this build ships
// points at a local server that wants no authorization, and refusing to start
// without a key nobody needs would be worse than useless.
//
// Nor does an absent credential get mentioned when a probe fails: on a local
// server there is correctly nothing to find, so pointing at authentication
// would send the user to `sb auth login` when the real answer is `ollama pull`.
// Turning a rejection into "you have no credential" needs a server that can
// actually issue one, and this build has no adapter that reaches such a server.
// That message gets written against a real 401 rather than a guess at one.
func (p *providers) credential(ctx context.Context, target provider.RouteTarget) (string, error) {
	ref := credential.Ref{Provider: target.Provider, Account: target.Surface}
	resolver := credential.Chain(authSettings(p.config, target))

	secret, err := resolver.Get(ctx, ref)
	if err != nil {
		if errors.Is(err, credential.ErrNotFound) {
			return "", nil
		}
		// A configured helper that is present and broken is the user's problem
		// to fix, and starting without the key it would have supplied only
		// moves the failure somewhere less legible.
		return "", err
	}
	// Exposed at the point of use and handed straight to the adapter, which is
	// the only place a credential is meant to be a plain string.
	return secret.Expose(), nil
}

// authOptions adapts credential resolution for the two adapters built on the
// OpenAI-compatible client.
func (p *providers) authOptions(ctx context.Context, target provider.RouteTarget) ([]openaicompat.Option, error) {
	key, err := p.credential(ctx, target)
	if err != nil || key == "" {
		return nil, err
	}
	return []openaicompat.Option{openaicompat.WithAPIKey(key)}, nil
}

// authSettings resolves the auth configuration for a target, filling in a
// bundled OAuth client where one exists and the user has not named their own.
//
// Configuration always wins. A user who registers their own client and writes
// it down uses theirs, and the bundled one is only what makes a surface work
// without any configuration at all.
func authSettings(cfg *config.Config, target provider.RouteTarget) credential.Settings {
	settings := cfg.AuthFor(target.Provider)
	if settings.OAuth.ClientID != "" {
		return settings
	}
	if target.Provider == openai.Name {
		if bundled := openai.DefaultOAuth(target.Surface); bundled.ClientID != "" {
			settings.OAuth = bundled
		}
	}
	return settings
}

// servedByOllama reports whether a target reaches an Ollama server, whether
// through the native API or the compatibility endpoint. It decides only whether
// "ollama pull" is useful advice.
func servedByOllama(target provider.RouteTarget) bool {
	return target.Provider == ollama.Name ||
		(target.Provider == openaicompat.Name && target.Surface == "ollama")
}

// probeTier confirms the target can actually drive the loop before a turn
// starts, so a missing model is an error now rather than halfway through.
func (p *providers) probeTier(ctx context.Context, tier config.Tier) (config.Tier, provider.Provider, error) {
	client, err := p.get(ctx, tier.Target)
	if err != nil {
		return config.Tier{}, nil, fmt.Errorf("tier %s: %w", tier.ID, err)
	}

	probe, err := client.Probe(ctx, tier.Target)
	if err != nil {
		return config.Tier{}, nil, err
	}
	p.mu.Lock()
	p.probes[tier.Target.ID()] = probe
	if probe.ContextWindow > 0 {
		p.windows[bareTargetKey(tier.Target)] = probedWindow{tokens: probe.ContextWindow, enforced: probe.WindowEnforced}
	} else {
		// Same posture as the effort list below: the freshest probe answer is
		// the truth, and a silent one clears what an earlier probe said.
		delete(p.windows, bareTargetKey(tier.Target))
	}
	if len(probe.EffortLevels) > 0 {
		p.efforts[bareTargetKey(tier.Target)] = probe.EffortLevels
	} else {
		// A fresh probe that states no levels replaces what an earlier one
		// said: the latest answer is the truth, and absent stays absent.
		delete(p.efforts, bareTargetKey(tier.Target))
	}
	p.mu.Unlock()
	switch {
	case !probe.Reachable:
		return config.Tier{}, nil, fmt.Errorf("no server responded for %s: %s", tier.Target.Display(), probe.Detail)
	case !probe.ModelPresent:
		if servedByOllama(tier.Target) {
			return config.Tier{}, nil, fmt.Errorf("%s\nrun: ollama pull %s", probe.Detail, tier.Target.ModelID)
		}
		return config.Tier{}, nil, fmt.Errorf("%s", probe.Detail)
	case probe.Tools == provider.ToolsNone:
		return config.Tier{}, nil, fmt.Errorf(
			"%s does not support tool calling, so it cannot drive the agent loop", tier.Target.ModelID)
	}
	return tier, client, nil
}

// probeTierFallback serves a tier from its primary target, or failing that
// from the first fallback that answers (§5.4). Fallback is an availability
// event, not a routing decision: the order is the user's, every entry was
// written into their config — which is what makes each an approved
// destination — and each candidate passes the same probe as a primary, so a
// fallback that cannot call tools is refused the same way. The returned
// note is the visible substitution the design requires; it is empty when
// the primary served, and the caller renders it before any content is sent
// and records it on the session.
func (p *providers) probeTierFallback(ctx context.Context, tier config.Tier) (config.Tier, provider.Provider, string, error) {
	return p.probeTierFallbackFeasible(ctx, tier, nil)
}

// probeTierFallbackFeasible keeps following the user-ordered fallback list
// when a reachable target still cannot serve this concrete turn. Availability
// is only one hard requirement; context, vision, and budget apply to every
// substitute before it may win.
func (p *providers) probeTierFallbackFeasible(ctx context.Context, tier config.Tier, feasible func(config.Tier) error) (config.Tier, provider.Provider, string, error) {
	turnSpecific := feasible != nil
	probed, client, primaryErr := p.probeTier(ctx, tier)
	if primaryErr == nil && feasible != nil {
		primaryErr = feasible(probed)
	}
	if primaryErr == nil {
		return probed, client, "", nil
	}
	if len(tier.Fallbacks) == 0 {
		return config.Tier{}, nil, "", primaryErr
	}

	attempts := []string{fmt.Sprintf("%s: %v", tier.Target.Display(), primaryErr)}
	for _, fb := range tier.Fallbacks {
		sub := tier
		sub.Target = fb
		probed, client, err := p.probeTier(ctx, sub)
		if err == nil && feasible != nil {
			err = feasible(probed)
		}
		if err != nil {
			attempts = append(attempts, fmt.Sprintf("%s: %v", fb.Display(), err))
			continue
		}
		note := fmt.Sprintf("%s is served by its fallback %s: %s is unavailable (%v)",
			tier.ID, fb.Display(), tier.Target.Display(), primaryErr)
		if turnSpecific {
			note = fmt.Sprintf("%s is served by its fallback %s: %s could not serve this turn (%v)",
				tier.ID, fb.Display(), tier.Target.Display(), primaryErr)
		}
		return probed, client, note, nil
	}
	if turnSpecific {
		return config.Tier{}, nil, "", fmt.Errorf("tier %s and its fallbacks cannot serve this turn:\n  %s",
			tier.ID, strings.Join(attempts, "\n  "))
	}
	return config.Tier{}, nil, "", fmt.Errorf("tier %s and its fallbacks are all unavailable:\n  %s",
		tier.ID, strings.Join(attempts, "\n  "))
}
