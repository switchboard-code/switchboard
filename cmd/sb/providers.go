package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
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
	// generation changes whenever the concrete client configuration changes.
	// A probe captures it with the client and may publish evidence or authorize
	// an asynchronous adoption only while that generation is still current.
	// Each generation also owns a cancellable epoch: concrete clients never
	// escape this registry, and every operation through an escaped providerRef
	// is tied to the epoch which supplied its credentials and endpoint.
	// Guarded by clientsMu.
	generation  uint64
	epoch       context.Context
	cancelEpoch context.CancelCauseFunc
	closed      bool
	ollama      *ollama.Client

	// compat is keyed by serving surface, which for this adapter is also the
	// profile name: two profiles are two different servers with different
	// capabilities, so they cannot share a client.
	compat map[string]*openaicompat.Client

	// openai is keyed by surface: the developer API and the subscription
	// backend are different endpoints with different credentials.
	openai map[string]*openaicompat.Client

	// Messages-compatible clients are still surface-scoped. Two endpoints
	// speaking the same format have different credentials, capability evidence,
	// and prices, so whichever was constructed first must not capture the other.
	anthropic map[string]*anthropic.Client
	kimi      map[string]*anthropic.Client

	// responses serves the subscription surface, which speaks a third wire
	// format and cannot share the compatible client.
	responses *openai.ResponsesClient

	config *config.Config

	// host is the Ollama address the flag asked for, kept so a rebuild after
	// a settings change resolves it against the same precedence as startup.
	// Guarded by clientsMu, which it is an input to.
	host string

	// probes remembers what each serving identity's own probe attested, because a
	// capability the server reported live outranks the catalog's default for
	// its surface — the local surface says vision: false, and the server
	// knows which pulled model actually takes images. Reasoning, temperature,
	// and output-cap parameters change request identity, but not what the
	// provider's model probe attested. Guarded: probes run from the UI's
	// goroutines as well as assembly.
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
	epoch, cancelEpoch := context.WithCancelCause(context.Background())
	return &providers{
		epoch:       epoch,
		cancelEpoch: cancelEpoch,
		ollama:      ollama.New(ollama.WithBaseURL(ollamaHost(host, cfg))),
		compat:      map[string]*openaicompat.Client{},
		openai:      map[string]*openaicompat.Client{},
		anthropic:   map[string]*anthropic.Client{},
		kimi:        map[string]*anthropic.Client{},
		config:      cfg,
		host:        host,
		probes:      map[provider.RouteTargetID]provider.ProbeResult{},
		efforts:     map[string][]string{},
		windows:     map[string]probedWindow{},
	}
}

// bareTargetKey identifies the serving identity a model probe describes. The
// request's inference parameters remain part of RouteTarget.ID for cache,
// pricing, and wire identity, but do not change the model's tool, vision,
// effort-list, or context-window capabilities.
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
	p.resetLocked()
	p.resetEvidence()
	p.clientsMu.Unlock()
}

// resetLocked is reset's body for a caller that already holds clientsMu.
func (p *providers) resetLocked() {
	if p.cancelEpoch != nil {
		// Revocation happens before any replacement is visible. Calls already in
		// flight retain their transport's conservative issued accounting, but no
		// later round can continue with the discarded key or endpoint.
		p.cancelEpoch(errProviderEpochChanged)
	}
	p.generation++
	p.epoch, p.cancelEpoch = context.WithCancelCause(context.Background())
	p.ollama = ollama.New(ollama.WithBaseURL(ollamaHost(p.host, p.config)))
	p.compat = map[string]*openaicompat.Client{}
	p.openai = map[string]*openaicompat.Client{}
	p.anthropic = map[string]*anthropic.Client{}
	p.kimi = map[string]*anthropic.Client{}
	p.responses = nil
}

// resetEvidence drops facts learned from the clients reset just discarded.
// Probe results are evidence about one concrete server configuration, not a
// timeless property of provider/model text. Keeping them across an address or
// credential change can exclude a target before the replacement client ever
// gets a chance to prove itself.
func (p *providers) resetEvidence() {
	p.mu.Lock()
	p.probes = map[provider.RouteTargetID]provider.ProbeResult{}
	p.efforts = map[string][]string{}
	p.windows = map[string]probedWindow{}
	p.mu.Unlock()
}

