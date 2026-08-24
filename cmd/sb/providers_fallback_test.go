package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/provider/anthropic"
	"github.com/switchboard-code/switchboard/internal/provider/kimi"
	"github.com/switchboard-code/switchboard/internal/provider/openai"
	route "github.com/switchboard-code/switchboard/internal/router"
)

func TestProviderClientCacheIsConcurrentAndCoherent(t *testing.T) {
	p := newProviders("http://127.0.0.1:11434", &config.Config{})
	targets := []provider.RouteTarget{
		{Provider: "openaicompat", Surface: "ollama", ModelID: "m"},
		{Provider: "openaicompat", Surface: "generic", ModelID: "m"},
	}
	var wg sync.WaitGroup
	results := make([][]provider.Provider, len(targets))
	for index, target := range targets {
		for range 32 {
			wg.Add(1)
			go func(index int, target provider.RouteTarget) {
				defer wg.Done()
				client, err := p.get(context.Background(), target)
				if err != nil {
					t.Errorf("get(%s): %v", target.Surface, err)
					return
				}
				p.clientsMu.Lock()
				results[index] = append(results[index], client)
				p.clientsMu.Unlock()
			}(index, target)
		}
	}
	wg.Wait()
	for index, clients := range results {
		if len(clients) != 32 {
			t.Fatalf("surface %s returned %d clients", targets[index].Surface, len(clients))
		}
		for _, client := range clients[1:] {
			if client != clients[0] {
				t.Fatalf("surface %s constructed duplicate clients", targets[index].Surface)
			}
		}
	}
}

func TestProviderRegistryScopesMessagesClientsByServingSurface(t *testing.T) {
	for _, tc := range []struct {
		name, providerName, defaultSurface string
	}{
		{name: "anthropic", providerName: anthropic.Name, defaultSurface: anthropic.Surface},
		{name: "kimi", providerName: kimi.Name, defaultSurface: kimi.Surface},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := func(model string) *httptest.Server {
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/v1/models" {
						http.NotFound(w, r)
						return
					}
					_, _ = fmt.Fprintf(w, `{"data":[{"id":%q}]}`, model)
				}))
				t.Cleanup(ts.Close)
				return ts
			}
			first, second := server("model-a"), server("model-b")
			customSurface := "private-gateway"
			cfg := &config.Config{}
			cfg.SetProviderBaseURL(config.ProviderSurfaceKey(tc.providerName, tc.defaultSurface), first.URL)
			cfg.SetProviderBaseURL(config.ProviderSurfaceKey(tc.providerName, customSurface), second.URL)
			registry := newProviders("", cfg)
			firstTier := config.Tier{ID: "t1", Target: provider.RouteTarget{
				Provider: tc.providerName, Surface: tc.defaultSurface, ModelID: "model-a",
			}}
			secondTier := config.Tier{ID: "t2", Target: provider.RouteTarget{
				Provider: tc.providerName, Surface: customSurface, ModelID: "model-b",
			}}
			_, firstClient, err := registry.probeTier(context.Background(), firstTier)
			if err != nil {
				t.Fatal(err)
			}
			_, secondClient, err := registry.probeTier(context.Background(), secondTier)
			if err != nil {
				t.Fatal(err)
			}
			if firstClient == secondClient {
				t.Fatal("two serving surfaces shared one Messages client")
			}
		})
	}
}

