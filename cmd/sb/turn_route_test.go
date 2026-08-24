package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/breakpoint"
	"github.com/switchboard-code/switchboard/internal/cachestate"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/costmodel"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/prefix"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/rootedfs"
	route "github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

type localModelSpec struct {
	name           string
	contextWindow  int
	vision         bool
	inputPerMTok   string
	outputPerMTok  string
	priceMaxInput  int
	cacheReadMTok  string
	cacheWriteMTok string
}

func catalogWithLocalModels(t *testing.T, specs ...localModelSpec) *catalog.Catalog {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".switchboard")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var contents strings.Builder
	for _, spec := range specs {
		metering := "local"
		if spec.inputPerMTok != "" || spec.outputPerMTok != "" {
			metering = "per-token"
		}
		input, output := spec.inputPerMTok, spec.outputPerMTok
		if input == "" {
			input = "0"
		}
		if output == "" {
			output = "0"
		}
		cacheRead := spec.cacheReadMTok
		if cacheRead == "" {
			cacheRead = "0"
		}
		cacheWriteLine := ""
		cacheBlock := `modes = ["none"]
  default_mode = "none"
  usage_accounting = "none"`
		if spec.cacheReadMTok != "" {
			cacheWrite := spec.cacheWriteMTok
			if cacheWrite == "" {
				cacheWrite = input
			}
			cacheWriteLine = fmt.Sprintf("  cache_write_per_mtok = { \"5m\" = %q }\n", cacheWrite)
			cacheBlock = `modes = ["explicit"]
  default_mode = "explicit"
  min_tokens = 512
  ttls = ["5m"]
  max_breakpoints = 4
  lookback_blocks = 20
  usage_accounting = "separate"`
		}
		fmt.Fprintf(&contents, `
[[model]]
provider = "ollama"
surface = "local"
provider_model_id = %q
display_name = %q
metering = %q
context_window = %d
max_output = 100
tools = "parallel"
vision = %t
verified_at = 2026-08-17T00:00:00Z

  [[model.pricing]]
  effective_at = 2026-08-17T00:00:00Z
  max_input_tokens = %d
  input_per_mtok = %q
  output_per_mtok = %q
  cache_read_per_mtok = %q
%s

  [model.cache]
  %s
`, spec.name, spec.name, metering, spec.contextWindow, spec.vision, spec.priceMaxInput, input, output,
			cacheRead, cacheWriteLine, cacheBlock)
	}
	if err := os.WriteFile(filepath.Join(dir, catalog.UserOverrideFile), []byte(contents.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	isolateTestHome(t, home)
	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

func capabilityOllama(t *testing.T, models map[string]bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			entries := make([]string, 0, len(models))
			for model := range models {
				entries = append(entries, `{"name":"`+model+`"}`)
			}
			sort.Strings(entries)
			_, _ = w.Write([]byte(`{"models":[` + strings.Join(entries, ",") + `]}`))
		case "/api/show":
			var input struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			caps := `"tools"`
			if models[input.Model] {
				caps += `,"vision"`
			}
			_, _ = w.Write([]byte(`{"capabilities":[` + caps + `]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func turnPlannerFixture(t *testing.T) (*agent.Loop, *config.Config, *catalog.Catalog, string) {
	t.Helper()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "check.py"), []byte("print('ok')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	low := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}
	mid := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-sonnet-5"}
	high := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"}
	cfg := &config.Config{Tiers: []config.Tier{
		{ID: "t1", Target: low}, {ID: "t2", Target: mid}, {ID: "t3", Target: high},
	}}
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(workspace, low.ID(), cat.Revision)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	registry, err := tools.NewRegistry(workspace, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	loop := &agent.Loop{Session: sess, Tools: registry, System: []provider.Block{provider.Text{Text: "system"}}}
	return loop, cfg, cat, workspace
}

func TestPerTurnPlanUsesStructuredSessionEvidence(t *testing.T) {
	loop, cfg, cat, workspace := turnPlannerFixture(t)
	testCall := provider.ToolUse{ID: "test-1", Name: "exec", Input: json.RawMessage(`{"argv":["go","test","./..."]}`)}
	readCall := provider.ToolUse{ID: "read-1", Name: "read", Input: json.RawMessage(`{"path":"main.go"}`)}
	for _, message := range []provider.Message{
		provider.UserText("run the tests"),
		{Role: provider.RoleAssistant, Content: []provider.Block{testCall, readCall}},
		{Role: provider.RoleTool, Content: []provider.Block{
			provider.ToolResult{ToolUseID: "test-1", Name: "exec", Content: "--- FAIL: TestThing", IsError: true},
			provider.ToolResult{ToolUseID: "read-1", Name: "read", Content: "package main"},
		}},
	} {
		if err := loop.Session.AppendMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	sticky := route.NewSticky(route.Policy{}, 0)
	tier, plan, err := planUserTurn(loop, cfg, cat, nil, nil, newCacheSet(cfg.Tiers[0].Target, nil), sticky,
		cfg.Tiers[0], provider.UserText("fix it"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if tier.ID != "t3" {
		t.Fatalf("tier = %s, want strong evidence to reach the top rung: %s; features=%+v", tier.ID, plan.Decision.Rationale, plan.Features)
	}
	if plan.Features.PriorFailures != 1 || !plan.Features.TestsInvolved {
		t.Fatalf("failure features = %+v", plan.Features)
	}
	if plan.Features.FilesInContext != 1 {
		t.Fatalf("files_in_context = %d, want main.go", plan.Features.FilesInContext)
	}
	if strings.Join(plan.Features.RepoLanguages, ",") != "Go,Python" {
		t.Fatalf("languages = %v", plan.Features.RepoLanguages)
	}
	if plan.Decision.PolicyRevision == "" {
		t.Fatal("decision omitted its policy revision")
	}
}

func TestRepoLanguagesOmitsEvidenceOnPartialTraversal(t *testing.T) {
	root := t.TempDir()
	for name := range map[string]struct{}{"one.go": {}, "two.py": {}} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("source"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	limits := rootedfs.WalkLimits{MaxEntries: 2, MaxDirectories: 1, MaxDepth: 0, ReadDirBatch: 1}
	if got := strings.Join(repoLanguagesWithLimits(root, limits, nil), ","); got != "Go,Python" {
		t.Fatalf("exact-cap languages = %q", got)
	}
	if err := os.WriteFile(filepath.Join(root, "three.ts"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := repoLanguagesWithLimits(root, limits, nil); got != nil {
		t.Fatalf("partial traversal supplied routing evidence: %v", got)
	}
	if got := repoLanguagesWithLimits("", limits, nil); got != nil {
		t.Fatalf("empty workspace scanned the process directory: %v", got)
	}
}

func TestRepoLanguagesHasAConservativeProductionWorkBudget(t *testing.T) {
	if routeLanguageMaxEntries >= 50_000 {
		t.Fatalf("per-turn language scan entry limit = %d, must remain below the former synchronous ceiling", routeLanguageMaxEntries)
	}
	if routeLanguageBatch <= 0 || routeLanguageBatch > 256 || routeLanguageTimeout <= 0 || routeLanguageTimeout > time.Second {
		t.Fatalf("per-turn language latency contract = batch %d timeout %s", routeLanguageBatch, routeLanguageTimeout)
	}
}

func TestRepoLanguagesOmitsEvidenceWhenDeadlineExpires(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.go"), []byte("package p"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	limits := rootedfs.WalkLimits{MaxEntries: 8, MaxDirectories: 2, MaxDepth: 1, ReadDirBatch: 1}
	if got := repoLanguagesWithContext(ctx, root, limits, nil); got != nil {
		t.Fatalf("cancelled traversal supplied routing evidence: %v", got)
	}
}

func TestPerTurnPlanChecksTheFullProspectiveContext(t *testing.T) {
	loop, cfg, cat, workspace := turnPlannerFixture(t)
	// Haiku's catalog window is 200k tokens; the full system prompt alone is
	// larger, while the upper rungs hold one million. The user's short text
	// must not hide that context overflow.
	loop.System = []provider.Block{provider.Text{Text: strings.Repeat("x", 900_000)}}
	tier, plan, err := planUserTurn(loop, cfg, cat, nil, nil, newCacheSet(cfg.Tiers[0].Target, nil),
		route.NewSticky(route.Policy{}, 0), cfg.Tiers[0], provider.UserText("continue"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if plan.PromptTokens <= 200_000 {
		t.Fatalf("prospective request estimated at %d tokens", plan.PromptTokens)
	}
	if tier.ID != "t2" {
		t.Fatalf("tier = %s, want the lowest context-feasible rung; exclusions=%v", tier.ID, plan.Decision.Infeasible)
	}
}

func TestPerTurnPlanScoresTheTargetActuallyServingCurrentTier(t *testing.T) {
	loop, cfg, cat, workspace := turnPlannerFixture(t)
	current := cfg.Tiers[0]
	current.Target = cfg.Tiers[2].Target // the configured primary fell back at session assembly
	tier, plan, err := planUserTurn(loop, cfg, cat, nil, nil, newCacheSet(current.Target, nil),
		route.NewSticky(route.Policy{}, 0), current, provider.UserText("small fix"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if tier.Target.ID() != current.Target.ID() || plan.Decision.Target != current.Target.ID() {
		t.Fatalf("planned tier=%s decision=%s, want active fallback %s", tier.Target.ID(), plan.Decision.Target, current.Target.ID())
	}
}

func TestPerTurnPlanScoresActiveFallbackCacheWarmth(t *testing.T) {
	loop, cfg, cat, workspace := turnPlannerFixture(t)
	current := cfg.Tiers[0]
	current.Target = cfg.Tiers[2].Target
	info, _, ok := cat.Lookup(current.Target)
	if !ok || info.Cache.UsageAccounting != catalog.AccountingSeparate {
		t.Fatal("fallback fixture no longer has observable cache accounting")
	}
	cache := &agent.Cache{
		Manager: &breakpoint.Manager{Policy: info.Cache, Target: current.Target.ID()},
		Tracker: cachestate.New(), Policy: info.Cache, Target: current.Target.ID(),
	}
	set := newCacheSet(current.Target, cache)
	opening := provider.UserText("small fix")
	messages := append(append([]provider.Message(nil), loop.Session.State().Messages...), opening)
	layout := prefix.New(loop.System, loop.Tools.Definitions(), 0)
	if len(messages) > 0 {
		layout.AppendHistory(messages[:len(messages)-1]...)
		layout.SetTail(messages[len(messages)-1].Content...)
	}
	cache.Tracker.Observe(cachestate.Observation{
		Target: current.Target.ID(), PrefixHash: layout.PrefixHash(),
		Usage: provider.Usage{CacheWriteTokens: 25_000}, At: time.Now(),
		Accounting: info.Cache.UsageAccounting, Eligible: true, MinimumTTL: time.Hour,
	})

	tier, plan, err := planUserTurn(loop, cfg, cat, nil, nil, set,
		route.NewSticky(route.Policy{}, 0), current, opening, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if tier.Target.ID() != current.Target.ID() {
		t.Fatalf("tier target = %s, want active fallback %s", tier.Target.ID(), current.Target.ID())
	}
	cold := candidateForTier(current, 0, cat, plan.PromptTokens, 0).Estimate.Expected
	if plan.Decision.EstimatedCost.Expected >= cold {
		t.Fatalf("fallback estimate stayed cold: routed=%s cold=%s", plan.Decision.EstimatedCost.Expected, cold)
	}
}

func TestPerTurnVisionRequirementIncludesReplayHistory(t *testing.T) {
	loop, cfg, cat, workspace := turnPlannerFixture(t)
	local := config.Tier{ID: "t1", Target: provider.RouteTarget{
		Provider: "ollama", Surface: "local", ModelID: "vision-unattested",
	}}
	cfg.Tiers = []config.Tier{local, cfg.Tiers[1]}
	imageTurn := provider.UserText("inspect this")
	imageTurn.Content = append(imageTurn.Content, provider.Image{MediaType: "image/png", Data: []byte{1}})
	if err := loop.Session.AppendMessage(imageTurn); err != nil {
		t.Fatal(err)
	}

	tier, _, err := planUserTurn(loop, cfg, cat, nil, nil, newCacheSet(local.Target, nil),
		route.NewSticky(route.Policy{}, 0), local, provider.UserText("continue"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if tier.ID != "t2" {
		t.Fatalf("text follow-up routed image-bearing history to %s", tier.ID)
	}
}

func TestPositiveLiveVisionProbeOverridesSurfaceDefault(t *testing.T) {
	loop, _, cat, workspace := turnPlannerFixture(t)
	local := config.Tier{ID: "t1", Target: provider.RouteTarget{
		Provider: "ollama", Surface: "local", ModelID: "vision-live",
	}}
	cfg := &config.Config{Tiers: []config.Tier{local}}
	probes := &providers{probes: map[provider.RouteTargetID]provider.ProbeResult{
		local.Target.ID(): {Reachable: true, ModelPresent: true, Tools: provider.ToolsParallel, Vision: true, VisionKnown: true},
	}}
	opening := provider.UserText("inspect")
	opening.Content = append(opening.Content, provider.Image{MediaType: "image/png", Data: []byte{1}})

	tier, _, err := planUserTurn(loop, cfg, cat, probes, nil, newCacheSet(local.Target, nil),
		route.NewSticky(route.Policy{}, 0), local, opening, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if tier.Target.ID() != local.Target.ID() {
		t.Fatalf("live vision target = %s, want %s", tier.Target.ID(), local.Target.ID())
	}
}

func TestLiveNegativeVisionOverridesSurfaceDefault(t *testing.T) {
	target := provider.RouteTarget{Provider: "openai", Surface: "subscription", ModelID: "text-only"}
	probes := &providers{probes: map[provider.RouteTargetID]provider.ProbeResult{
		target.ID(): {Reachable: true, ModelPresent: true, Tools: provider.ToolsParallel, VisionKnown: true, Vision: false},
	}}
	candidate := route.Candidate{Target: target, Info: catalog.ModelInfo{Vision: true}}
	if got := withLiveCapabilities(candidate, probes); got.Info.Vision {
		t.Fatal("known text-only live evidence retained the surface's vision default")
	}
}

func TestUnknownLiveVisionDoesNotOverrideVerifiedCatalog(t *testing.T) {
	target := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "vision-catalogued"}
	probes := &providers{probes: map[provider.RouteTargetID]provider.ProbeResult{
		target.ID(): {Reachable: true, ModelPresent: true, Tools: provider.ToolsParallel},
	}}
	candidate := route.Candidate{Target: target, Info: catalog.ModelInfo{Vision: true}}
	if got := withLiveCapabilities(candidate, probes); !got.Info.Vision {
		t.Fatal("a protocol silent about modalities erased verified catalog vision")
	}
}

func TestFirstImageTurnProbesUnknownVisionCandidates(t *testing.T) {
	loop, _, cat, workspace := turnPlannerFixture(t)
	server := capabilityOllama(t, map[string]bool{"text": false, "vision": true})
	textTier := ollamaTier("t1", "text")
	visionTier := ollamaTier("t2", "vision")
	cfg := &config.Config{Tiers: []config.Tier{textTier, visionTier}}
	probes := newProviders(server.URL, cfg)
	opening := provider.UserText("inspect this")
	opening.Content = append(opening.Content, provider.Image{MediaType: "image/png", Data: []byte{1, 2, 3}})

	tier, _, _, plan, err := resolveUserTurn(context.Background(), loop, cfg, cat, probes, nil,
		newCacheSet(textTier.Target, nil), route.NewSticky(route.Policy{}, 0), textTier, probes.ollama, opening, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if tier.ID != visionTier.ID || tier.Target.ID() != visionTier.Target.ID() {
		t.Fatalf("first image routed to %s/%s; decision=%+v", tier.ID, tier.Target.ID(), plan.Decision)
	}
	if attested, known := probes.probedVision(visionTier.Target); !known || !attested {
		t.Fatalf("vision candidate was not live-probed: known=%v attested=%v", known, attested)
	}
}

func TestPositiveLiveToolsOverrideCatalogNone(t *testing.T) {
	target := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "tools-live"}
	probes := &providers{probes: map[provider.RouteTargetID]provider.ProbeResult{
		target.ID(): {Reachable: true, ModelPresent: true, Tools: provider.ToolsParallel},
	}}
	candidate := withLiveCapabilities(route.Candidate{Target: target, Info: catalog.ModelInfo{Tools: catalog.ToolsNone}}, probes)
	if candidate.Info.Tools != catalog.ToolsParallel {
		t.Fatalf("live tools = %s, want parallel", candidate.Info.Tools)
	}
}

func TestFallbackSearchContinuesPastReachableInfeasibleTarget(t *testing.T) {
	server := capabilityOllama(t, map[string]bool{"primary": false, "small": false, "roomy": false})
	cfg := &config.Config{}
	probes := newProviders(server.URL, cfg)
	tier := ollamaTier("t2", "missing", "small", "roomy")
	got, _, note, err := probes.probeTierFallbackFeasible(context.Background(), tier, func(candidate config.Tier) error {
		if candidate.Target.ModelID == "small" {
			return errors.New("context window is too small")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Target.ModelID != "roomy" {
		t.Fatalf("fallback = %s, want roomy after small failed feasibility", got.Target.ModelID)
	}
	if !strings.Contains(note, "is unavailable") {
		t.Fatalf("fallback note did not name the primary outage: %q", note)
	}
}

func TestReachableCurrentPrimaryContextFailureDoesNotActivateFallback(t *testing.T) {
	loop, _, _, workspace := turnPlannerFixture(t)
	loop.System = nil
	loop.Tools = &tools.Registry{}
	cat := catalogWithLocalModels(t,
		localModelSpec{name: "small", contextWindow: 100},
		localModelSpec{name: "roomy", contextWindow: 1_000},
	)
	server := capabilityOllama(t, map[string]bool{"small": false, "roomy": false})
	tier := ollamaTier("t1", "small", "roomy")
	cfg := &config.Config{Tiers: []config.Tier{tier}}
	probes := newProviders(server.URL, cfg)
	opening := provider.UserText(strings.Repeat("x", 700))

	_, _, _, _, err := resolveUserTurn(context.Background(), loop, cfg, cat, probes, nil,
		newCacheSet(tier.Target, nil), route.NewSticky(route.Policy{}, 0), tier, probes.ollama, opening, workspace)
	if err == nil || !strings.Contains(err.Error(), "holds 100 tokens") {
		t.Fatalf("reachable context-infeasible primary activated its fallback: %v", err)
	}
}

func TestRouterDoesNotUseFallbackToRepairContextInfeasibleTier(t *testing.T) {
	loop, _, _, workspace := turnPlannerFixture(t)
	loop.System = nil
	loop.Tools = &tools.Registry{}
	cat := catalogWithLocalModels(t,
		localModelSpec{name: "tiny", contextWindow: 100},
		localModelSpec{name: "small", contextWindow: 100},
		localModelSpec{name: "roomy", contextWindow: 1_000},
	)
	server := capabilityOllama(t, map[string]bool{"tiny": false, "small": false, "roomy": false})
	current := ollamaTier("t1", "tiny")
	target := ollamaTier("t2", "small", "roomy")
	cfg := &config.Config{Tiers: []config.Tier{current, target}}
	probes := newProviders(server.URL, cfg)

	_, _, _, _, err := resolveUserTurn(context.Background(), loop, cfg, cat, probes, nil,
		newCacheSet(current.Target, nil), route.NewSticky(route.Policy{}, 0), current, probes.ollama,
		provider.UserText(strings.Repeat("x", 700)), workspace)
	if err == nil || !strings.Contains(err.Error(), "holds 100 tokens") {
		t.Fatalf("context-infeasible tier activated a same-rung fallback: %v", err)
	}
}

func TestDenseUnicodeUsesByteLevelContextBound(t *testing.T) {
	loop, _, _, workspace := turnPlannerFixture(t)
	loop.System = nil
	loop.Tools = &tools.Registry{}
	cat := catalogWithLocalModels(t,
		localModelSpec{name: "small", contextWindow: 2_000},
		localModelSpec{name: "roomy", contextWindow: 10_000},
	)
	server := capabilityOllama(t, map[string]bool{"small": false, "roomy": false})
	current := ollamaTier("t1", "small")
	roomy := ollamaTier("t2", "roomy")
	cfg := &config.Config{Tiers: []config.Tier{current, roomy}}
	probes := newProviders(server.URL, cfg)
	opening := provider.UserText(strings.Repeat("🧪", 1_000)) // 4,000 payload bytes

	got, _, _, plan, err := resolveUserTurn(context.Background(), loop, cfg, cat, probes, nil,
		newCacheSet(current.Target, nil), route.NewSticky(route.Policy{}, 0), current, probes.ollama,
		opening, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if plan.PromptTokens >= 2_000 {
		t.Fatalf("fixture chars/4 floor = %d, want it below the small window", plan.PromptTokens)
	}
	if plan.ContextTokens < len(opening.Text()) {
		t.Fatalf("hard context bound = %d, want at least %d payload bytes", plan.ContextTokens, len(opening.Text()))
	}
	if got.ID != "t2" {
		t.Fatalf("dense-token prompt routed to %s, want byte-safe roomy tier; exclusions=%v", got.ID, plan.Decision.Infeasible)
	}
}

func TestPinnedTierMayFallbackButCannotJumpTiers(t *testing.T) {
	loop, _, _, workspace := turnPlannerFixture(t)
	loop.System = nil
	loop.Tools = &tools.Registry{}
	cat := catalogWithLocalModels(t,
		localModelSpec{name: "tiny", contextWindow: 100},
		localModelSpec{name: "roomy", contextWindow: 1_000},
	)
	server := capabilityOllama(t, map[string]bool{"tiny": false, "roomy": false})
	probes := newProviders(server.URL, &config.Config{})
	opening := provider.UserText(strings.Repeat("x", 700))

	withFallback := ollamaTier("t1", "missing", "roomy")
	cfg := &config.Config{Tiers: []config.Tier{withFallback}}
	probes.config = cfg
	sticky := route.NewSticky(route.Policy{}, 0)
	sticky.Pin(0)
	got, _, _, _, err := resolveUserTurn(context.Background(), loop, cfg, cat, probes, nil,
		newCacheSet(withFallback.Target, nil), sticky, withFallback, probes.ollama, opening, workspace)
	if err != nil || got.Target.ModelID != "roomy" {
		t.Fatalf("pinned fallback = %s, err=%v", got.Target.ModelID, err)
	}

	current := ollamaTier("t1", "tiny")
	cfg = &config.Config{Tiers: []config.Tier{current, ollamaTier("t2", "roomy")}}
	probes.config = cfg
	sticky = route.NewSticky(route.Policy{}, 0)
	sticky.Pin(0)
	if _, _, _, _, err := resolveUserTurn(context.Background(), loop, cfg, cat, probes, nil,
		newCacheSet(current.Target, nil), sticky, current, probes.ollama, opening, workspace); err == nil {
		t.Fatal("pinned infeasible tier silently jumped to another rung")
	}
}

// /routing off is the user holding the rung themselves, so the opening route
// must treat the current tier the way a pin treats it — otherwise the rung
// still moves at every turn and the setting is a lie. The current rung's own
// fallback still serves, because availability is not routing.
func TestRoutingOffHoldsTheCurrentRung(t *testing.T) {
	loop, _, _, workspace := turnPlannerFixture(t)
	loop.System = nil
	loop.Tools = &tools.Registry{}
	cat := catalogWithLocalModels(t,
		localModelSpec{name: "tiny", contextWindow: 100},
		localModelSpec{name: "roomy", contextWindow: 1_000},
	)
	server := capabilityOllama(t, map[string]bool{"tiny": false, "roomy": false})
	opening := provider.UserText(strings.Repeat("x", 700))
	off := false

	withFallback := ollamaTier("t1", "missing", "roomy")
	cfg := &config.Config{Tiers: []config.Tier{withFallback}, RouteAuto: &off}
	probes := newProviders(server.URL, cfg)
	got, _, _, _, err := resolveUserTurn(context.Background(), loop, cfg, cat, probes, nil,
		newCacheSet(withFallback.Target, nil), route.NewSticky(route.Policy{}, 0), withFallback, probes.ollama, opening, workspace)
	if err != nil || got.ID != "t1" || got.Target.ModelID != "roomy" {
		t.Fatalf("routing off with a live fallback = %s/%s, err=%v; want t1 served by its fallback", got.ID, got.Target.ModelID, err)
	}

	current := ollamaTier("t1", "tiny")
	cfg = &config.Config{Tiers: []config.Tier{current, ollamaTier("t2", "roomy")}, RouteAuto: &off}
	probes.config = cfg
	if _, _, _, _, err := resolveUserTurn(context.Background(), loop, cfg, cat, probes, nil,
		newCacheSet(current.Target, nil), route.NewSticky(route.Policy{}, 0), current, probes.ollama, opening, workspace); err == nil {
		t.Fatal("routing off silently moved an infeasible current rung to another tier")
	}

	// The same ladder with routing on makes the move, so the hold above is
	// the setting's doing and not the fixture's.
	on := true
	cfg.RouteAuto = &on
	got, _, _, _, err = resolveUserTurn(context.Background(), loop, cfg, cat, probes, nil,
		newCacheSet(current.Target, nil), route.NewSticky(route.Policy{}, 0), current, probes.ollama, opening, workspace)
	if err != nil || got.ID != "t2" {
		t.Fatalf("routing on resolved %s, err=%v; want the move the hold prevents", got.ID, err)
	}
}

func TestReachablePrimaryHardFailuresDoNotActivateFallbacks(t *testing.T) {
	t.Run("vision", func(t *testing.T) {
		loop, _, _, workspace := turnPlannerFixture(t)
		loop.System = nil
		loop.Tools = &tools.Registry{}
		cat := catalogWithLocalModels(t,
			localModelSpec{name: "text", contextWindow: 100_000},
			localModelSpec{name: "vision", contextWindow: 100_000, vision: true},
		)
		server := capabilityOllama(t, map[string]bool{"text": false, "vision": true})
		tier := ollamaTier("t1", "text", "vision")
		cfg := &config.Config{Tiers: []config.Tier{tier}}
		probes := newProviders(server.URL, cfg)
		opening := provider.UserText("inspect")
		opening.Content = append(opening.Content, provider.Image{MediaType: "image/png", Data: []byte("invalid-image")})
		_, _, _, _, err := resolveUserTurn(context.Background(), loop, cfg, cat, probes, nil,
			newCacheSet(tier.Target, nil), route.NewSticky(route.Policy{}, 0), tier, probes.ollama, opening, workspace)
		if err == nil || !strings.Contains(err.Error(), "cannot read images") {
			t.Fatalf("reachable vision-infeasible primary activated its fallback: %v", err)
		}
	})

	t.Run("budget", func(t *testing.T) {
		loop, _, _, workspace := turnPlannerFixture(t)
		loop.System = nil
		loop.Tools = &tools.Registry{}
		cat := catalogWithLocalModels(t,
			localModelSpec{name: "pricey", contextWindow: 100_000, inputPerMTok: "10", outputPerMTok: "10"},
			localModelSpec{name: "cheap", contextWindow: 100_000, inputPerMTok: "0.1", outputPerMTok: "0.1"},
		)
		server := capabilityOllama(t, map[string]bool{"pricey": false, "cheap": false})
		tier := ollamaTier("t1", "pricey", "cheap")
		cfg := &config.Config{Tiers: []config.Tier{tier}}
		probes := newProviders(server.URL, cfg)
		budget := &budgetState{}
		budget.set(1_000)
		_, _, _, plan, err := resolveUserTurn(context.Background(), loop, cfg, cat, probes, budget,
			newCacheSet(tier.Target, nil), route.NewSticky(route.Policy{}, 0), tier, probes.ollama,
			provider.UserText(strings.Repeat("x", 700)), workspace)
		if err == nil || !strings.Contains(err.Error(), "ceiling") {
			t.Fatalf("reachable over-budget primary activated its fallback; decision=%s err=%v", plan.Decision.Target, err)
		}
	})
}

func TestReachablePrimaryPricingGapDoesNotActivateFallback(t *testing.T) {
	loop, _, _, workspace := turnPlannerFixture(t)
	loop.System = nil
	loop.Tools = &tools.Registry{}
	cat := catalogWithLocalModels(t,
		localModelSpec{name: "pricing-gap", contextWindow: 10_000, inputPerMTok: "1", outputPerMTok: "1", priceMaxInput: 500},
		localModelSpec{name: "covered", contextWindow: 10_000, inputPerMTok: "1", outputPerMTok: "1"},
	)
	var mu sync.Mutex
	var chatModels []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"pricing-gap"},{"name":"covered"}]}`))
		case "/api/show":
			_, _ = w.Write([]byte(`{"capabilities":["tools"]}`))
		case "/api/chat":
			var body struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			chatModels = append(chatModels, body.Model)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"done"},"done":true,"done_reason":"stop","prompt_eval_count":8,"eval_count":1}` + "\n"))
		default:
			http.NotFound(w, request)
		}
	}))
	t.Cleanup(server.Close)

	tier := ollamaTier("t1", "pricing-gap", "covered")
	cfg := &config.Config{Tiers: []config.Tier{tier}}
	probes := newProviders(server.URL, cfg)
	opening := provider.UserText(strings.Repeat("x", 600))
	preview := prospectiveTurnPlan(loop, route.NewSticky(route.Policy{}, 0), opening, workspace)
	primaryCandidate := candidateForTierContext(tier, 0, cat, preview.PromptTokens, preview.ContextTokens, 0)
	_, _, _, plan, err := resolveUserTurn(context.Background(), loop, cfg, cat, probes, nil,
		newCacheSet(tier.Target, nil), route.NewSticky(route.Policy{}, 0), tier, probes.ollama,
		opening, workspace)
	if err == nil || !strings.Contains(err.Error(), "no positive conservative cost bound") {
		t.Fatalf("pricing-gap primary activated a fallback; exclusions=%v primary={known:%v free:%v metering:%s ceiling:%s context:%d} err=%v",
			plan.Decision.Infeasible, primaryCandidate.CatalogKnown, primaryCandidate.Info.Free(),
			primaryCandidate.Info.Metering, primaryCandidate.CeilingCost, preview.ContextTokens, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(chatModels) != 0 {
		t.Fatalf("provider calls = %v, want no model call after the reachable primary failed hard feasibility", chatModels)
	}
}

func TestRouterContinuesAfterSelectedTierCannotBind(t *testing.T) {
	loop, _, cat, workspace := turnPlannerFixture(t)
	server := capabilityOllama(t, map[string]bool{"low": false})
	t1 := ollamaTier("t1", "low")
	t2 := ollamaTier("t2", "missing")
	t3 := ollamaTier("t3", "also-missing")
	cfg := &config.Config{Tiers: []config.Tier{t1, t2, t3}}
	probes := newProviders(server.URL, cfg)

	tier, _, _, plan, err := resolveUserTurn(context.Background(), loop, cfg, cat, probes, nil,
		newCacheSet(t1.Target, nil), route.NewSticky(route.Policy{}, 0), t1, probes.ollama,
		provider.UserText("refactor this function"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if tier.ID != "t1" {
		t.Fatalf("failed selected t2 did not continue deterministically: got %s", tier.ID)
	}
	if !strings.Contains(strings.Join(plan.Decision.Infeasible, " "), "t2") {
		t.Fatalf("rejected selected tier absent from decision: %v", plan.Decision.Infeasible)
	}
}

func TestAutomaticTurnReprobesStaleCurrentTarget(t *testing.T) {
	loop, _, _, workspace := turnPlannerFixture(t)
	loop.System = nil
	loop.Tools = &tools.Registry{}
	cat := catalogWithLocalModels(t,
		localModelSpec{name: "current", contextWindow: 100_000},
		localModelSpec{name: "fallback", contextWindow: 100_000},
	)
	live := map[string]bool{"current": false, "fallback": false}
	server := capabilityOllama(t, live)
	tier := ollamaTier("t1", "current", "fallback")
	cfg := &config.Config{Tiers: []config.Tier{tier}}
	probes := newProviders(server.URL, cfg)
	if _, _, err := probes.probeTier(context.Background(), tier); err != nil {
		t.Fatal(err)
	}
	delete(live, "current") // the server dies after its successful startup probe

	got, _, note, _, err := resolveUserTurn(context.Background(), loop, cfg, cat, probes, nil,
		newCacheSet(tier.Target, nil), route.NewSticky(route.Policy{}, 0), tier, probes.ollama,
		provider.UserText("continue"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if got.Target.ModelID != "fallback" {
		t.Fatalf("stale current target was reused: got %s", got.Target.ModelID)
	}
	if !strings.Contains(note, "fallback") || !strings.Contains(note, "current") {
		t.Fatalf("liveness fallback note = %q", note)
	}
}

func TestPriorFailureEvidenceCorrelatesResultsToTestCalls(t *testing.T) {
	testCall := provider.ToolUse{ID: "test", Name: "exec", Input: json.RawMessage(`{"argv":["go","test","./..."]}`)}
	readCall := provider.ToolUse{ID: "read", Name: "read", Input: json.RawMessage(`{"path":"main.go"}`)}
	messages := []provider.Message{
		provider.UserText("work"),
		{Role: provider.RoleAssistant, Content: []provider.Block{testCall, readCall}},
		{Role: provider.RoleTool, Content: []provider.Block{
			provider.ToolResult{ToolUseID: "test", Name: "exec", Content: "ok"},
			provider.ToolResult{ToolUseID: "read", Name: "read", Content: "missing", IsError: true},
		}},
	}
	failures, testFailures, tests := priorTurnEvidence(messages)
	if failures != 1 || testFailures != 0 || !tests {
		t.Fatalf("evidence = failures %d, test failures %d, tests %v", failures, testFailures, tests)
	}
}

func TestRetargetTurnPlanRepricesConcreteFallback(t *testing.T) {
	loop, cfg, cat, _ := turnPlannerFixture(t)
	opening := provider.UserText("continue")
	plan := prospectiveTurnPlan(loop, nil, opening, t.TempDir())
	primary := candidateForTier(cfg.Tiers[0], 0, cat, plan.PromptTokens, 0)
	plan.Decision = route.Decision{Tier: cfg.Tiers[0].ID, Target: cfg.Tiers[0].Target.ID(), EstimatedCost: primary.Estimate}
	retargetTurnPlan(&plan, loop, cat, newCacheSet(cfg.Tiers[0].Target, nil), cfg.Tiers[2], 0, opening)
	want := candidateForTier(cfg.Tiers[2], 0, cat, plan.PromptTokens, 0).Estimate.Expected
	if plan.Decision.Target != cfg.Tiers[2].Target.ID() || plan.Decision.EstimatedCost.Expected != want {
		t.Fatalf("retargeted decision = %+v, want target %s estimate %s", plan.Decision, cfg.Tiers[2].Target.ID(), want)
	}
}

func TestCandidateContextBoundDoesNotDoubleWidenCost(t *testing.T) {
	_, cfg, cat, _ := turnPlannerFixture(t)
	candidate := candidateForTierContext(cfg.Tiers[0], 0, cat, 7_600, 10_000, 0)
	if candidate.ContextTokens != 10_000 {
		t.Fatalf("context tokens = %d, want conservative 10000", candidate.ContextTokens)
	}
	want := costmodel.Estimator{}.Turn(costmodel.Inputs{
		Target: cfg.Tiers[0].Target, Info: candidate.Info, PrefixTokens: 7_600, OutputTokens: 512,
		Eligible: candidate.Info.Cache.UsageAccounting == catalog.AccountingSeparate,
	})
	if candidate.Estimate.High != want.High {
		t.Fatalf("cost was double-widened: got %s want %s", candidate.Estimate.High, want.High)
	}
}

func TestCandidateReservesConcreteAdapterOutputAllowance(t *testing.T) {
	_, cfg, cat, _ := turnPlannerFixture(t)
	defaultCandidate := candidateForTierContext(cfg.Tiers[0], 0, cat, 1_000, 1_000, 0)
	if defaultCandidate.ReservedOutputTokens != 8_192 {
		t.Fatalf("Anthropic default reserve = %d, want adapter default 8192 (not catalog max %d)",
			defaultCandidate.ReservedOutputTokens, defaultCandidate.Info.MaxOutput)
	}

	explicit := cfg.Tiers[0]
	explicit.Target.Params.MaxOutputTokens = 1_234
	explicitCandidate := candidateForTierContext(explicit, 0, cat, 1_000, 1_000, 0)
	if explicitCandidate.ReservedOutputTokens != 1_234 {
		t.Fatalf("explicit output reserve = %d, want 1234", explicitCandidate.ReservedOutputTokens)
	}

	thinking := cfg.Tiers[0]
	thinking.Target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}
	thinkingCandidate := candidateForTierContext(thinking, 0, cat, 1_000, 1_000, 0)
	if thinkingCandidate.ReservedOutputTokens != 16_384+8_192 {
		t.Fatalf("thinking output reserve = %d, want adapter-raised allowance", thinkingCandidate.ReservedOutputTokens)
	}

	adaptive := cfg.Tiers[1]
	adaptive.Target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}
	adaptiveCandidate := candidateForTierContext(adaptive, 1, cat, 1_000, 1_000, 0)
	if adaptiveCandidate.ReservedOutputTokens != 8_192 {
		t.Fatalf("adaptive effort reserve = %d, want wire max_tokens 8192 with no invented budget", adaptiveCandidate.ReservedOutputTokens)
	}
}

func TestCheckTurnFeasibleUsesEnforcedProbeWindow(t *testing.T) {
	loop, cfg, cat, _ := turnPlannerFixture(t)
	tier := cfg.Tiers[0]
	tier.Target.Params.MaxOutputTokens = 128
	probes := newProviders("http://127.0.0.1:1", cfg)
	probes.mu.Lock()
	probes.windows[bareTargetKey(tier.Target)] = probedWindow{tokens: 1_000, enforced: true}
	probes.mu.Unlock()

	plan := turnPlan{PromptTokens: 900, ContextTokens: 900}
	err := checkTurnFeasible(loop, cat, probes, nil, nil, tier, 0, plan, provider.UserText("continue"))
	if err == nil || !strings.Contains(err.Error(), "holds 1000 tokens") {
		t.Fatalf("enforced live window did not reject the turn: %v", err)
	}
}

func TestCheckTurnFeasibleUsesDeclaredWindowWhenProbeIsHeuristic(t *testing.T) {
	loop, cfg, cat, _ := turnPlannerFixture(t)
	tier := cfg.Tiers[0]
	tier.Target.Params.MaxOutputTokens = 128
	cfg.SetProviderContextWindow(config.ProviderSurfaceKey(tier.Target.Provider, tier.Target.Surface), 2_000)
	probes := newProviders("http://127.0.0.1:1", cfg)
	probes.mu.Lock()
	probes.windows[bareTargetKey(tier.Target)] = probedWindow{tokens: 1_000, enforced: false}
	probes.mu.Unlock()

	plan := turnPlan{PromptTokens: 900, ContextTokens: 900}
	if err := checkTurnFeasible(loop, cat, probes, nil, nil, tier, 0, plan, provider.UserText("continue")); err != nil {
		t.Fatalf("heuristic probe overrode the larger declared window: %v", err)
	}
}

func TestConcreteTurnFallbackMustPassHardFeasibility(t *testing.T) {
	loop, cfg, cat, _ := turnPlannerFixture(t)
	// Simulate a roomy selected tier whose live fallback is the smaller Haiku
	// target. The fallback is reachable and tool-capable, but still must not
	// receive a request beyond its own catalogued window.
	fallback := config.Tier{ID: cfg.Tiers[1].ID, Target: cfg.Tiers[0].Target}
	plan := turnPlan{PromptTokens: 250_000}
	err := checkTurnFeasible(loop, cat, nil, nil, nil, fallback, 1, plan, provider.UserText("continue"))
	if err == nil || !strings.Contains(err.Error(), "holds") {
		t.Fatalf("oversized fallback feasibility error = %v", err)
	}
}

func TestExhaustedDollarBudgetStillRoutesToLocalTarget(t *testing.T) {
	loop, _, cat, workspace := turnPlannerFixture(t)
	local := config.Tier{ID: "t1", Target: provider.RouteTarget{
		Provider: "ollama", Surface: "local", ModelID: "qwen3:4b",
	}}
	paid := config.Tier{ID: "t2", Target: provider.RouteTarget{
		Provider: "anthropic", Surface: "first-party", ModelID: "claude-sonnet-5",
	}}
	cfg := &config.Config{Tiers: []config.Tier{local, paid}}
	budget := &budgetState{}
	budget.set(catalog.Money(100_000))
	if err := loop.Session.AppendRetryReserve(100_000); err != nil {
		t.Fatal(err)
	}

	tier, plan, err := planUserTurn(loop, cfg, cat, nil, budget, newCacheSet(local.Target, nil),
		route.NewSticky(route.Policy{}, 0), local, provider.UserText("fix every failing test"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if tier.ID != local.ID {
		t.Fatalf("exhausted dollar budget routed to %s, want local %s; exclusions=%v", tier.ID, local.ID, plan.Decision.Infeasible)
	}
	if !strings.Contains(strings.Join(plan.Decision.Infeasible, " "), "ceiling") {
		t.Fatalf("paid target was not excluded by exhausted budget: %v", plan.Decision.Infeasible)
	}
}

func TestRouteRecordPersistsDecisionFeaturesAndExactMoves(t *testing.T) {
	loop, cfg, _, _ := turnPlannerFixture(t)
	features := route.SessionFeatures{
		PromptTokens: 812, ContextTokens: 1_160, PriorFailures: 2, TestFailures: 1, FilesInContext: 5, DiffSizeSoFar: 240,
		DiffSizeKnown: true, TestsInvolved: true, LastTurnEscalated: true,
		RepoLanguages: []string{"Go", "TypeScript"},
	}
	decision := &route.Decision{
		Tier: "t1", Target: cfg.Tiers[0].Target.ID(), Source: route.SourceHeuristic,
		Rationale: "fixture", PolicyRevision: route.PolicyRevision,
	}
	state := loop.Session.State()
	usageWindow := loop.Session.BeginUsageWindow()
	if err := appendRouteRecord(loop.Session, "fix it", cfg.Tiers[0], cfg.Tiers[2], state, usageWindow,
		time.Now(), nil, decision, features, 2); err != nil {
		t.Fatal(err)
	}
	timeline, err := session.ReadTimeline(loop.Session.Path())
	if err != nil {
		t.Fatal(err)
	}
	var got *session.Route
	for _, item := range timeline {
		if item.Route != nil {
			got = item.Route
		}
	}
	if got == nil {
		t.Fatal("no route record")
	}
	if got.PromptTokens != 812 || got.ContextTokens != 1_160 || got.PriorFailures != 2 || got.TestFailures != 1 || got.FilesInContext != 5 ||
		got.DiffSize != 240 || !got.DiffSizeKnown || !got.TestsInvolved ||
		!got.LastTurnEscalated || got.Escalations != 2 || got.PolicyRevision != route.PolicyRevision {
		t.Fatalf("route = %+v", *got)
	}
	if strings.Join(got.Languages, ",") != "Go,TypeScript" || got.EndedOn != cfg.Tiers[2].Target.ID() || got.EndedTier != cfg.Tiers[2].ID {
		t.Fatalf("route metadata = %+v", *got)
	}
	if got.VerificationStatus != session.RouteVerificationUnavailable || got.VerificationRan || got.Verified {
		t.Fatalf("route verification telemetry = %+v", *got)
	}
	if got.Outcome != string(route.Escalated) {
		t.Fatalf("successful recorded move outcome = %q, want escalated", got.Outcome)
	}
}

func TestRouteRecordUsesOnlyDurableTurnCallIDsDuringAdvisorOverlap(t *testing.T) {
	loop, cfg, _, _ := turnPlannerFixture(t)
	state := loop.Session.State()
	window := loop.Session.BeginUsageWindow()
	advisorDone := make(chan error, 1)
	go func() {
		advisorDone <- loop.Session.AppendUsage(session.Usage{
			Purpose: session.UsagePurposeAdvisor,
			Usage:   provider.Usage{InputTokens: 900, OutputTokens: 90}, CostMicroUSD: 9000,
		})
	}()
	turn, err := loop.Session.AppendUsageRecord(session.Usage{
		Purpose: session.UsagePurposeTurn,
		Usage:   provider.Usage{InputTokens: 120, OutputTokens: 12}, CostMicroUSD: 1200,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-advisorDone; err != nil {
		t.Fatal(err)
	}
	if err := appendRouteRecord(loop.Session, "overlap", cfg.Tiers[0], cfg.Tiers[0], state, window,
		time.Now(), nil, nil, route.SessionFeatures{}, 0); err != nil {
		t.Fatal(err)
	}
	timeline, err := session.ReadTimeline(loop.Session.Path())
	if err != nil {
		t.Fatal(err)
	}
	var got *session.Route
	for _, item := range timeline {
		if item.Route != nil {
			got = item.Route
		}
	}
	if got == nil || got.Usage != turn.Usage || got.CostMicroUSD != turn.CostMicroUSD ||
		len(got.UsageCallIDs) != 1 || got.UsageCallIDs[0] != turn.CallID {
		t.Fatalf("overlap route = %#v, turn receipt = %#v", got, turn)
	}
	usages, err := session.ReadUsages(loop.Session.Path())
	if err != nil {
		t.Fatal(err)
	}
	resolved := 0
	for _, usage := range usages {
		if usage.CallID == turn.CallID {
			resolved++
		}
	}
	if resolved != 1 {
		t.Fatalf("turn CallID %q resolved %d times", turn.CallID, resolved)
	}
}

func TestRouteRecordZeroMoveErrorsAreFailedNotEscalated(t *testing.T) {
	cases := []struct {
		name string
		err  error
		kind string
	}{
		{name: "provider", err: &provider.APIError{Provider: "test", StatusCode: 503, Body: "down"}, kind: session.RouteFailureProvider},
		{name: "budget", err: fmt.Errorf("%w: fixture", errBudgetUnavailable), kind: session.RouteFailureBudget},
		{name: "context", err: fmt.Errorf("wrapped: %w", &agent.ContextWindowError{Target: "test/openai/small", Window: 100, InputTokens: 90, ReservedOutput: 20}), kind: session.RouteFailureContext},
		{name: "round-limit", err: agent.ErrRoundLimit, kind: session.RouteFailureRoundLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loop, cfg, _, _ := turnPlannerFixture(t)
			state := loop.Session.State()
			window := loop.Session.BeginUsageWindow()
			if err := appendRouteRecord(loop.Session, "failed", cfg.Tiers[0], cfg.Tiers[0], state, window,
				time.Now(), tc.err, nil, route.SessionFeatures{}, 0); err != nil {
				t.Fatal(err)
			}
			timeline, err := session.ReadTimeline(loop.Session.Path())
			if err != nil {
				t.Fatal(err)
			}
			var got *session.Route
			for _, item := range timeline {
				if item.Route != nil {
					got = item.Route
				}
			}
			if got == nil || got.Outcome != string(route.Failed) || got.FailureKind != tc.kind || got.Escalations != 0 || got.EndedOn != "" {
				t.Fatalf("zero-move %s route = %#v", tc.name, got)
			}
			label := route.LabelFor(route.Outcome(got.Outcome), false)
			if !label.Censored || label.Positive || label.Negative || got.VerificationStatus != session.RouteVerificationUnavailable {
				t.Fatalf("zero-move %s evidence = route %#v label %#v", tc.name, got, label)
			}
		})
	}
}