// adoptOllamaHost takes an address the user chose during the session. It
// supersedes the launch flag rather than losing to it: both are the same
// person naming the same server, and this one was said later.
func (p *providers) adoptOllamaHost(raw string) {
	if p == nil {
		return
	}
	p.clientsMu.Lock()
	p.host = raw
	p.resetLocked()
	p.resetEvidence()
	p.clientsMu.Unlock()
}

// localServer is the Ollama client as it stands now. Callers read it through
// here rather than through the field, because reset replaces it.
func (p *providers) localServer() *ollama.Client {
	p.clientsMu.Lock()
	defer p.clientsMu.Unlock()
	return p.ollama
}

// discoverySnapshot returns a registry whose clients and configuration belong
// only to one asynchronous UI discovery request. A stale worker may finish
// after /setup changes the live registry; it must not keep reading that live
// config or repopulate its client maps beside reset.
func (p *providers) discoverySnapshot(cfg *config.Config) *providers {
	host := ""
	var parentEpoch context.Context
	if p != nil {
		p.clientsMu.Lock()
		parentEpoch = p.epoch
		if p.ollama != nil {
			host = p.ollama.BaseURL()
		}
		p.clientsMu.Unlock()
	}
	snapshot := newProviders(host, cfg)
	if parentEpoch != nil {
		snapshot.clientsMu.Lock()
		snapshot.cancelEpoch(errProviderEpochChanged)
		snapshot.epoch, snapshot.cancelEpoch = context.WithCancelCause(parentEpoch)
		snapshot.clientsMu.Unlock()
	}
	return snapshot
}

// releaseSnapshot detaches an ephemeral discovery registry from the live
// epoch on normal completion. Without it, every /models or /setup visit would
// remain a child of the live context until the next provider reset.
func (p *providers) releaseSnapshot() {
	if p == nil {
		return
	}
	p.clientsMu.Lock()
	if p.closed {
		p.clientsMu.Unlock()
		return
	}
	p.closed = true
	if p.cancelEpoch != nil {
		p.cancelEpoch(context.Canceled)
		p.cancelEpoch = nil
	}
	p.ollama = nil
	p.compat = nil
	p.openai = nil
	p.anthropic = nil
	p.kimi = nil
	p.responses = nil
	p.clientsMu.Unlock()
}

