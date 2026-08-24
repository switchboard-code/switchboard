package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	route "github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

func testModel(t *testing.T) *tuiModel {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "test:7b"}
	sess, err := store.Create(t.TempDir(), target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })

	cfg := &config.Config{Tiers: []config.Tier{{ID: "t1", Label: "light", Target: target}}}
	registry, err := tools.NewRegistry(t.TempDir(), execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	loop := &agent.Loop{
		Session: sess,
		Target:  target,
		Tools:   registry,
		Perms:   permission.NewEngine(permission.ModeDefault, execution.Capability{}),
	}
	app := &tuiApp{
		loop:      loop,
		store:     store,
		config:    cfg,
		catalog:   &catalog.Catalog{Revision: "test"},
		tier:      cfg.Tiers[0],
		workspace: "/tmp/ws",
	}
	m := newTUIModel(app, darkTheme(), newMarkdown(80, true), newTextarea())
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return m
}

// The status line is the product's always-on readout: routing visible at rest.
func TestStatusLineShowsRouteAtRest(t *testing.T) {
	m := testModel(t)
	view := m.View()
	for _, want := range []string{"t1 · light", "ollama/local/test:7b", "default", "unpriced"} {
		if !strings.Contains(view, want) {
			t.Errorf("status line missing %q:\n%s", want, view)
		}
	}
}

func TestParameterizedTargetLabelsStayReadableAcrossUI(t *testing.T) {
	m := testModel(t)
	temperature := 0.5
	target := m.app.tier.Target
	target.Params = provider.Params{
		MaxOutputTokens: 2_048,
		Temperature:     &temperature,
		Reasoning:       &provider.Reasoning{Enabled: true, Effort: "high"},
	}
	m.app.tier.Target = target
	binding := m.app.loop.Binding()
	binding.Target = target
	m.app.loop.Bind(binding)
	m.tierLine = m.app.tierLine()
	cmdWhy(m, "")
	why := strings.Join(m.tr.flat, "\n")
	replLine := (&repl{loop: m.app.loop, tier: m.app.tier}).tierLine()

	for name, output := range map[string]string{
		"TUI tier line":  m.app.tierLine(),
		"REPL tier line": replLine,
		"/why":           why,
	} {
		for _, readable := range []string{"ollama/local/test:7b", "think:high", "max:2048", "temp:0.5"} {
			if !strings.Contains(output, readable) {
				t.Errorf("%s = %q, missing %q", name, output, readable)
			}
		}
		for _, opaque := range []string{"rt2:", "b2xsYW1h", "dGVzdDo3Yg"} {
			if strings.Contains(output, opaque) {
				t.Errorf("%s = %q, leaked opaque identity %q", name, output, opaque)
			}
		}
	}
}

func TestOpeningBannerDistinguishesParameterizedRungs(t *testing.T) {
	m := testModel(t)
	parameterized := m.app.tier
	parameterized.Target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}
	plain := m.app.tier
	plain.ID = "t2"
	plain.Label = "default"
	m.app.config.Tiers = []config.Tier{parameterized, plain}
	m.app.tier = parameterized
	binding := m.app.loop.Binding()
	binding.Target = parameterized.Target
	m.app.loop.Bind(binding)
	m.addBanner(m.app.loop.Session, false)
	banner := stripANSI(strings.Join(m.tr.flat, "\n"))
	if strings.Count(banner, parameterized.Target.ModelID) < 2 || !strings.Contains(banner, "think:high") {
		t.Fatalf("opening banner did not distinguish same-model parameter variants:\n%s", banner)
	}
	if strings.Contains(banner, "rt2:") {
		t.Fatalf("opening banner leaked machine identity:\n%s", banner)
	}
}

func TestSlashSuggestionsAppear(t *testing.T) {
	m := testModel(t)
	m.ta.SetValue("/he")
	if !m.suggestionsVisible() {
		t.Fatal("typing /he showed no suggestions")
	}
	if view := m.suggestionsView(); !strings.Contains(view, "/help") {
		t.Fatalf("suggestions missing /help:\n%s", view)
	}
}

func TestHelpCommandRenders(t *testing.T) {
	m := testModel(t)
	m.ta.SetValue("/help")
	m.submit()
	joined := strings.Join(m.tr.flat, "\n")
	if !strings.Contains(joined, "commands") || !strings.Contains(joined, "/resume") {
		t.Fatalf("/help did not land in the transcript:\n%s", joined)
	}
}