func TestProvidersRejectUnknownOpenAISurfaceBeforeNetwork(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	t.Cleanup(server.Close)
	cfg := &config.Config{}
	cfg.SetProviderBaseURL(config.ProviderSurfaceKey(openai.Name, "firstparty"), server.URL)
	registry := newProviders("", cfg)
	tier := config.Tier{ID: "t1", Target: provider.RouteTarget{
		Provider: openai.Name, Surface: "firstparty", ModelID: "gpt-test",
	}}
	if _, _, err := registry.probeTier(context.Background(), tier); err == nil || !strings.Contains(err.Error(), "not a tested") {
		t.Fatalf("unknown OpenAI surface error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("unknown OpenAI surface made %d network calls", calls)
	}
}

func TestProviderResetInvalidatesLiveEvidence(t *testing.T) {
	p := newProviders("http://127.0.0.1:11434", &config.Config{})
	target := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "moving"}
	p.mu.Lock()
	p.probes[target.ID()] = provider.ProbeResult{Reachable: true, ModelPresent: true, Tools: provider.ToolsParallel, Vision: true}
	p.efforts[bareTargetKey(target)] = []string{"low", "high"}
	p.windows[bareTargetKey(target)] = probedWindow{tokens: 32_768, enforced: true}
	p.mu.Unlock()

	p.reset()

	if _, known := p.probedCapabilities(target); known {
		t.Fatal("a provider reset retained capability evidence from the discarded client")
	}
	if _, known := p.probedEffortLevels(target); known {
		t.Fatal("a provider reset retained effort evidence from the discarded client")
	}
	if window, enforced := p.probedContextWindow(target); window != 0 || enforced {
		t.Fatalf("a provider reset retained context evidence: window=%d enforced=%v", window, enforced)
	}
}

