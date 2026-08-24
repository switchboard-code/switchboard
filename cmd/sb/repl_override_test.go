package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/cachestate"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	route "github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

type replRequestCapture struct {
	mu         sync.Mutex
	bodies     []string
	status     int
	blockChat  bool
	entered    chan struct{}
	showCaps   string
	promptEval int
	onChat     func()
}

func (c *replRequestCapture) add(body string) {
	c.mu.Lock()
	c.bodies = append(c.bodies, body)
	c.mu.Unlock()
}

func newOverrideREPL(t *testing.T, models ...string) (*repl, *replRequestCapture, func() string) {
	t.Helper()
	capture := &replRequestCapture{showCaps: `{"capabilities":["tools","vision"]}`}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/api/tags":
			entries := make([]string, 0, len(models))
			for _, model := range models {
				entries = append(entries, `{"name":`+strconvQuote(model)+`}`)
			}
			_, _ = io.WriteString(w, `{"models":[`+strings.Join(entries, ",")+`]}`)
		case "/api/show":
			capture.mu.Lock()
			showCaps := capture.showCaps
			capture.mu.Unlock()
			_, _ = io.WriteString(w, showCaps)
		case "/api/chat":
			body, _ := io.ReadAll(req.Body)
			capture.add(string(body))
			capture.mu.Lock()
			status := capture.status
			blockChat := capture.blockChat
			entered := capture.entered
			promptEval := capture.promptEval
			onChat := capture.onChat
			capture.mu.Unlock()
			if onChat != nil {
				onChat()
			}
			if blockChat {
				if entered != nil {
					close(entered)
				}
				<-req.Context().Done()
				return
			}
			if status != 0 {
				http.Error(w, "forced failure", status)
				return
			}
			if promptEval == 0 {
				promptEval = 8
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": map[string]string{"role": "assistant", "content": "done"},
				"done":    true, "done_reason": "stop", "prompt_eval_count": promptEval, "eval_count": 1,
			})
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(server.Close)

	workspace := t.TempDir()
	cat := catalogWithLocalModels(t,
		localModelSpec{name: "small", contextWindow: 100_000},
		localModelSpec{name: "large", contextWindow: 100_000},
		localModelSpec{name: "backup", contextWindow: 100_000},
	)
	cfg := &config.Config{Tiers: []config.Tier{ollamaTier("t1", "small"), ollamaTier("t2", "large", "backup")}}
	providers := newProviders(server.URL, cfg)
	tier, client, err := providers.probeTier(context.Background(), cfg.Tiers[0])
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(workspace, tier.Target.ID(), cat.Revision)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	registry, err := tools.NewRegistry(workspace, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	loop := &agent.Loop{
		Provider: client, Target: tier.Target, Cache: cacheFor(tier.Target, cat),
		Tools: registry, Perms: permission.NewEngine(permission.ModePlan, execution.Capability{}),
		Session: sess, Catalog: cat,
	}
	outFile, err := os.CreateTemp(t.TempDir(), "repl-output")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = outFile.Close() })
	out := newRenderer(outFile)
	loop.SetObserver(out)
	sticky := route.NewSticky(route.Policy{MinimumDwell: 2}, 0)
	r := &repl{
		loop: loop, out: out, in: bufio.NewReader(strings.NewReader("")), workspace: workspace,
		config: cfg, catalog: cat, tier: tier, providers: providers, sticky: sticky,
		budget: &budgetState{}, caches: newCacheSet(tier.Target, loop.Cache),
	}
	readOutput := func() string {
		out.flush()
		data, _ := os.ReadFile(outFile.Name())
		return string(data)
	}
	return r, capture, readOutput
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func installWarmPricedREPLCache(t *testing.T, r *repl) {
	t.Helper()
	cat := catalogWithLocalModels(t,
		localModelSpec{name: "small", contextWindow: 100_000, inputPerMTok: "5", outputPerMTok: "1", cacheReadMTok: "0.5", cacheWriteMTok: "6.25"},
		localModelSpec{name: "large", contextWindow: 100_000, inputPerMTok: "5", outputPerMTok: "1"},
		localModelSpec{name: "backup", contextWindow: 100_000, inputPerMTok: "5", outputPerMTok: "1"},
	)
	r.catalog = cat
	r.loop.Catalog = cat
	cache := cacheFor(r.tier.Target, cat)
	binding := r.loop.Binding()
	binding.Cache = cache
	r.loop.Bind(binding)
	r.caches = newCacheSet(r.tier.Target, cache)
	info, _, ok := cat.Lookup(r.tier.Target)
	if !ok || cache == nil || cache.Tracker == nil {
		t.Fatal("priced cache fixture was not constructed")
	}
	cache.Tracker.Observe(cachestate.Observation{
		Target: r.tier.Target.ID(), PrefixHash: "warm-prefix", At: time.Now(),
		Usage: provider.Usage{CacheWriteTokens: 50_000}, Accounting: info.Cache.UsageAccounting,
		Eligible: true, MinimumTTL: 5 * time.Minute,
	})
}