func TestRoutingPhaseSerializesSubmittedPrompts(t *testing.T) {
	m := testModel(t)
	if cmd := m.launchTurn("first", nil); cmd == nil || !m.busy || m.turnCancel == nil {
		t.Fatalf("routing did not own the busy/cancel state: busy=%v cancel=%v", m.busy, m.turnCancel != nil)
	}
	if cmd := m.enqueue("second", ""); cmd != nil || len(m.queue) != 1 {
		t.Fatalf("prompt was not queued behind routing: cmd=%v queue=%v", cmd != nil, m.queue)
	}
	next := m.onTurnPlan(turnPlanMsg{generation: m.turnGeneration, err: errors.New("probe failed")})
	if next == nil || len(m.queue) != 0 || !m.busy {
		t.Fatalf("routing failure did not advance the queue: next=%v queue=%v busy=%v", next != nil, m.queue, m.busy)
	}
}

func TestCancelledRoutingRejectsItsLateResult(t *testing.T) {
	m := testModel(t)
	before := m.app.loop.Session.State()
	cmd := m.launchTurn("never send this", nil)
	oldGeneration := m.turnGeneration
	if cmd == nil || !m.turnPlanning {
		t.Fatal("routing did not enter its cancellable planning phase")
	}
	if next := m.interrupt(); next != nil {
		// There is no queued turn in this test.
		t.Fatal("cancelling an unqueued plan returned work")
	}
	if m.busy || m.turnPlanning || m.turnCancel != nil || m.turnGeneration == oldGeneration {
		t.Fatalf("cancel did not invalidate planning: busy=%v planning=%v cancel=%v generation=%d",
			m.busy, m.turnPlanning, m.turnCancel != nil, m.turnGeneration)
	}

	msg := cmd().(turnPlanMsg)
	if msg.generation != oldGeneration || !errors.Is(msg.err, context.Canceled) {
		t.Fatalf("late result = %+v, want cancelled generation %d", msg, oldGeneration)
	}
	if next := m.onTurnPlan(msg); next != nil {
		t.Fatal("stale plan launched work")
	}
	if after := m.app.loop.Session.State(); len(after.Messages) != len(before.Messages) {
		t.Fatalf("cancelled planning appended a message: before=%d after=%d", len(before.Messages), len(after.Messages))
	}
}

func TestTierSwitchOwnsBusyStateAndRejectsLateResult(t *testing.T) {
	m := testModel(t)
	second := config.Tier{ID: "t2", Label: "strong", Target: provider.RouteTarget{
		Provider: "ollama", Surface: "local", ModelID: "strong:7b",
	}}
	m.app.config.Tiers = append(m.app.config.Tiers, second)
	cmd := m.switchTier("t2")
	operation, sourceID := m.operationGeneration, m.operationSourceID
	if cmd == nil || !m.busy || !m.operationActive || m.turnCancel == nil {
		t.Fatalf("switch did not claim ownership: busy=%v active=%v cancel=%v", m.busy, m.operationActive, m.turnCancel != nil)
	}
	if next := m.enqueue("wait for the switch", ""); next != nil || len(m.queue) != 1 {
		t.Fatalf("prompt did not queue behind switch: cmd=%v queue=%v", next != nil, m.queue)
	}

	// Simulate another generation taking ownership before the old probe reports.
	m.finishOperation(operation, false)
	_, generation := m.startPlanning()
	m.onTierSwitch(tierSwitchMsg{tier: second, operation: operation, sourceID: sourceID})
	if m.app.tier.ID != "t1" || !m.busy || !m.turnPlanning || m.turnGeneration != generation {
		t.Fatalf("late switch mutated active turn: tier=%s busy=%v planning=%v generation=%d",
			m.app.tier.ID, m.busy, m.turnPlanning, m.turnGeneration)
	}
	m.finishPlanning()
}

func TestOwnedTierSwitchAppliesThenAdvancesQueue(t *testing.T) {
	m := testModel(t)
	second := config.Tier{ID: "t2", Label: "strong", Target: provider.RouteTarget{
		Provider: "ollama", Surface: "local", ModelID: "strong:7b",
	}}
	m.app.config.Tiers = append(m.app.config.Tiers, second)
	if cmd := m.switchTier("t2"); cmd == nil {
		t.Fatal("switch returned no probe command")
	}
	operation, sourceID := m.operationGeneration, m.operationSourceID
	m.queue = append(m.queue, "next")
	next := m.onTierSwitch(tierSwitchMsg{tier: second, client: m.app.loop.Binding().Provider,
		operation: operation, sourceID: sourceID})
	if m.app.tier.ID != "t2" || m.operationActive || next == nil || !m.turnPlanning {
		t.Fatalf("switch/queue state: tier=%s active=%v next=%v planning=%v",
			m.app.tier.ID, m.operationActive, next != nil, m.turnPlanning)
	}
	m.finishPlanning()
}