func TestResetDuringProbeCannotRepublishDiscardedClientEvidence(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			startOnce.Do(func() { close(started) })
			<-release
			_, _ = w.Write([]byte(`{"models":[{"name":"moving"}]}`))
		case "/api/show":
			_, _ = w.Write([]byte(`{"capabilities":["tools","vision"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(old.Close)
	current := fakeOllama(t, "moving") // tool capable, but no vision evidence

	registry := newProviders(old.URL, &config.Config{})
	tier := ollamaTier("t1", "moving")
	type answer struct {
		client provider.Provider
		err    error
	}
	done := make(chan answer, 1)
	go func() {
		_, client, err := registry.probeTier(context.Background(), tier)
		done <- answer{client: client, err: err}
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("old provider probe did not start")
	}
	registry.adoptOllamaHost(current.URL)
	close(release)

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if _, ok := got.client.(*providerRef); !ok {
			t.Fatalf("probe returned %T, want registry-owned provider reference", got.client)
		}
		if !registry.preparedClientCurrent(got.client) {
			t.Fatal("probe returned authority from the client generation discarded by reset")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not retry against the replacement provider")
	}
	probe, known := registry.probedCapabilities(tier.Target)
	if !known {
		t.Fatal("replacement provider evidence was not recorded")
	}
	if probe.Vision {
		t.Fatal("discarded provider's vision evidence survived the reset")
	}
}

func TestProbeCancellationInterruptsCredentialResolution(t *testing.T) {
	if os.Getenv("SB_TEST_BLOCKING_CREDENTIAL_HELPER") == "1" {
		_ = os.WriteFile(os.Getenv("SB_TEST_CREDENTIAL_HELPER_STARTED"), []byte("started"), 0o600)
		for {
			time.Sleep(time.Hour)
		}
	}

	marker := filepath.Join(t.TempDir(), "started")
	t.Setenv("SB_TEST_BLOCKING_CREDENTIAL_HELPER", "1")
	t.Setenv("SB_TEST_CREDENTIAL_HELPER_STARTED", marker)
	cfg := &config.Config{Auth: map[string]credential.Settings{
		anthropic.Name: {Helper: []string{os.Args[0], "-test.run=^TestProbeCancellationInterruptsCredentialResolution$"}},
	}}
	registry := newProviders("http://127.0.0.1:1", cfg)
	target := provider.RouteTarget{Provider: anthropic.Name, Surface: "first-party", ModelID: "test"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := registry.probeTier(ctx, config.Tier{ID: "t1", Target: target})
		done <- err
	}()

	// A cold whole-repository -race run may still be compiling/linking other
	// packages while this child test binary starts. The marker proves the helper
	// actually reached its blocking point; give process startup enough headroom
	// without weakening the prompt-cancellation deadline below.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("credential helper did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("probe error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled credential lookup did not return promptly")
	}
	if len(registry.anthropic) != 0 {
		t.Fatal("cancelled credential lookup installed a provider client")
	}
	if _, ok := registry.probedCapabilities(target); ok {
		t.Fatal("cancelled credential lookup installed probe evidence")
	}
}

func TestProbeEvidenceFollowsServingIdentityAcrossInferenceParameters(t *testing.T) {
	server := fakeOllama(t, "same-model")
	registry := newProviders(server.URL, &config.Config{})
	base := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "same-model"}
	withMax := base
	withMax.Params.MaxOutputTokens = 2_048
	temperature := 0.2
	withTemperature := base
	withTemperature.Params.Temperature = &temperature

	for index, target := range []provider.RouteTarget{withMax, withTemperature} {
		if _, _, err := registry.probeTier(context.Background(), config.Tier{ID: fmt.Sprintf("t%d", index+1), Target: target}); err != nil {
			t.Fatalf("probe %s: %v", target.ID(), err)
		}
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if len(registry.probes) != 1 {
		t.Fatalf("parameter variants produced %d probe entries, want one serving identity", len(registry.probes))
	}
	if _, ok := registry.probes[provider.RouteTargetID(bareTargetKey(base))]; !ok {
		t.Fatalf("serving-identity probe was lost: %#v", registry.probes)
	}
}

func TestProbeCapabilitiesSurviveThinkAndMaxOutputRebindsUntilReset(t *testing.T) {
	server := fakeOllama(t, "custom-unpriced")
	registry := newProviders(server.URL, &config.Config{})
	base := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "custom-unpriced"}
	if _, _, err := registry.probeTier(context.Background(), config.Tier{ID: "t1", Target: base}); err != nil {
		t.Fatal(err)
	}

	variants := []provider.RouteTarget{base}
	withThink := base
	withThink.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}
	variants = append(variants, withThink)
	withMax := base
	withMax.Params.MaxOutputTokens = 4096
	variants = append(variants, withMax)
	for _, target := range variants {
		probe, ok := registry.probedCapabilities(target)
		if !ok || !probe.Reachable || !probe.ModelPresent || probe.Tools == provider.ToolsNone {
			t.Fatalf("parameterized target %s lost live capabilities: %+v known=%v", target.ID(), probe, ok)
		}
	}

	registry.reset()
	for _, target := range variants {
		if _, ok := registry.probedCapabilities(target); ok {
			t.Fatalf("provider generation reset retained evidence for %s", target.ID())
		}
	}
}

// fakeOllama serves a tags list holding only the named models, each
// advertising tool support, which is what the probe needs to accept one.
func fakeOllama(t *testing.T, models ...string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			var entries []string
			for _, m := range models {
				entries = append(entries, `{"name":"`+m+`"}`)
			}
			w.Write([]byte(`{"models":[` + strings.Join(entries, ",") + `]}`))
		case "/api/show":
			w.Write([]byte(`{"capabilities":["tools"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func ollamaTier(id, model string, fallbacks ...string) config.Tier {
	tier := config.Tier{
		ID:     id,
		Target: provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: model},
	}
	for _, fb := range fallbacks {
		tier.Fallbacks = append(tier.Fallbacks, provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: fb})
	}
	return tier
}

func TestProbeTierFallbackServesTheFirstAnswer(t *testing.T) {
	ts := fakeOllama(t, "backup")
	p := newProviders(ts.URL, &config.Config{})

	tier, _, note, err := p.probeTierFallback(context.Background(), ollamaTier("t2", "missing", "also-missing", "backup"))
	if err != nil {
		t.Fatal(err)
	}
	if tier.Target.ModelID != "backup" {
		t.Fatalf("served %s, want the fallback that answered", tier.Target.ID())
	}
	if tier.ID != "t2" {
		t.Errorf("tier ID = %s, want the rung unchanged: fallback is availability, not routing", tier.ID)
	}
	if !strings.Contains(note, "t2 is served by its fallback") || !strings.Contains(note, "missing") {
		t.Errorf("note = %q, want the substitution and its reason named", note)
	}
}

func TestProbeTierFallbackRejectsMismatchedRungCap(t *testing.T) {
	ts := fakeOllama(t, "primary", "backup")
	p := newProviders(ts.URL, &config.Config{})
	tier := ollamaTier("t1", "primary", "backup")
	tier.Target.Params.MaxOutputTokens = 4096
	tier.Fallbacks[0].Params.MaxOutputTokens = 2048

	_, _, _, err := p.probeTierFallback(context.Background(), tier)
	if err == nil || !strings.Contains(err.Error(), "fallback 1 has max_output 2048") ||
		!strings.Contains(err.Error(), "rung's 4096") {
		t.Fatalf("mismatched rung cap error = %v", err)
	}
}

func TestDelegateFallbackMustPassDestinationPolicy(t *testing.T) {
	local := fakeOllama(t) // the approved primary is unavailable
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-5"}]}`))
	}))
	t.Cleanup(remote.Close)

	primary := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "missing"}
	fallback := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"}
	cfg := &config.Config{
		Tiers:        []config.Tier{{ID: "t1", Target: primary, Fallbacks: []provider.RouteTarget{fallback}}},
		Destinations: []string{"ollama"},
	}
	cfg.SetProviderBaseURL(config.ProviderSurfaceKey("anthropic", "first-party"), remote.URL)
	registry := newProviders(local.URL, cfg)

	got, _, _, err := probeDelegateTier(context.Background(), cfg, registry, "t1")
	if err == nil || !strings.Contains(err.Error(), "not an approved destination") {
		t.Fatalf("delegate fallback resolved to %s with error %v; want destination refusal", got.Target.Display(), err)
	}
}