func TestREPLTierPromptRunsOnceAndRestoresBinding(t *testing.T) {
	r, capture, output := newOverrideREPL(t, "small", "large")
	if err := os.WriteFile(filepath.Join(r.workspace, "note.txt"), []byte("attached evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	priorBinding := r.loop.Binding()
	r.sticky.Observe(route.RepeatedToolCall)
	r.sticky.CallServed()

	if done := r.command(context.Background(), "/t2 inspect @note.txt"); done {
		t.Fatal("one-turn override exited the REPL")
	}
	if r.tier.ID != "t1" || r.loop.Binding().Target.ID() != priorBinding.Target.ID() ||
		r.loop.Binding().Provider != priorBinding.Provider || r.loop.Binding().Cache != priorBinding.Cache {
		t.Fatalf("binding was not restored: tier=%s target=%s", r.tier.ID, r.loop.Binding().Target.ID())
	}
	if r.sticky.Rank() != 0 || r.sticky.Pinned() {
		t.Fatalf("automatic sticky state was not restored: rank=%d pinned=%v", r.sticky.Rank(), r.sticky.Pinned())
	}
	if len(capture.bodies) != 1 || !strings.Contains(capture.bodies[0], `"model":"large"`) ||
		!strings.Contains(capture.bodies[0], "attached evidence") {
		t.Fatalf("override request = %#v", capture.bodies)
	}
	timeline, err := session.ReadTimeline(r.loop.Session.Path())
	if err != nil {
		t.Fatal(err)
	}
	var recorded *session.Route
	for _, event := range timeline {
		if event.Route != nil {
			recorded = event.Route
		}
	}
	if recorded == nil || recorded.Tier != "t2" || recorded.Target != r.config.Tiers[1].Target.ID() ||
		recorded.Source != string(route.SourceUserPin) || recorded.EndedOn != "" || recorded.Escalations != 0 {
		t.Fatalf("one-turn route record = %#v", recorded)
	}
	if text := output(); strings.Contains(text, "leaves the previous target's cache") {
		t.Fatalf("temporary override reported false cache abandonment:\n%s", text)
	}
}

func TestREPLSameTierRebindUsesNewConfiguredTarget(t *testing.T) {
	r, capture, _ := newOverrideREPL(t, "small", "large")
	if err := r.config.BindTier("t1", "replacement", "ollama/large", "", ""); err != nil {
		t.Fatal(err)
	}
	r.switchTier(context.Background(), "t1")
	if r.tier.Target.ModelID != "large" || r.loop.Binding().Target.ModelID != "large" {
		t.Fatalf("same-tier rebind stayed on stale target: tier=%s binding=%s",
			r.tier.Target.ModelID, r.loop.Binding().Target.ModelID)
	}
	if err := r.loop.TurnMessage(context.Background(), provider.UserText("use the rebound model")); err != nil {
		t.Fatal(err)
	}
	capture.mu.Lock()
	bodies := append([]string(nil), capture.bodies...)
	capture.mu.Unlock()
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"model":"large"`) || strings.Contains(bodies[0], `"model":"small"`) {
		t.Fatalf("post-switch provider requests = %#v, want one request on large and none on small", bodies)
	}
}

func TestREPLAutomaticOpeningMoveReportsAbandonedWarmth(t *testing.T) {
	r, _, output := newOverrideREPL(t, "small", "large")
	installWarmPricedREPLCache(t, r)
	if err := r.turn(context.Background(), "refactor this across the codebase"); err != nil {
		t.Fatal(err)
	}
	if r.tier.ID != "t2" {
		t.Fatalf("broad opening stayed on %s, so the automatic cache departure path did not run", r.tier.ID)
	}
	text := output()
	if !strings.Contains(text, "modeled value") || !strings.Contains(text, "warm prefix") {
		t.Fatalf("automatic opening move hid abandoned warmth:\n%s", text)
	}
	raw, err := os.ReadFile(r.loop.Session.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "modeled value") {
		t.Fatalf("automatic opening cache note was not persisted:\n%s", raw)
	}
}

func TestREPLMidturnMoveReportsAbandonedWarmthOnlyWhenTargetChanges(t *testing.T) {
	r, _, output := newOverrideREPL(t, "small", "large")
	installWarmPricedREPLCache(t, r)
	bind, after, ok := r.moveTo(context.Background(), 1, "stronger evidence")
	if !ok || bind == nil || !bind() {
		t.Fatal("feasible midturn move did not commit")
	}
	if after != nil {
		after()
	}
	if text := output(); !strings.Contains(text, "modeled value") {
		t.Fatalf("midturn move hid abandoned warmth:\n%s", text)
	}

	shared, _, sharedOutput := newOverrideREPL(t, "small")
	installWarmPricedREPLCache(t, shared)
	shared.config.Tiers[1].Target = shared.config.Tiers[0].Target
	shared.config.Tiers[1].Fallbacks = nil
	bind, after, ok = shared.moveTo(context.Background(), 1, "rank-only move")
	if !ok || bind == nil || !bind() {
		t.Fatal("shared-target tier move did not commit")
	}
	if after != nil {
		after()
	}
	if text := sharedOutput(); strings.Contains(text, "warm prefix") || strings.Contains(text, "modeled value") {
		t.Fatalf("shared-target rank move invented cache abandonment:\n%s", text)
	}
}

func TestREPLTierPromptUsesFallbackAndRestoresPermanentPin(t *testing.T) {
	r, capture, _ := newOverrideREPL(t, "small", "backup")
	r.sticky.Pin(0)
	prior := r.loop.Binding()
	r.command(context.Background(), "/t2 use the fallback")
	if len(capture.bodies) != 1 || !strings.Contains(capture.bodies[0], `"model":"backup"`) {
		t.Fatalf("fallback request = %#v", capture.bodies)
	}
	if r.loop.Binding().Target.ID() != prior.Target.ID() || r.sticky.Rank() != 0 || !r.sticky.Pinned() {
		t.Fatalf("permanent pin not restored: target=%s rank=%d pinned=%v",
			r.loop.Binding().Target.ID(), r.sticky.Rank(), r.sticky.Pinned())
	}
}

func TestREPLBareTierStillPinsPermanently(t *testing.T) {
	r, _, _ := newOverrideREPL(t, "small", "large")
	r.command(context.Background(), "/t2")
	if r.tier.ID != "t2" || r.loop.Binding().Target.ModelID != "large" || !r.sticky.Pinned() || r.sticky.Rank() != 1 {
		t.Fatalf("bare tier did not pin: tier=%s target=%s rank=%d pinned=%v",
			r.tier.ID, r.loop.Binding().Target.ModelID, r.sticky.Rank(), r.sticky.Pinned())
	}
}

func TestREPLTierPromptProbeCancellationLeavesStateUntouched(t *testing.T) {
	r, capture, _ := newOverrideREPL(t, "small", "large")
	prior := r.loop.Binding()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.command(ctx, "/t2 never send")
	if len(capture.bodies) != 0 || r.tier.ID != "t1" || r.loop.Binding().Target.ID() != prior.Target.ID() || r.sticky.Rank() != 0 || r.sticky.Pinned() {
		t.Fatalf("cancelled probe changed state: requests=%d tier=%s target=%s rank=%d pinned=%v",
			len(capture.bodies), r.tier.ID, r.loop.Binding().Target.ID(), r.sticky.Rank(), r.sticky.Pinned())
	}
}

func TestREPLLateSuccessfulRouteProbeCannotCommitAfterCancellation(t *testing.T) {
	r, capture, _ := newOverrideREPL(t, "small", "large")
	if err := persistRuntimeBinding(r.loop.Session, r.tier, false); err != nil {
		t.Fatal(err)
	}
	beforeBinding := r.loop.Binding()
	beforeRuntime := r.loop.Session.State().RuntimeBinding
	beforeRank, beforePinned := r.sticky.Rank(), r.sticky.Pinned()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // model a probe that ignored this cancellation and returned success
	destination := r.config.Tiers[1]
	plan := turnPlan{Decision: route.Decision{
		Tier: destination.ID, Target: destination.Target.ID(), Rationale: "late successful probe",
	}}
	err := r.acceptTurnResolution(ctx, destination, beforeBinding.Provider, "late fallback", plan)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("late resolution error = %v, want context.Canceled", err)
	}
	if got := r.loop.Session.State().RuntimeBinding; got != beforeRuntime {
		t.Fatalf("late resolution changed durable binding: got %+v want %+v", got, beforeRuntime)
	}
	if r.tier.ID != "t1" || r.loop.Binding().Target.ID() != beforeBinding.Target.ID() ||
		r.sticky.Rank() != beforeRank || r.sticky.Pinned() != beforePinned || r.route != nil {
		t.Fatalf("late resolution changed runtime: tier=%s target=%s rank=%d pinned=%v route=%v",
			r.tier.ID, r.loop.Binding().Target.ID(), r.sticky.Rank(), r.sticky.Pinned(), r.route != nil)
	}
	if len(capture.bodies) != 0 {
		t.Fatalf("late resolution sent %d provider requests", len(capture.bodies))
	}
}

func TestREPLSameTargetResolutionInstallsFreshProviderClient(t *testing.T) {
	r, _, _ := newOverrideREPL(t, "small", "large")
	stale := &racedProvider{}
	fresh := &racedProvider{}
	binding := r.loop.Binding()
	binding.Provider = stale
	r.loop.Bind(binding)

	plan := turnPlan{Decision: route.Decision{
		Tier: r.tier.ID, Target: r.tier.Target.ID(), Rationale: "same target after provider reset",
	}}
	if err := r.acceptTurnResolution(context.Background(), r.tier, fresh, "", plan); err != nil {
		t.Fatal(err)
	}
	if got := r.loop.Binding().Provider; got != fresh {
		t.Fatalf("same-target resolution kept stale provider %p; want prepared client %p", got, fresh)
	}
}

func TestREPLTierPromptTurnErrorRestoresExactBinding(t *testing.T) {
	r, capture, _ := newOverrideREPL(t, "small", "large")
	capture.mu.Lock()
	capture.status = http.StatusInternalServerError
	capture.mu.Unlock()
	prior := r.loop.Binding()
	r.sticky.Pin(0)
	err := r.turnOnTier(context.Background(), "t2", "fail after binding", nil)
	if err == nil {
		t.Fatal("forced provider failure returned nil")
	}
	after := r.loop.Binding()
	if r.tier.ID != "t1" || after.Target.ID() != prior.Target.ID() || after.Provider != prior.Provider ||
		after.Cache != prior.Cache || r.sticky.Rank() != 0 || !r.sticky.Pinned() {
		t.Fatalf("error restore: tier=%s target=%s cache=%p/%p rank=%d pinned=%v",
			r.tier.ID, after.Target.ID(), after.Cache, prior.Cache, r.sticky.Rank(), r.sticky.Pinned())
	}
}

func TestREPLTierPromptTurnCancellationRestoresExactBinding(t *testing.T) {
	r, capture, _ := newOverrideREPL(t, "small", "large")
	entered := make(chan struct{})
	capture.mu.Lock()
	capture.blockChat = true
	capture.entered = entered
	capture.mu.Unlock()
	prior := r.loop.Binding()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- r.turnOnTier(ctx, "t2", "cancel after binding", nil) }()
	<-entered
	cancel()
	if err := <-result; err == nil {
		t.Fatal("cancelled provider turn returned nil")
	}
	after := r.loop.Binding()
	if r.tier.ID != "t1" || after.Target.ID() != prior.Target.ID() || after.Provider != prior.Provider ||
		after.Cache != prior.Cache || r.sticky.Rank() != 0 || r.sticky.Pinned() {
		t.Fatalf("cancel restore: tier=%s target=%s rank=%d pinned=%v",
			r.tier.ID, after.Target.ID(), r.sticky.Rank(), r.sticky.Pinned())
	}
}

func TestREPLTierPromptInfeasibleDestinationDoesNotMutate(t *testing.T) {
	r, capture, _ := newOverrideREPL(t, "small")
	prior := r.loop.Binding()
	err := r.turnOnTier(context.Background(), "t2", "cannot run", nil)
	if err == nil {
		t.Fatal("unavailable tier unexpectedly ran")
	}
	after := r.loop.Binding()
	if len(capture.bodies) != 0 || r.tier.ID != "t1" || after.Target.ID() != prior.Target.ID() ||
		after.Provider != prior.Provider || after.Cache != prior.Cache || r.sticky.Rank() != 0 || r.sticky.Pinned() {
		t.Fatalf("infeasible override mutated state: calls=%d tier=%s target=%s rank=%d pinned=%v",
			len(capture.bodies), r.tier.ID, after.Target.ID(), r.sticky.Rank(), r.sticky.Pinned())
	}
}

func TestREPLTierPromptSecretGatesAssembledMention(t *testing.T) {
	r, capture, _ := newOverrideREPL(t, "small", "large")
	if err := os.WriteFile(filepath.Join(r.workspace, "secret.txt"), []byte(testGitHubToken), 0o600); err != nil {
		t.Fatal(err)
	}
	r.in = bufio.NewReader(strings.NewReader("\n")) // default-safe: drop it
	r.command(context.Background(), "/t2 inspect @secret.txt")
	if len(capture.bodies) != 0 {
		t.Fatalf("secret-gate rejection still sent %d requests", len(capture.bodies))
	}
	if r.tier.ID != "t1" || r.loop.Binding().Target.ModelID != "small" {
		t.Fatal("dropped prompt changed the active binding")
	}
}

func TestREPLOffLadderImageRefusesBeforeProviderCall(t *testing.T) {
	r, capture, _ := newOverrideREPL(t, "small", "large")
	capture.mu.Lock()
	capture.showCaps = `{"capabilities":["tools"]}`
	capture.mu.Unlock()
	r.tier.ID = "-model"
	image := provider.Image{MediaType: "image/png", Data: []byte{0x89, 0x50, 0x4e, 0x47}}
	err := r.turnPrepared(context.Background(), "inspect the image", []provider.Image{image}, false)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "image") {
		t.Fatalf("off-ladder non-vision target was not refused: %v", err)
	}
	if len(capture.bodies) != 0 || len(r.loop.Session.State().Messages) != 0 {
		t.Fatalf("infeasible off-ladder image reached provider/session: calls=%d messages=%d",
			len(capture.bodies), len(r.loop.Session.State().Messages))
	}
}

func TestREPLOffLadderParameterizedTargetReprobesPositiveVision(t *testing.T) {
	r, capture, _ := newOverrideREPL(t, "small", "large")
	r.tier.ID = "-model"
	r.tier.Target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}
	binding := r.loop.Binding()
	binding.Target = r.tier.Target
	r.loop.Bind(binding)
	image := provider.Image{MediaType: "image/png", Data: []byte{0x89, 0x50, 0x4e, 0x47}}
	if err := r.turnPrepared(context.Background(), "inspect the image", []provider.Image{image}, false); err != nil {
		t.Fatal(err)
	}
	if len(capture.bodies) != 1 {
		t.Fatalf("positive off-ladder image turn made %d provider calls, want 1", len(capture.bodies))
	}
	if vision, known := r.providers.probedVision(r.tier.Target); !known || !vision {
		t.Fatalf("parameterized target probe evidence = vision %v known %v", vision, known)
	}
}

func TestREPLOffLadderContextEnvelopeRefusesBeforeProviderCall(t *testing.T) {
	r, capture, _ := newOverrideREPL(t, "small", "large")
	r.tier.ID = "-resumed"
	err := r.turnPrepared(context.Background(), strings.Repeat("x", 120_000), nil, false)
	if err == nil || (!strings.Contains(strings.ToLower(err.Error()), "context") && !strings.Contains(strings.ToLower(err.Error()), "tokens")) {
		t.Fatalf("off-ladder context overflow was not refused: %v", err)
	}
	if len(capture.bodies) != 0 {
		t.Fatalf("off-ladder context overflow reached provider %d times", len(capture.bodies))
	}
}

func TestREPLOffLadderBudgetRefusesBeforeProviderCall(t *testing.T) {
	r, capture, _ := newOverrideREPL(t, "small", "large")
	r.tier.ID = "-model"
	r.catalog = catalogWithLocalModels(t, localModelSpec{
		name: "small", contextWindow: 100_000, inputPerMTok: "1", outputPerMTok: "1",
	})
	r.loop.Catalog = r.catalog
	r.caches = newCacheSet(r.tier.Target, cacheFor(r.tier.Target, r.catalog))
	r.budget.set(1)
	err := r.turnPrepared(context.Background(), "a request whose output allowance costs more than one micro-dollar", nil, false)
	if err == nil || (!strings.Contains(strings.ToLower(err.Error()), "budget") && !strings.Contains(strings.ToLower(err.Error()), "cost")) {
		t.Fatalf("off-ladder hard budget was not refused: %v", err)
	}
	if len(capture.bodies) != 0 {
		t.Fatalf("off-ladder over-budget turn reached provider %d times", len(capture.bodies))
	}
}

// The REPL is a first-class surface for the routing setting: /routing off
// holds the current rung from here too, and the choice outlives the process.
func TestREPLRoutingOffPersistsAndReports(t *testing.T) {
	r, _, readOutput := newOverrideREPL(t, "small", "large", "backup")
	r.config.Path = filepath.Join(t.TempDir(), config.FileName)

	if r.command(context.Background(), "/routing off") {
		t.Fatal("/routing off asked the REPL to exit")
	}
	if r.config.RouteAutoOn() {
		t.Fatal("/routing off left the setting on")
	}
	saved, err := config.LoadFile(r.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.RouteAutoOn() {
		t.Fatal("routing off did not persist")
	}
	if out := readOutput(); !strings.Contains(out, "routing off") {
		t.Errorf("output did not confirm the hold: %q", out)
	}

	if r.command(context.Background(), "/routing on") {
		t.Fatal("/routing on asked the REPL to exit")
	}
	if !r.config.RouteAutoOn() {
		t.Fatal("/routing on did not restore the setting")
	}
}

func TestREPLRoutingSaveFailureLeavesLivePostureUnchanged(t *testing.T) {
	for _, initial := range []bool{false, true} {
		initial := initial
		t.Run(map[bool]string{false: "off", true: "on"}[initial], func(t *testing.T) {
			r, _, readOutput := newOverrideREPL(t, "small", "large")
			r.config.Path = t.TempDir() // a directory cannot be replaced by the config file
			r.config.RouteAuto = &initial
			r.watcher = newWatcher(nil, r.sticky, len(r.config.Tiers)-1, nil)
			r.watcher.setPaused(!initial)

			requested := !initial
			if r.command(context.Background(), "/routing "+map[bool]string{false: "off", true: "on"}[requested]) {
				t.Fatal("failed routing save asked the REPL to exit")
			}
			if out := readOutput(); !strings.Contains(out, "saving the routing setting failed") {
				t.Fatalf("failed save output = %q", out)
			}
			if r.config.RouteAutoOn() != initial {
				t.Fatalf("failed save changed live config from %v to %v", initial, r.config.RouteAutoOn())
			}
			if r.watcher.isPaused() != !initial {
				t.Fatalf("failed save changed watcher pause from %v to %v", !initial, r.watcher.isPaused())
			}
		})
	}
}