func TestSharedTargetTierSwitchStillReportsFallbackSubstitution(t *testing.T) {
	m := testModel(t)
	shared := m.app.tier.Target
	second := config.Tier{ID: "t2", Label: "shared fallback", Target: shared}
	m.app.config.Tiers = append(m.app.config.Tiers, second)
	if cmd := m.switchTier("t2"); cmd == nil {
		t.Fatal("distinct tier switch returned no owned probe command")
	}
	operation, sourceID := m.operationGeneration, m.operationSourceID
	const note = "t2 is served by its fallback shared: primary is unavailable"
	m.onTierSwitch(tierSwitchMsg{
		tier: second, client: m.app.loop.Binding().Provider, note: note,
		operation: operation, sourceID: sourceID,
	})
	if got := strings.Join(m.tr.flat, "\n"); !strings.Contains(got, note) {
		t.Fatalf("shared-target fallback substitution was hidden:\n%s", got)
	}
	raw, err := os.ReadFile(m.app.loop.Session.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), note) {
		t.Fatalf("shared-target fallback substitution was not persisted:\n%s", raw)
	}
	if strings.Contains(strings.Join(m.routeLog, "\n"), "cache") {
		t.Fatalf("shared target switch invented cache abandonment: %v", m.routeLog)
	}
}