func TestProbeTierFallbackStaysQuietWhenThePrimaryServes(t *testing.T) {
	ts := fakeOllama(t, "primary")
	p := newProviders(ts.URL, &config.Config{})

	tier, _, note, err := p.probeTierFallback(context.Background(), ollamaTier("t1", "primary", "backup"))
	if err != nil {
		t.Fatal(err)
	}
	if tier.Target.ModelID != "primary" || note != "" {
		t.Errorf("served %s with note %q; the fallback list must not be consulted when the primary answers",
			tier.Target.ID(), note)
	}
}

func TestReachablePrimaryFeasibilityFailureDoesNotActivateFallback(t *testing.T) {
	ts := fakeOllama(t, "primary", "backup")
	p := newProviders(ts.URL, &config.Config{})

	got, _, note, err := p.probeTierFallbackFeasible(context.Background(),
		ollamaTier("t1", "primary", "backup"), func(candidate config.Tier) error {
			if candidate.Target.ModelID == "primary" {
				return errors.New("context window is too small")
			}
			return nil
		})
	if err == nil || !strings.Contains(err.Error(), "context window is too small") {
		t.Fatalf("reachable infeasible primary resolved to %s (%q), err=%v; fallback is availability-only", got.Target.Display(), note, err)
	}
}

func TestProbeTierFallbackReportsEveryAttempt(t *testing.T) {
	ts := fakeOllama(t) // a server with nothing pulled
	p := newProviders(ts.URL, &config.Config{})

	_, _, _, err := p.probeTierFallback(context.Background(), ollamaTier("t1", "missing", "backup"))
	if err == nil {
		t.Fatal("nothing was servable, but no error came back")
	}
	for _, want := range []string{"missing", "backup", "all unavailable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q; the user needs every attempt named", err, want)
		}
	}
}