// probedVision reports whether this target's live probe attested image
// input, and whether the target has been probed at all. Unknown is unknown:
// the caller falls back to the catalog, whose entries carry their own
// verification dates.
func (p *providers) probedVision(target provider.RouteTarget) (attested, known bool) {
	probe, known := p.probedCapabilities(target)
	return probe.Vision, known && probe.VisionKnown
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
	probe, ok := p.probes[provider.RouteTargetID(bareTargetKey(target))]
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

// outputTokenAllowance resolves the exact wire allowance without probing or
// reading credentials. Messages targets delegate to the adapter's pure dialect
// policy; every other adapter uses its optional bound capability when one is
// already installed, then the generic explicit/catalog rule.
func (p *providers) outputTokenAllowance(target provider.RouteTarget, catalogMax int) int {
	if target.Provider == anthropic.Name || target.Provider == kimi.Name {
		return effectiveOutputTokenAllowance(nil, target, catalogMax)
	}
	if p != nil {
		p.clientsMu.Lock()
		var bound provider.Provider
		switch target.Provider {
		case ollama.Name:
			bound = p.ollama
		case openaicompat.Name:
			bound = p.compat[target.Surface]
		case openai.Name:
			if target.Surface == openai.Subscription {
				bound = p.responses
			} else {
				bound = p.openai[target.Surface]
			}
		}
		p.clientsMu.Unlock()
		return effectiveOutputTokenAllowance(bound, target, catalogMax)
	}
	return effectiveOutputTokenAllowance(nil, target, catalogMax)
}

// baseURL is the configured address for a target's surface, or empty for the
// adapter's default. It is per surface rather than per provider because one
// provider can front several servers at once (§4).
func (p *providers) baseURL(target provider.RouteTarget) string {
	return p.config.ProviderForTarget(target.Provider, target.Surface).BaseURL
}

func (p *providers) get(ctx context.Context, target provider.RouteTarget) (provider.Provider, error) {
	client, _, err := p.getStamped(ctx, target)
	return client, err
}

var errProviderEpochChanged = errors.New("provider settings changed")

// providerRef is the only provider implementation probeTier lets escape.
// It contains no adapter and therefore no credential or endpoint. Each call
// reacquires the concrete client installed for the registry's current epoch;
// proofGeneration records which probe authorized an asynchronous adoption.
// A reset can invalidate that proof without making a long-lived loop, advisor,
// reviewer, delegate, or race arm retain stale authority.
type providerRef struct {
	registry        *providers
	target          provider.RouteTarget
	proofGeneration uint64
}

func (r *providerRef) Name() string {
	if r == nil {
		return ""
	}
	return r.target.Provider
}

// accepts limits one probe proof to the concrete serving identity and model it
// attested. Inference parameters may differ: /think deliberately reuses the
// same surface/model binding while changing only request-local parameters,
// which the adapter validates independently before transport.
func (r *providerRef) accepts(target provider.RouteTarget) bool {
	return r != nil && target.Provider == r.target.Provider && target.Surface == r.target.Surface &&
		target.ModelID == r.target.ModelID
}

func (r *providerRef) targetError(target provider.RouteTarget) error {
	return provider.MarkUnissued(fmt.Errorf("provider reference for %s cannot serve different target %s; probe that target first",
		r.target.Display(), target.Display()))
}

// ResolveOutputTokenAllowance is pure target policy. It deliberately does not
// acquire a credential-bearing adapter just to answer a local admission check.
func (r *providerRef) ResolveOutputTokenAllowance(target provider.RouteTarget, catalogMax int) (int, error) {
	if target.Provider == anthropic.Name || target.Provider == kimi.Name {
		return anthropic.ResolveOutputTokenAllowance(target)
	}
	return provider.ResolveOutputTokenAllowance(nil, target, catalogMax)
}

func (r *providerRef) OutputTokenAllowance(target provider.RouteTarget, catalogMax int) int {
	allowance, err := r.ResolveOutputTokenAllowance(target, catalogMax)
	if err != nil {
		return math.MaxInt
	}
	return allowance
}

func (r *providerRef) Stream(ctx context.Context, target provider.RouteTarget, req provider.Request) (provider.EventStream, error) {
	if r == nil || r.registry == nil {
		return nil, provider.MarkUnissued(errors.New("provider reference is not bound to a registry"))
	}
	if !r.accepts(target) {
		return nil, r.targetError(target)
	}
	client, call, err := r.registry.acquire(ctx, target)
	if err != nil {
		return nil, provider.MarkUnissued(err)
	}
	stream, err := client.Stream(call.ctx, target, req)
	if err != nil {
		translated := call.translate(err)
		call.release()
		return nil, translated
	}
	return &providerRefStream{inner: stream, call: call}, nil
}

func (r *providerRef) CountTokens(ctx context.Context, target provider.RouteTarget, req provider.Request) (provider.TokenEstimate, error) {
	if r == nil || r.registry == nil {
		return provider.TokenEstimate{}, provider.MarkUnissued(errors.New("provider reference is not bound to a registry"))
	}
	if !r.accepts(target) {
		return provider.TokenEstimate{}, r.targetError(target)
	}
	client, call, err := r.registry.acquire(ctx, target)
	if err != nil {
		return provider.TokenEstimate{}, provider.MarkUnissued(err)
	}
	defer call.release()
	estimate, err := client.CountTokens(call.ctx, target, req)
	return estimate, call.translate(err)
}

func (r *providerRef) Probe(ctx context.Context, target provider.RouteTarget) (provider.ProbeResult, error) {
	if r == nil || r.registry == nil {
		return provider.ProbeResult{}, provider.MarkUnissued(errors.New("provider reference is not bound to a registry"))
	}
	if !r.accepts(target) {
		return provider.ProbeResult{}, r.targetError(target)
	}
	client, call, err := r.registry.acquire(ctx, target)
	if err != nil {
		return provider.ProbeResult{}, provider.MarkUnissued(err)
	}
	defer call.release()
	result, err := client.Probe(call.ctx, target)
	return result, call.translate(err)
}

// providerCall is one operation's authority lease. The child context observes
// both caller cancellation and registry revocation without holding clientsMu
// across network I/O.
type providerCall struct {
	ctx        context.Context
	caller     context.Context
	epoch      context.Context
	generation uint64
	release    func()
}

func newProviderCall(caller, epoch context.Context, generation uint64) *providerCall {
	ctx, cancel := context.WithCancelCause(caller)
	stop := context.AfterFunc(epoch, func() {
		cancel(context.Cause(epoch))
	})
	// AfterFunc schedules an already-cancelled context asynchronously. Apply
	// its cause synchronously too so a discovery snapshot revoked before it is
	// consumed has no check-to-network window.
	if cause := context.Cause(epoch); cause != nil {
		cancel(cause)
	}
	var once sync.Once
	return &providerCall{
		ctx:        ctx,
		caller:     caller,
		epoch:      epoch,
		generation: generation,
		release: func() {
			once.Do(func() {
				stop()
				cancel(context.Canceled)
			})
		},
	}
}

func (c *providerCall) revoked() bool {
	// Read the epoch directly. AfterFunc cancellation is intentionally
	// asynchronous; consulting only the derived call context leaves a window in
	// which an adapter can return a stale successful result immediately after
	// reset cancelled the authoritative epoch.
	return c != nil && c.caller.Err() == nil && errors.Is(context.Cause(c.epoch), errProviderEpochChanged)
}

// providerReconfiguredError is retryable through ErrStreamIncomplete. A
// revocation can happen after transport issuance, so it intentionally does
// not claim RequestIssued=false; the normal retry ledger remains conservative.
type providerReconfiguredError struct{ err error }

func (e *providerReconfiguredError) Error() string {
	if e.err == nil {
		return "provider settings changed while the request was in flight; retry with the current credentials"
	}
	return "provider settings changed while the request was in flight; retry with the current credentials: " + e.err.Error()
}
func (e *providerReconfiguredError) Unwrap() error { return provider.ErrStreamIncomplete }

func (c *providerCall) translate(err error) error {
	// A provider is allowed to finish successfully without observing context
	// cancellation. Success from an epoch that was revoked in the meantime is
	// still stale authority: CountTokens and Probe must not feed old endpoint
	// evidence into a plan that will send through the replacement client.
	if c.revoked() {
		return &providerReconfiguredError{err: err}
	}
	if err == nil && c != nil && c.caller != nil && c.caller.Err() != nil {
		return c.caller.Err()
	}
	return err
}

type providerRefStream struct {
	inner provider.EventStream
	call  *providerCall
	done  bool
	once  sync.Once
}

func (s *providerRefStream) Next() (provider.Event, error) {
	if s == nil || s.inner == nil {
		return provider.Event{}, errors.New("provider stream is not initialized")
	}
	// Once the provider supplied its terminal event, a reset between that event
	// and the required EOF read must not turn an already-complete response into
	// a phantom retry. The wrapper owns that terminal state: do not consult the
	// adapter or the revoked call again, so every later read is the same EOF.
	if s.done {
		return provider.Event{}, io.EOF
	}
	if !s.done && s.call.revoked() {
		return provider.Event{}, &providerReconfiguredError{}
	}
	event, err := s.inner.Next()
	if event.Type == provider.EventDone {
		s.done = true
	}
	if !s.done && s.call.revoked() {
		return provider.Event{}, &providerReconfiguredError{err: err}
	}
	if err != nil && !s.done {
		err = s.call.translate(err)
	}
	return event, err
}

func (s *providerRefStream) Close() error {
	if s == nil {
		return nil
	}
	var err error
	s.once.Do(func() {
		if s.inner != nil {
			err = s.inner.Close()
		}
		if s.call != nil {
			s.call.release()
		}
	})
	return err
}

// acquire resolves a concrete adapter and ties it to the same epoch while
// holding clientsMu. A reset after unlock cancels the operation context before
// net/http can continue on the discarded credential or endpoint.
func (p *providers) acquire(ctx context.Context, target provider.RouteTarget) (provider.Provider, *providerCall, error) {
	if p == nil {
		return nil, nil, errors.New("provider registry is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	p.clientsMu.Lock()
	if p.closed {
		p.clientsMu.Unlock()
		return nil, nil, errors.New("provider discovery snapshot is closed")
	}
	client, err := p.getLocked(ctx, target)
	if err != nil {
		p.clientsMu.Unlock()
		return nil, nil, err
	}
	call := newProviderCall(ctx, p.epoch, p.generation)
	if call.revoked() {
		call.release()
		p.clientsMu.Unlock()
		return nil, nil, &providerReconfiguredError{}
	}
	p.clientsMu.Unlock()
	return client, call, nil
}

func (p *providers) providerRef(target provider.RouteTarget, generation uint64) provider.Provider {
	return &providerRef{registry: p, target: target, proofGeneration: generation}
}

// preparedClientCurrent validates the generation whose probe authorized an
// async result. Non-registry providers are accepted for tests and embeddings;
// only this registry can make a claim about its own proof.
func (p *providers) preparedClientCurrent(client provider.Provider) bool {
	ref, ok := client.(*providerRef)
	if !ok || ref.registry != p {
		return true
	}
	return p.generationIsCurrent(ref.proofGeneration)
}

// getStamped returns a client and the registry generation it belongs to in
// one critical section. Probes need both: reading the generation after get
// returned could label an old client with a generation installed by a reset
// in between the two reads.
func (p *providers) getStamped(ctx context.Context, target provider.RouteTarget) (provider.Provider, uint64, error) {
	// Provider construction is lazy and probes may overlap (routing, advisor,
	// and escalation all run asynchronously). Serialize map access and first
	// construction so callers see one coherent client without concurrent map
	// writes or duplicate OAuth-backed adapters.
	p.clientsMu.Lock()
	defer p.clientsMu.Unlock()
	client, err := p.getLocked(ctx, target)
	return client, p.generation, err
}

// getLocked resolves or constructs a client while the caller holds clientsMu.
func (p *providers) getLocked(ctx context.Context, target provider.RouteTarget) (provider.Provider, error) {
	switch target.Provider {
	case ollama.Name:
		return p.ollama, nil

	case anthropic.Name:
		if err := p.requireExplicitCustomSurface(target, anthropic.Surface); err != nil {
			return nil, err
		}
		if client := p.anthropic[target.Surface]; client != nil {
			return client, nil
		}
		key, err := p.credential(ctx, target)
		if err != nil {
			return nil, err
		}
		client := anthropic.New(
			anthropic.WithAPIKey(key),
			anthropic.WithBaseURL(p.baseURL(target)),
		)
		p.anthropic[target.Surface] = client
		return client, nil

	case kimi.Name:
		if err := p.requireExplicitCustomSurface(target, kimi.Surface); err != nil {
			return nil, err
		}
		if client := p.kimi[target.Surface]; client != nil {
			return client, nil
		}
		key, err := p.credential(ctx, target)
		if err != nil {
			return nil, err
		}
		client := kimi.New(key, anthropic.WithBaseURL(p.baseURL(target)))
		p.kimi[target.Surface] = client
		return client, nil

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
		c, newErr := openai.New(target.Surface, opts...)
		if newErr != nil {
			return nil, fmt.Errorf("target %s: %w", target.Display(), newErr)
		}
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

// requireExplicitCustomSurface keeps a typo from inheriting a provider's
// default endpoint. A custom Messages-compatible surface is supported, but it
// is a claim about one concrete server and therefore needs its own scoped URL.
func (p *providers) requireExplicitCustomSurface(target provider.RouteTarget, defaultSurface string) error {
	if target.Surface == defaultSurface {
		return nil
	}
	if p.config != nil && p.config.Providers[config.ProviderSurfaceKey(target.Provider, target.Surface)].BaseURL != "" {
		return nil
	}
	return fmt.Errorf("target %s names unknown serving surface %q; configure an address for %s to make it a distinct target",
		target.Display(), target.Surface, config.ProviderSurfaceKey(target.Provider, target.Surface))
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
	var generation uint64
	var probe provider.ProbeResult
	published := false
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return config.Tier{}, nil, err
		}
		client, call, err := p.acquire(ctx, tier.Target)
		if err != nil {
			return config.Tier{}, nil, fmt.Errorf("tier %s: %w", tier.ID, err)
		}
		generation = call.generation
		probe, err = client.Probe(call.ctx, tier.Target)
		err = call.translate(err)
		call.release()
		if err != nil {
			if p.generationIsCurrent(generation) {
				return config.Tier{}, nil, err
			}
			continue
		}
		if p.recordProbe(generation, tier.Target, probe) {
			published = true
			break
		}
	}
	if !published {
		return config.Tier{}, nil, fmt.Errorf("provider settings changed repeatedly while probing tier %s; retry the turn", tier.ID)
	}
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
	return tier, p.providerRef(tier.Target, generation), nil
}

func (p *providers) generationIsCurrent(generation uint64) bool {
	p.clientsMu.Lock()
	defer p.clientsMu.Unlock()
	return p.generation == generation
}

// recordProbe atomically checks that the client which produced probe is still
// installed and publishes its evidence. reset takes the same locks in the
// same order, so either the observation lands before reset and is cleared, or
// reset wins and the stale observation is refused.
func (p *providers) recordProbe(generation uint64, target provider.RouteTarget, probe provider.ProbeResult) bool {
	p.clientsMu.Lock()
	defer p.clientsMu.Unlock()
	if p.generation != generation {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probes[provider.RouteTargetID(bareTargetKey(target))] = probe
	if probe.ContextWindow > 0 {
		p.windows[bareTargetKey(target)] = probedWindow{tokens: probe.ContextWindow, enforced: probe.WindowEnforced}
	} else {
		// Same posture as the effort list below: the freshest probe answer is
		// the truth, and a silent one clears what an earlier probe said.
		delete(p.windows, bareTargetKey(target))
	}
	if len(probe.EffortLevels) > 0 {
		p.efforts[bareTargetKey(target)] = probe.EffortLevels
	} else {
		// A fresh probe that states no levels replaces what an earlier one
		// said: the latest answer is the truth, and absent stays absent.
		delete(p.efforts, bareTargetKey(target))
	}
	return true
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

// probeTierFallbackFeasible follows the user-ordered fallback list only when
// the primary cannot pass its live provider probe. A reachable primary that
// fails context, vision, destination, or budget feasibility makes this rung
// infeasible; those are routing constraints, not outages. Once an outage has
// unlocked the list, every substitute still has to pass the same hard checks
// before it may win.
func (p *providers) probeTierFallbackFeasible(ctx context.Context, tier config.Tier, feasible func(config.Tier) error) (config.Tier, provider.Provider, string, error) {
	for index, fallback := range tier.Fallbacks {
		if fallback.Params.MaxOutputTokens != tier.Target.Params.MaxOutputTokens {
			return config.Tier{}, nil, "", fmt.Errorf(
				"tier %s fallback %d has max_output %d, different from the rung's %d",
				tier.ID, index+1, fallback.Params.MaxOutputTokens, tier.Target.Params.MaxOutputTokens)
		}
	}
	turnSpecific := feasible != nil
	probed, client, primaryProbeErr := p.probeTier(ctx, tier)
	if primaryProbeErr == nil {
		if feasible != nil {
			if err := feasible(probed); err != nil {
				return config.Tier{}, nil, "", err
			}
		}
		return probed, client, "", nil
	}
	if len(tier.Fallbacks) == 0 {
		return config.Tier{}, nil, "", primaryProbeErr
	}

	attempts := []string{fmt.Sprintf("%s: %v", tier.Target.Display(), primaryProbeErr)}
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
			tier.ID, fb.Display(), tier.Target.Display(), primaryProbeErr)
		return probed, client, note, nil
	}
	if turnSpecific {
		return config.Tier{}, nil, "", fmt.Errorf("tier %s is unavailable and none of its fallbacks can serve this turn:\n  %s",
			tier.ID, strings.Join(attempts, "\n  "))
	}
	return config.Tier{}, nil, "", fmt.Errorf("tier %s and its fallbacks are all unavailable:\n  %s",
		tier.ID, strings.Join(attempts, "\n  "))
}