func TestSameTierRebindProbesAndUsesNewConfiguredTarget(t *testing.T) {
	r, capture, _ := newOverrideREPL(t, "small", "large")
	app := &tuiApp{
		loop: r.loop, config: r.config, catalog: r.catalog, tier: r.tier,
		providers: r.providers, workspace: r.workspace, sticky: r.sticky,
		budget: r.budget, caches: r.caches,
	}
	m := newTUIModel(app, darkTheme(), newMarkdown(80, true), newTextarea())
	if err := app.config.BindTier("t1", "replacement", "ollama/large", "", ""); err != nil {
		t.Fatal(err)
	}
	cmd := m.switchTier("t1")
	if cmd == nil || !m.operationActive {
		t.Fatalf("same tier with a new target short-circuited: cmd=%v active=%v", cmd != nil, m.operationActive)
	}
	raw := cmd()
	msg, ok := raw.(tierSwitchMsg)
	if !ok {
		t.Fatalf("switch command returned %T, want tierSwitchMsg", raw)
	}
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	m.onTierSwitch(msg)
	if app.tier.Target.ModelID != "large" || app.loop.Binding().Target.ModelID != "large" {
		t.Fatalf("same-tier rebind stayed on stale target: tier=%s binding=%s",
			app.tier.Target.ModelID, app.loop.Binding().Target.ModelID)
	}

	// The TUI observer needs a running Bubble Tea program. This assertion only
	// exercises the rebound runtime, so use the loop's inert observer directly.
	app.loop.SetObserver(agent.NopObserver{})
	if err := app.loop.TurnMessage(context.Background(), provider.UserText("use the rebound model")); err != nil {
		t.Fatal(err)
	}
	capture.mu.Lock()
	bodies := append([]string(nil), capture.bodies...)
	capture.mu.Unlock()
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"model":"large"`) || strings.Contains(bodies[0], `"model":"small"`) {
		t.Fatalf("post-switch provider requests = %#v, want one request on large and none on small", bodies)
	}
}

func TestCancelledTierSwitchCannotCommitSuccessfulLateProbe(t *testing.T) {
	m := testModel(t)
	second := config.Tier{ID: "t2", Target: provider.RouteTarget{
		Provider: "ollama", Surface: "local", ModelID: "strong:7b",
	}}
	m.app.config.Tiers = append(m.app.config.Tiers, second)
	m.switchTier("t2")
	operation, sourceID := m.operationGeneration, m.operationSourceID
	m.queue = append(m.queue, "after cancellation")
	m.interrupt()
	if !m.operationCancelling {
		t.Fatal("interrupt did not mark the owned switch as cancelling")
	}
	next := m.onTierSwitch(tierSwitchMsg{tier: second, client: m.app.loop.Binding().Provider,
		operation: operation, sourceID: sourceID})
	if m.app.tier.ID != "t1" || m.operationActive || next == nil || !m.turnPlanning || len(m.moves) != 0 {
		t.Fatalf("cancelled switch committed or stranded queue: tier=%s active=%v next=%v planning=%v moves=%v",
			m.app.tier.ID, m.operationActive, next != nil, m.turnPlanning, m.moves)
	}
	m.finishPlanning()
}

func TestActiveTierOverrideDoesNotInventMoveOrAbandonCache(t *testing.T) {
	m := testModel(t)
	cache := &agent.Cache{}
	binding := m.app.loop.Binding()
	binding.Target = m.app.tier.Target
	binding.Cache = cache
	m.app.loop.Bind(binding)
	m.app.caches = newCacheSet(m.app.tier.Target, cache)
	m.app.sticky = route.NewSticky(route.Policy{MinimumDwell: 1}, 0)
	m.app.sticky.Observe(route.RepeatedToolCall)
	m.app.sticky.CallServed()

	m.applyOverrideBinding(overrideProbeMsg{tier: m.app.tier, client: binding.Provider})
	if len(m.moves) != 0 || m.app.loop.Binding().Cache != cache {
		t.Fatalf("same-tier override changed routing/cache: moves=%v cache_preserved=%v", m.moves, m.app.loop.Binding().Cache == cache)
	}
	if strings.Contains(strings.Join(m.routeLog, "\n"), "cache") {
		t.Fatalf("same-tier override claimed cache abandonment: %v", m.routeLog)
	}
	if m.app.sticky == nil || !m.app.sticky.Pinned() {
		t.Fatal("same-tier override did not retain one-turn pin provenance")
	}

	m.app.sticky.StartTurn() // borrowed-turn mutations must disappear on restore
	m.restoreOverride()
	if len(m.moves) != 0 || m.app.loop.Binding().Cache != cache || m.app.sticky.Pinned() {
		t.Fatalf("same-tier restore changed routing/cache: moves=%v cache_preserved=%v pinned=%v",
			m.moves, m.app.loop.Binding().Cache == cache, m.app.sticky.Pinned())
	}
	if move := m.app.sticky.Assess(1); move.Direction != 1 {
		t.Fatalf("same-tier restore lost dwell/evidence state: %+v", move)
	}
}

func TestSameTargetOverrideRefreshesProviderThroughRestore(t *testing.T) {
	m := testModel(t)
	stale := &racedProvider{}
	fresh := &racedProvider{}
	binding := m.app.loop.Binding()
	binding.Provider = stale
	m.app.loop.Bind(binding)

	m.applyOverrideBinding(overrideProbeMsg{tier: m.app.tier, client: fresh})
	if got := m.app.loop.Binding().Provider; got != fresh {
		t.Fatalf("same-target override kept stale provider %p; want prepared client %p", got, fresh)
	}
	if m.restoreBinding.Provider != fresh {
		t.Fatal("override restore snapshot retained the discarded provider")
	}
	m.restoreOverride()
	if got := m.app.loop.Binding().Provider; got != fresh {
		t.Fatalf("override restore reinstated stale provider %p; want prepared client %p", got, fresh)
	}
}

func TestResumeUsesConfiguredFallbackAndPersistsSubstitution(t *testing.T) {
	m := testModel(t)
	server := fakeOllama(t, "resume-backup")
	resumedTier := ollamaTier("t2", "resume-primary", "resume-backup")
	m.app.catalog = catalogWithLocalModels(t,
		localModelSpec{name: "resume-primary", contextWindow: 100_000},
		localModelSpec{name: "resume-backup", contextWindow: 100_000},
	)
	m.app.config.Tiers = append(m.app.config.Tiers, resumedTier)
	m.app.providers = newProviders(server.URL, m.app.config)
	m.app.caches = newCacheSet(m.app.tier.Target, m.app.loop.Binding().Cache)
	m.app.sticky = route.NewSticky(route.Policy{}, 0)

	old, err := m.app.store.Create(m.app.workspace, resumedTier.Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	id, path := old.ID(), old.Path()
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := m.reopen(id)
	if cmd == nil {
		t.Fatal("resume returned no asynchronous command")
	}
	msg := cmd().(sessionSwapMsg)
	if msg.err != nil || msg.tier.Target.ModelID != "resume-backup" || !msg.warnNote || !strings.Contains(msg.note, "fallback") {
		t.Fatalf("resume fallback result = %#v", msg)
	}
	m.onSessionSwap(msg)
	if m.app.loop.Session.ID() != id || m.app.loop.Binding().Target.ModelID != "resume-backup" {
		t.Fatalf("resume did not land on fallback: session=%s target=%s", m.app.loop.Session.ID(), m.app.loop.Binding().Target.ModelID)
	}
	timeline, err := session.ReadTimeline(path)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range timeline {
		if event.Note != nil && event.Note.Level == "warn" && strings.Contains(event.Note.Text, "fallback") {
			found = true
		}
	}
	if !found {
		t.Fatal("resume fallback substitution was not persisted on resumed session")
	}
}

func TestCrossTierOverrideRestoresExactStateWithoutPermanentMove(t *testing.T) {
	m := testModel(t)
	second := config.Tier{ID: "t2", Target: provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "other:7b"}}
	m.app.config.Tiers = append(m.app.config.Tiers, second)
	cache := &agent.Cache{}
	binding := m.app.loop.Binding()
	binding.Cache = cache
	m.app.loop.Bind(binding)
	m.app.caches = newCacheSet(m.app.tier.Target, cache)
	m.app.sticky = route.NewSticky(route.Policy{MinimumDwell: 1}, 0)
	m.app.sticky.Observe(route.RepeatedToolCall)
	m.app.sticky.CallServed()

	m.applyOverrideBinding(overrideProbeMsg{tier: second, client: binding.Provider})
	if len(m.moves) != 0 || strings.Contains(strings.Join(m.routeLog, "\n"), "cache") {
		t.Fatalf("temporary cross-tier borrow became a permanent move: moves=%v log=%v", m.moves, m.routeLog)
	}
	m.app.sticky.StartTurn()
	m.restoreOverride()
	if m.app.tier.ID != "t1" || m.app.loop.Binding().Cache != cache || m.app.sticky.Pinned() || m.app.sticky.Rank() != 0 {
		t.Fatalf("cross-tier restore: tier=%s cache=%v rank=%d pinned=%v",
			m.app.tier.ID, m.app.loop.Binding().Cache == cache, m.app.sticky.Rank(), m.app.sticky.Pinned())
	}
	if move := m.app.sticky.Assess(1); move.Direction != 1 {
		t.Fatalf("cross-tier restore lost prior policy evidence: %+v", move)
	}
}

func TestCancelledSessionOperationRejectsItsLateSwap(t *testing.T) {
	m := testModel(t)
	source := m.app.loop.Session
	cmd := m.clearSession()
	if cmd == nil || !m.operationActive || !m.busy {
		t.Fatal("clear did not claim exclusive ownership")
	}
	msg, ok := cmd().(sessionSwapMsg)
	if !ok || msg.err != nil || msg.sess == nil {
		t.Fatalf("clear result = %#v", msg)
	}

	m.interrupt()
	if !m.operationActive || !m.operationCancelling || !m.busy {
		t.Fatal("cancel released ownership before the operation reported completion")
	}
	m.onSessionSwap(msg)
	if m.operationActive || m.busy {
		t.Fatal("cancelled operation did not release ownership at completion")
	}
	if m.app.loop.Session != source {
		t.Fatal("late clear result replaced the source after cancellation")
	}
	if err := msg.sess.AppendNote("info", "must be closed"); !errors.Is(err, session.ErrSessionPoisoned) {
		t.Fatalf("late session remained writable: %v", err)
	}
}

func TestStalePlanCannotClearANewerPlanningGeneration(t *testing.T) {
	m := testModel(t)
	_ = m.launchTurn("old", nil)
	oldGeneration := m.turnGeneration
	m.interrupt()
	_ = m.launchTurn("new", nil)
	newGeneration := m.turnGeneration

	if cmd := m.onTurnPlan(turnPlanMsg{generation: oldGeneration, err: context.Canceled}); cmd != nil {
		t.Fatal("stale result returned work")
	}
	if !m.busy || !m.turnPlanning || m.turnCancel == nil || m.turnGeneration != newGeneration {
		t.Fatalf("stale result damaged new planning: busy=%v planning=%v cancel=%v generation=%d want=%d",
			m.busy, m.turnPlanning, m.turnCancel != nil, m.turnGeneration, newGeneration)
	}
	m.interrupt()
}

func TestTUIRuntimeTierAndLoopBindingStayCoherent(t *testing.T) {
	m := testModel(t)
	a := m.app.tier
	b := config.Tier{ID: "t2", Label: "heavy", Target: provider.RouteTarget{
		Provider: "ollama", Surface: "local", ModelID: "other:14b",
	}}
	m.app.config.Tiers = append(m.app.config.Tiers, b)
	m.app.caches = newCacheSet(a.Target, nil)
	client := &racedProvider{}
	m.app.bindRuntime(a, client)

	errs := make(chan string, 1)
	report := func(text string) {
		select {
		case errs <- text:
		default:
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 10_000; i++ {
			m.app.bindRuntime(b, client)
			m.app.bindRuntime(a, client)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20_000; i++ {
			tier, binding := m.app.runtimeSnapshot()
			if tier.Target.ID() != binding.Target.ID() {
				report(fmt.Sprintf("torn runtime state: tier=%s binding=%s", tier.Target.ID(), binding.Target.ID()))
				return
			}
		}
	}()
	wg.Wait()
	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}
}

func TestTierAutoReleasesManualPin(t *testing.T) {
	m := testModel(t)
	m.app.sticky = route.NewSticky(route.Policy{}, 0)
	m.app.sticky.Pin(0)
	if cmd := cmdTier(m, "auto"); cmd != nil {
		t.Fatal("/tier auto unexpectedly started asynchronous work")
	}
	if m.app.sticky.Pinned() {
		t.Fatal("/tier auto left the router pinned")
	}
}

func TestUnknownCommandIsANotice(t *testing.T) {
	m := testModel(t)
	m.ta.SetValue("/frobnicate")
	if cmd := m.submit(); cmd != nil {
		if msg := cmd(); msg != nil {
			m.Update(msg)
		}
	}
	last := m.tr.last()
	if last == nil || last.kind != kindNotice || last.level != "error" {
		t.Fatalf("unknown command did not produce an error notice: %+v", last)
	}
}

func TestModeCycleMovesAndReports(t *testing.T) {
	m := testModel(t)
	for _, want := range []permission.Mode{
		permission.ModeAcceptEdits, permission.ModeAuto, permission.ModePlan, permission.ModeYOLO, permission.ModeDefault,
	} {
		m.cycleMode()
		if m.mode != want || m.app.loop.Perms.Mode() != want {
			t.Fatalf("shift+tab moved UI=%s engine=%s, want %s", m.mode, m.app.loop.Perms.Mode(), want)
		}
		if m.mode == permission.ModeBypass {
			t.Fatalf("shift+tab entered explicit-only mode %s", m.mode)
		}
		if got := m.app.loop.Perms.Execution().FullAccess(); got != (want == permission.ModeYOLO) {
			t.Fatalf("full host access = %v in %s, want it only under yolo", got, want)
		}
	}
	copy := stripANSI(strings.Join(m.tr.flat, "\n"))
	if !strings.Contains(copy, "FULL HOST ACCESS") {
		t.Fatal("cycling into yolo did not say what it grants")
	}
	cmdMode(m, "default")
	if m.mode != permission.ModeDefault || m.app.loop.Perms.Execution().FullAccess() {
		t.Fatal("leaving yolo did not restore the confined posture")
	}
}

func TestAutoModeCopyKeepsHostDirectCommandsWithTheHuman(t *testing.T) {
	m := testModel(t)
	cmdMode(m, "auto")
	copy := stripANSI(strings.Join(m.tr.flat, "\n"))
	for _, want := range []string{"active verified sandbox", "cheap approver", "host-direct", "ask you"} {
		if !strings.Contains(copy, want) {
			t.Fatalf("auto-mode notice omitted %q:\n%s", want, copy)
		}
	}

	cmdMode(m, "")
	picker, ok := m.dlg.(*pickerDialog)
	if !ok {
		t.Fatalf("bare /mode dialog = %T", m.dlg)
	}
	for _, item := range picker.items {
		if item.id == string(permission.ModeAuto) {
			if !strings.Contains(item.desc, "confined commands") || !strings.Contains(item.desc, "host-direct commands ask you") {
				t.Fatalf("auto-mode picker description = %q", item.desc)
			}
			return
		}
	}
	t.Fatal("mode picker omitted auto")
}

func TestModeCycleWorksWhileTurnIsBusy(t *testing.T) {
	m := testModel(t)
	m.busy = true
	// The engine publishes mode and reach under one lock, and every later
	// tool call in the turn is checked against the new mode — clamping a
	// wandering turn to plan without killing it is the point.
	m.key(tea.KeyMsg{Type: tea.KeyShiftTab})
	if m.mode != permission.ModeAcceptEdits || m.app.loop.Perms.Mode() != permission.ModeAcceptEdits {
		t.Fatalf("mid-turn shift+tab: UI=%s engine=%s, want acceptEdits", m.mode, m.app.loop.Perms.Mode())
	}
}

func testTranscript(t *testing.T, width int) *transcript {
	t.Helper()
	return newTranscript(width, darkTheme(), newMarkdown(width, true))
}

func TestTranscriptRendersAndScrolls(t *testing.T) {
	tr := testTranscript(t, 80)
	tr.add(&entry{kind: kindUser, text: "hello"})
	e := tr.add(&entry{kind: kindAssistant, text: "", live: true})
	tr.appendText(tr.indexOf(e), "world")
	tr.finalize(e)

	if len(tr.flat) == 0 {
		t.Fatal("nothing rendered")
	}
	joined := strings.Join(tr.flat, "\n")
	if !strings.Contains(joined, "hello") || !strings.Contains(joined, "world") {
		t.Fatalf("transcript lost content:\n%s", joined)
	}

	for i := 0; i < 50; i++ {
		tr.add(&entry{kind: kindInfo, text: "line"})
	}
	view := tr.view(10)
	if got := strings.Count(view, "\n") + 1; got != 10 {
		t.Fatalf("view height = %d, want 10", got)
	}
	tr.scrollBy(5)
	if tr.offset != 5 {
		t.Fatalf("offset = %d, want 5", tr.offset)
	}
	tr.scrollBy(-100)
	if tr.offset != 0 {
		t.Fatalf("offset below bottom = %d", tr.offset)
	}
}

func TestCompletedEntriesRenderOncePerWidth(t *testing.T) {
	tr := testTranscript(t, 80)
	e := tr.add(&entry{kind: kindAssistant, text: "some **bold** text"})
	if _, ok := e.cache[80]; !ok {
		t.Fatal("completed entry was not cached at its width")
	}
	// A width change re-renders, but returning to a seen width is a cache hit.
	tr.setWidth(40)
	tr.setWidth(80)
	if _, ok := e.cache[80]; !ok {
		t.Fatal("per-width cache did not survive a resize round trip")
	}
}

func TestStreamingEntryNeverTouchesTheCache(t *testing.T) {
	tr := testTranscript(t, 80)
	e := tr.add(&entry{kind: kindAssistant, live: true})
	tr.appendText(tr.indexOf(e), "in flight")
	if len(e.cache) != 0 {
		t.Fatal("a streaming entry was cached; the fast path is not the fast path")
	}
}

func TestToolEntryCollapseAndExpand(t *testing.T) {
	tr := testTranscript(t, 80)
	e := tr.add(&entry{kind: kindTool, tool: toolEntry{name: "exec", desc: "go test ./..."}})
	if got := tr.render(e); len(got) != 1 {
		t.Fatalf("running tool rendered %d lines, want 1", len(got))
	}
	e.tool.done = true
	e.tool.detail = "ok  github.com/example/pkg"
	e.cache = nil
	if got := tr.render(e); len(got) != 2 {
		t.Fatalf("collapsed tool rendered %d lines, want 2:\n%s", len(got), strings.Join(got, "\n"))
	}
	e.tool.detail = strings.Repeat("line\n", 100)
	e.expanded = true
	e.cache = nil
	if got := tr.render(e); len(got) <= 2 {
		t.Fatal("expanded tool did not show its detail")
	}
}

func TestConcurrentSameNameToolEndsPairByCallID(t *testing.T) {
	m := testModel(t)
	m.onToolStart(toolStartMsg{id: "read-a", name: "read", req: permission.Request{
		Tool: "read", Path: "a.go",
	}})
	m.onToolStart(toolStartMsg{id: "read-b", name: "read", req: permission.Request{
		Tool: "read", Path: "b.go",
	}})

	m.onToolEnd(toolEndMsg{id: "read-a", name: "read", res: tools.Result{
		Content: "failure-a", IsError: true,
	}, took: time.Second})
	m.onToolEnd(toolEndMsg{id: "read-b", name: "read", res: tools.Result{
		Content: "success-b",
	}, took: 2 * time.Second})

	byID := map[string]toolEntry{}
	for _, entry := range m.tr.entries {
		if entry.kind == kindTool {
			byID[entry.tool.id] = entry.tool
		}
	}
	a, b := byID["read-a"], byID["read-b"]
	if !a.done || !a.failed || a.detail != "failure-a" || a.took != time.Second || !strings.Contains(a.desc, "a.go") {
		t.Fatalf("call A was mispaired: %+v", a)
	}
	if !b.done || b.failed || b.detail != "success-b" || b.took != 2*time.Second || !strings.Contains(b.desc, "b.go") {
		t.Fatalf("call B was mispaired: %+v", b)
	}
}

func TestRouteEntryCollapsesToOneLine(t *testing.T) {
	tr := testTranscript(t, 80)
	e := tr.add(&entry{kind: kindRoute, routeSummary: "t2 via heuristic (test)", routeLines: []string{"route t2", "estimate $0.01"}})
	// A one-line entry earns no gap: it packs tight with its neighbors.
	if got := tr.render(e); len(got) != 1 {
		t.Fatalf("collapsed route rendered %d lines, want exactly the summary: %q", len(got), got)
	}
	e.expanded = true
	e.cache = nil
	// Expanded it is multi-line, so the breathing line follows.
	got := tr.render(e)
	if len(got) <= 2 || got[len(got)-1] != "" {
		t.Fatalf("expanded route should show the record and end with a gap: %q", got)
	}
}

func TestMatchingCommandsIncludesTiers(t *testing.T) {
	cfg := &config.Config{Tiers: []config.Tier{{ID: "t9", Label: "deep"}}}
	matches := matchingCommands("t", cfg)
	found := false
	for _, m := range matches {
		if m.name == "t9" {
			found = true
		}
	}
	if !found {
		t.Fatal("tier entries missing from suggestions")
	}
	matches = matchingCommands("exi", cfg)
	if len(matches) != 1 || matches[0].name != "exit" {
		t.Fatalf("prefix matching is off: %+v", matches)
	}
}

func TestNewerVersion(t *testing.T) {
	cases := []struct {
		candidate, current string
		want               bool
	}{
		{"v0.3.0", "v0.2.9", true},
		{"0.2.9", "v0.2.9", false},
		{"v1.0.0", "v0.99.99", true},
		{"v0.2.9-rc1", "v0.2.9", false},
		{"garbage", "v0.2.9", false},
		// The beta channel's whole path: beta.1 → beta.2 → release, each a
		// real upgrade, none repeating.
		{"v0.4.0-beta.2", "v0.4.0-beta.1", true},
		{"v0.4.0", "v0.4.0-beta.2", true},
		{"v0.4.0-beta.1", "v0.4.0-beta.1", false},
		{"v0.4.0-beta.1", "v0.4.0", false},
		{"v0.4.0-beta.10", "v0.4.0-beta.9", true},
		{"v0.4.0-rc.1", "v0.4.0-beta.2", true},
	}
	for _, c := range cases {
		if got := newerVersion(c.candidate, c.current); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", c.candidate, c.current, got, c.want)
		}
	}
}

func TestChecksumFor(t *testing.T) {
	darwinSum := strings.Repeat("a", 64)
	linuxSum := strings.Repeat("B", 64)
	sums := []byte(darwinSum + "  sb_0.3.0_darwin_arm64.tar.gz\n" + linuxSum + " *sb_0.3.0_linux_amd64.tar.gz\n")
	got, err := checksumFor(sums, "sb_0.3.0_darwin_arm64.tar.gz")
	if err != nil || got != darwinSum {
		t.Fatalf("got %q, %v", got, err)
	}
	got, err = checksumFor(sums, "sb_0.3.0_linux_amd64.tar.gz")
	if err != nil || got != strings.ToLower(linuxSum) {
		t.Fatalf("binary-mode entry: got %q, %v", got, err)
	}
	if _, err := checksumFor(sums, "missing"); err == nil {
		t.Fatal("a missing asset must be an error, not an empty checksum")
	}
}

func TestCompact(t *testing.T) {
	if got := compact(999); got != "999" {
		t.Errorf("compact(999) = %s", got)
	}
	if got := compact(1500); got != "1.5k" {
		t.Errorf("compact(1500) = %s", got)
	}
	if got := compact(2_500_000); got != "2.5M" {
		t.Errorf("compact(2.5M) = %s", got)
	}
}

// The §14 claim behind these is that view cost tracks the viewport, not the
// session: a 500-turn transcript renders no slower than a 50-turn one once
// completed entries are cached. Run both and compare.
func benchTranscript(b *testing.B, turns int) {
	tr := newTranscript(100, darkTheme(), newMarkdown(100, true))
	for i := 0; i < turns; i++ {
		tr.add(&entry{kind: kindUser, text: "a question that fills a line or two of the terminal"})
		tr.add(&entry{kind: kindAssistant, text: "an answer with some **markdown** and\n\n```go\ncode := true\n```\n"})
	}
	b.ResetTimer()
	for b.Loop() {
		tr.view(40)
	}
}

func BenchmarkTranscriptView50Turns(b *testing.B)  { benchTranscript(b, 50) }
func BenchmarkTranscriptView500Turns(b *testing.B) { benchTranscript(b, 500) }