func TestInteractiveBootstrapIsNotRecordedAsAnEmptyPromptDecision(t *testing.T) {
	ts := fakeOllama(t, "small", "large")
	cfg := &config.Config{Tiers: []config.Tier{
		ollamaTier("t1", "small"),
		ollamaTier("t2", "large"),
	}}
	p := newProviders(ts.URL, cfg)
	var chosen route.Decision
	tier, _, _, err := resolveTier(context.Background(), p, cfg, &catalog.Catalog{}, &options{}, "", &chosen)
	if err != nil {
		t.Fatal(err)
	}
	if tier.ID != "t1" {
		t.Fatalf("bootstrap tier = %s, want bottom rung", tier.ID)
	}
	if chosen.Source != "" {
		t.Fatalf("empty interactive prompt was recorded as a routing decision: %+v", chosen)
	}
}

func TestAutomaticInteractiveBootstrapSkipsUnavailableTier(t *testing.T) {
	server := fakeOllama(t, "healthy")
	cfg := &config.Config{Tiers: []config.Tier{
		ollamaTier("t1", "missing", "also-missing"),
		ollamaTier("t2", "healthy"),
	}}
	registry := newProviders(server.URL, cfg)
	var chosen route.Decision
	tier, client, note, err := resolveTier(context.Background(), registry, cfg, &catalog.Catalog{}, &options{}, "", &chosen)
	if err != nil {
		t.Fatal(err)
	}
	if tier.ID != "t2" || tier.Target.ModelID != "healthy" || client == nil {
		t.Fatalf("automatic bootstrap = %s/%s (%T), want reachable t2", tier.ID, tier.Target.ModelID, client)
	}
	if chosen.Source != "" {
		t.Fatalf("interactive bootstrap fabricated an empty-prompt route decision: %+v", chosen)
	}
	if !strings.Contains(note, "tier t1") || !strings.Contains(note, "continued on tier t2") {
		t.Fatalf("bootstrap exclusion note = %q", note)
	}
}

// Routing off is the user owning the rung on the headless surface too: the
// bootstrap takes the first reachable rung, exactly as interactive startup
// does, instead of letting the policy pick by prompt.
func TestHeadlessBootstrapWithRoutingOffTakesTheFirstReachableRung(t *testing.T) {
	cat := catalogWithLocalModels(t,
		localModelSpec{name: "small", contextWindow: 100_000},
		localModelSpec{name: "large", contextWindow: 100_000},
	)
	server := fakeOllama(t, "small", "large")
	off := false
	cfg := &config.Config{
		Tiers:     []config.Tier{ollamaTier("t1", "small"), ollamaTier("t2", "large")},
		RouteAuto: &off,
	}
	registry := newProviders(server.URL, cfg)
	var chosen route.Decision
	tier, _, _, err := resolveTier(context.Background(), registry, cfg, cat,
		&options{prompt: "refactor the parser"}, "", &chosen)
	if err != nil {
		t.Fatal(err)
	}
	if tier.ID != "t1" {
		t.Fatalf("routing-off headless bootstrap = %s, want the first reachable rung", tier.ID)
	}
	if chosen.Source != "" {
		t.Fatalf("a routing-off bootstrap was recorded as a decision: %+v", chosen)
	}
}

func TestAutomaticHeadlessBootstrapReroutesAroundUnavailableTier(t *testing.T) {
	cat := catalogWithLocalModels(t,
		localModelSpec{name: "missing", contextWindow: 100_000},
		localModelSpec{name: "also-missing", contextWindow: 100_000},
		localModelSpec{name: "healthy", contextWindow: 100_000},
	)
	server := fakeOllama(t, "healthy")
	cfg := &config.Config{Tiers: []config.Tier{
		ollamaTier("t1", "missing", "also-missing"),
		ollamaTier("t2", "healthy"),
	}}
	registry := newProviders(server.URL, cfg)
	var chosen route.Decision
	tier, client, note, err := resolveTier(context.Background(), registry, cfg, cat,
		&options{prompt: "make the small edit"}, "", &chosen)
	if err != nil {
		t.Fatal(err)
	}
	if tier.ID != "t2" || tier.Target.ModelID != "healthy" || client == nil {
		t.Fatalf("automatic headless bootstrap = %s/%s (%T), want reachable t2", tier.ID, tier.Target.ModelID, client)
	}
	if chosen.Tier != "t2" || chosen.Target != tier.Target.ID() || chosen.Source != route.SourceHeuristic {
		t.Fatalf("headless startup decision = %+v", chosen)
	}
	joined := strings.Join(chosen.Infeasible, " ")
	if !strings.Contains(joined, "tier t1") || !strings.Contains(joined, "unavailable at startup") {
		t.Fatalf("live startup exclusion missing from decision: %v", chosen.Infeasible)
	}
	if !strings.Contains(note, "continued on tier t2") {
		t.Fatalf("headless bootstrap note = %q", note)
	}
}

func TestResumeRecognizesConfiguredFallbackAsItsOriginalTier(t *testing.T) {
	server := fakeOllama(t, "backup")
	tier := ollamaTier("t1", "primary", "backup")
	cfg := &config.Config{Tiers: []config.Tier{tier}}
	registry := newProviders(server.URL, cfg)
	recorded := string(tier.Fallbacks[0].ID())

	resolved, _, _, err := resolveTier(context.Background(), registry, cfg, &catalog.Catalog{}, &options{}, recorded, &route.Decision{})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ID != "t1" || resolved.Target.ID() != tier.Fallbacks[0].ID() {
		t.Fatalf("resumed fallback resolved as %s/%s, want t1/%s", resolved.ID, resolved.Target.ID(), tier.Fallbacks[0].ID())
	}
	if len(resolved.Fallbacks) == 0 || resolved.Fallbacks[0].ID() != tier.Target.ID() {
		t.Fatalf("active fallback did not retain the primary as a later option: %+v", resolved.Fallbacks)
	}
}

func TestRecordedTargetRoundTripsPlusModelAndMatchesLegacySession(t *testing.T) {
	target := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "vendor/model+preview%2B"}
	parsed, err := parseRecordedTarget(string(target.ID()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed, target) {
		t.Fatalf("canonical recorded target = %#v, want %#v", parsed, target)
	}

	legacy := string(target.LegacyID())
	parsed, err = parseRecordedTarget(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed, target) {
		t.Fatalf("legacy plus-bearing target = %#v, want %#v", parsed, target)
	}
	cfg := &config.Config{Tiers: []config.Tier{{ID: "t1", Target: target}}}
	matched, ok, err := tierForTarget(cfg, legacy)
	if err != nil || !ok || !reflect.DeepEqual(matched.Target, target) {
		t.Fatalf("legacy session target %q did not recover configured target: %#v, %v", legacy, matched, ok)
	}
}

func TestLegacySuffixLookingSessionRequiresUniqueConfiguredTarget(t *testing.T) {
	literal := provider.RouteTarget{Provider: "openai", Surface: "api", ModelID: "model+think:high"}
	parameterized := provider.RouteTarget{Provider: "openai", Surface: "api", ModelID: "model"}
	parameterized.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}
	recorded := string(literal.LegacyID())
	if matched, ok, err := tierForTarget(&config.Config{Tiers: []config.Tier{{ID: "t1", Target: literal}}}, recorded); err != nil || !ok || matched.Target.ID() != literal.ID() {
		t.Fatalf("unique legacy literal target did not resume: %#v, %v", matched, ok)
	}
	if _, ok, err := tierForTarget(&config.Config{Tiers: []config.Tier{
		{ID: "t1", Target: literal}, {ID: "t2", Target: parameterized},
	}}, recorded); ok || err == nil {
		t.Fatalf("ambiguous legacy suffix-looking target selected an arbitrary tier: ok=%v err=%v", ok, err)
	}
	registry := newProviders("http://127.0.0.1:1", &config.Config{})
	if _, _, _, err := resolveTier(context.Background(), registry, &config.Config{Tiers: []config.Tier{
		{ID: "t1", Target: literal}, {ID: "t2", Target: parameterized},
	}}, &catalog.Catalog{}, &options{}, recorded, &route.Decision{}); err == nil || !strings.Contains(err.Error(), "matches 2 configured targets") {
		t.Fatalf("ambiguous resumed target did not fail before probing: %v", err)
	}
}

func TestSharedTargetResumeRequiresExplicitTierBeforeProbe(t *testing.T) {
	shared := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "shared"}
	shared.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}
	cfg := &config.Config{Tiers: []config.Tier{
		{ID: "t1", Target: provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "small"}, Fallbacks: []provider.RouteTarget{shared}},
		{ID: "t2", Target: shared},
	}}
	if _, ok, err := tierForTarget(cfg, string(shared.ID())); ok || err == nil ||
		!strings.Contains(err.Error(), "matches 2 configured targets") || !strings.Contains(err.Error(), "think:high") ||
		strings.Contains(err.Error(), "rt2:") {
		t.Fatalf("shared target recovered an arbitrary owner or leaked its machine ID: ok=%v err=%v", ok, err)
	}
	registry := newProviders("http://127.0.0.1:1", cfg)
	if _, _, _, err := resolveTier(context.Background(), registry, cfg, &catalog.Catalog{}, &options{},
		string(shared.ID()), &route.Decision{}); err == nil || !strings.Contains(err.Error(), "choose -tier or -model") {
		t.Fatalf("shared resumed target did not fail before provider probing: %v", err)
	}
}

func TestOffConfigLegacyPercentModelIsNotDecodedAsPlus(t *testing.T) {
	const model = "gpt%2Bpreview"
	server := fakeOllama(t, model)
	cfg := &config.Config{}
	registry := newProviders(server.URL, cfg)
	tier, _, _, err := resolveTier(context.Background(), registry, cfg, &catalog.Catalog{}, &options{},
		"ollama/local/"+model, &route.Decision{})
	if err != nil {
		t.Fatal(err)
	}
	if tier.Target.ModelID != model {
		t.Fatalf("legacy percent model resumed as %q, want literal %q", tier.Target.ModelID, model)
	}
}

func TestCanonicalDefaultWinsOverLossyExplicitThinkFalseLegacyID(t *testing.T) {
	ordinary := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "same"}
	disabled := ordinary
	disabled.Params.Reasoning = &provider.Reasoning{Enabled: false}
	if ordinary.ID() == disabled.ID() {
		t.Fatal("explicit think:false still aliases the omitted provider default")
	}
	cfg := &config.Config{Tiers: []config.Tier{
		{ID: "t1", Target: ordinary}, {ID: "t2", Target: disabled},
	}}
	matched, ok, err := tierForTarget(cfg, string(ordinary.ID()))
	if err != nil || !ok || matched.ID != "t1" || matched.Target.ID() != ordinary.ID() {
		t.Fatalf("canonical default did not win over lossy think:false legacy spelling: matched=%#v ok=%v err=%v", matched, ok, err)
	}
}

func TestCanonicalDefaultSessionIsNotReinterpretedThroughLossyLegacyID(t *testing.T) {
	base := provider.RouteTarget{Provider: "openai", Surface: "api", ModelID: "same"}
	temperature := 0.2
	variants := []provider.RouteTarget{base, base, base}
	variants[0].Params.MaxOutputTokens = 2_048
	variants[1].Params.Temperature = &temperature
	variants[2].Params.Reasoning = &provider.Reasoning{Enabled: false}
	for _, variant := range variants {
		cfg := &config.Config{Tiers: []config.Tier{{ID: "t1", Target: variant}}}
		if matched, ok, err := tierForTarget(cfg, string(base.ID())); err != nil || ok {
			t.Errorf("canonical default %q was reinterpreted as lossy target %q: matched=%#v ok=%v err=%v",
				base.ID(), variant.ID(), matched, ok, err)
		}
	}
}
