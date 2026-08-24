package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
	route "github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/session"
)

func reopenREPLBinding(t *testing.T, r *repl) session.RuntimeBinding {
	t.Helper()
	sess := r.loop.Session
	id, path := sess.ID(), sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Dir(filepath.Dir(path)))
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	return reopened.State().RuntimeBinding
}

func TestBareTierPinSurvivesReopen(t *testing.T) {
	r, _, _ := newOverrideREPL(t, "small", "large")
	r.command(context.Background(), "/t2")
	got := reopenREPLBinding(t, r)
	if got.Tier != "t2" || got.Target != r.config.Tiers[1].Target.ID() || !got.Pinned {
		t.Fatalf("reopened bare tier pin = %+v", got)
	}
}

func TestTierAutoSurvivesReopenAsUnpinned(t *testing.T) {
	r, _, _ := newOverrideREPL(t, "small", "large")
	r.command(context.Background(), "/t2")
	r.command(context.Background(), "/tier auto")
	got := reopenREPLBinding(t, r)
	if got.Tier != "t2" || got.Target != r.config.Tiers[1].Target.ID() || got.Pinned {
		t.Fatalf("reopened automatic posture = %+v", got)
	}
}

func TestCommittedAutomaticMoveSurvivesReopen(t *testing.T) {
	r, _, _ := newOverrideREPL(t, "small", "large")
	if err := persistRuntimeBinding(r.loop.Session, r.tier, false); err != nil {
		t.Fatal(err)
	}
	r.sticky = route.NewSticky(route.Policy{MinimumDwell: 1}, 0)
	r.sticky.Observe(route.RepeatedToolCall)
	r.sticky.CallServed()
	move := r.sticky.Assess(1)
	bind, after, ok := r.moveTo(context.Background(), 1, move.Rationale)
	if !ok || !r.sticky.ApplyChecked(move, bind) {
		t.Fatal("automatic move did not commit")
	}
	if after != nil {
		after()
	}
	got := reopenREPLBinding(t, r)
	if got.Tier != "t2" || got.Target != r.config.Tiers[1].Target.ID() || got.Pinned {
		t.Fatalf("reopened automatic move = %+v", got)
	}
}

func TestTemporaryTierTurnRetainsOriginalBindingOnReopen(t *testing.T) {
	r, _, _ := newOverrideREPL(t, "small", "large")
	want := session.RuntimeBinding{Tier: "t1", Target: r.config.Tiers[0].Target.ID()}
	if err := r.loop.Session.AppendRuntimeBinding(want.Tier, want.Target, want.Pinned); err != nil {
		t.Fatal(err)
	}
	if done := r.command(context.Background(), "/t2 one borrowed turn"); done {
		t.Fatal("temporary turn exited the REPL")
	}
	if got := reopenREPLBinding(t, r); got != want {
		t.Fatalf("temporary override persisted borrowed state: got %+v want %+v", got, want)
	}
}

func TestThinkRemainsProcessOnly(t *testing.T) {
	m := testModel(t)
	m.app.caches = newCacheSet(m.app.tier.Target, m.app.loop.Binding().Cache)
	want := session.RuntimeBinding{Tier: m.app.tier.ID, Target: m.app.tier.Target.ID(), Pinned: true}
	if err := m.app.loop.Session.AppendRuntimeBinding(want.Tier, want.Target, want.Pinned); err != nil {
		t.Fatal(err)
	}
	m.applyThink("high")
	if got := m.app.loop.Session.State().RuntimeBinding; got != want {
		t.Fatalf("/think changed durable binding: got %+v want %+v", got, want)
	}
	if m.app.loop.Binding().Target.ID() == want.Target {
		t.Fatal("/think test did not change the live process binding")
	}
}

func TestFailedAutomaticMoveWALLeavesRankAndBindingUntouched(t *testing.T) {
	r, _, _ := newOverrideREPL(t, "small", "large")
	r.sticky = route.NewSticky(route.Policy{MinimumDwell: 1}, 0)
	r.sticky.Observe(route.RepeatedToolCall)
	r.sticky.CallServed()
	move := r.sticky.Assess(1)
	before := r.loop.Binding().Target.ID()
	bind, _, ok := r.moveTo(context.Background(), 1, move.Rationale)
	if !ok {
		t.Fatal("destination preparation failed before WAL failure seam")
	}
	if err := r.loop.Session.Close(); err != nil {
		t.Fatal(err)
	}
	if r.sticky.ApplyChecked(move, bind) {
		t.Fatal("move committed despite a failed runtime-binding append")
	}
	if r.sticky.Rank() != 0 || r.loop.Binding().Target.ID() != before {
		t.Fatalf("failed WAL changed runtime: rank=%d target=%s", r.sticky.Rank(), r.loop.Binding().Target.ID())
	}
}

func TestDerivedSessionSwapKeepsThinkOutOfRuntimeBinding(t *testing.T) {
	m := testModel(t)
	m.app.caches = newCacheSet(m.app.tier.Target, m.app.loop.Binding().Cache)
	want := session.RuntimeBinding{Tier: m.app.tier.ID, Target: m.app.tier.Target.ID(), Pinned: true}
	if err := m.app.loop.Session.AppendRuntimeBinding(want.Tier, want.Target, want.Pinned); err != nil {
		t.Fatal(err)
	}
	m.applyThink("high")
	live := m.app.tier
	replacement, err := m.app.store.Create(m.app.workspace, live.Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	m.onSessionSwap(sessionSwapMsg{sess: replacement, tier: live, client: m.app.loop.Binding().Provider,
		fresh: true, preserveRuntimeTarget: true})
	if got := replacement.State().RuntimeBinding; got != want {
		t.Fatalf("derived session persisted process-only think/posture: got %+v want %+v", got, want)
	}
	if m.app.loop.Binding().Target.ID() != live.Target.ID() {
		t.Fatal("derived session did not keep the live process-only think binding")
	}
	if m.app.sticky == nil || !m.app.sticky.Pinned() {
		t.Fatal("derived session dropped the durable pin posture")
	}
}

func prepareMovedSecondTurn(t *testing.T, m *tuiModel) config.Tier {
	t.Helper()
	first := m.app.tier
	second := config.Tier{ID: "t2", Label: "strong", Target: provider.RouteTarget{
		Provider: "ollama", Surface: "local", ModelID: "strong:14b",
	}}
	m.app.config.Tiers = append(m.app.config.Tiers, second)
	if err := m.app.loop.Session.AppendRuntimeBinding(first.ID, first.Target.ID(), false); err != nil {
		t.Fatal(err)
	}
	appendTurn(t, m, "first question", "first answer")
	if err := m.app.loop.Session.AppendMessage(provider.UserText("second question")); err != nil {
		t.Fatal(err)
	}
	m.app.bind(second, m.app.loop.Binding().Provider, true)
	if err := persistRuntimeBinding(m.app.loop.Session, second, true); err != nil {
		t.Fatal(err)
	}
	if err := m.app.loop.Session.AppendMessage(provider.Message{Role: provider.RoleAssistant,
		Content: []provider.Block{provider.Text{Text: "second answer"}}}); err != nil {
		t.Fatal(err)
	}
	return second
}

func assertLiveAndReopenedBinding(t *testing.T, m *tuiModel, want session.RuntimeBinding) {
	t.Helper()
	if got := m.app.loop.Session.State().RuntimeBinding; got != want {
		t.Fatalf("live session runtime binding = %+v, want %+v", got, want)
	}
	if got := m.app.tier.ID; got != want.Tier {
		t.Fatalf("live tier = %s, want %s", got, want.Tier)
	}
	if got := m.app.loop.Binding().Target.ID(); got != want.Target {
		t.Fatalf("live target = %s, want %s", got, want.Target)
	}
	if got := m.app.sticky != nil && m.app.sticky.Pinned(); got != want.Pinned {
		t.Fatalf("live pinned = %v, want %v", got, want.Pinned)
	}

	id := m.app.loop.Session.ID()
	if err := m.app.loop.Session.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := m.app.store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.State().RuntimeBinding; got != want {
		t.Fatalf("reopened runtime binding = %+v, want %+v", got, want)
	}
}

func TestEarlierCutForkKeepsCurrentDurableBindingLiveAndOnReopen(t *testing.T) {
	m := testModel(t)
	second := prepareMovedSecondTurn(t, m)
	source := m.app.loop.Session

	forked, err := m.app.store.ForkSession(source, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := forked.State().RuntimeBinding; got.Tier != "t1" {
		forked.Close()
		t.Fatalf("test setup did not cut away the later move: %+v", got)
	}
	m.onSessionSwap(sessionSwapMsg{sess: forked, tier: second, client: m.app.loop.Binding().Provider,
		preserveRuntimeTarget: true})

	assertLiveAndReopenedBinding(t, m, session.RuntimeBinding{
		Tier: second.ID, Target: second.Target.ID(), Pinned: true,
	})
}

func TestRetryAfterMovedTurnKeepsCurrentDurableBindingLiveAndOnReopen(t *testing.T) {
	m := testModel(t)
	second := prepareMovedSecondTurn(t, m)

	swap, ok := cmdRetry(m, "")().(sessionSwapMsg)
	if !ok || swap.err != nil {
		t.Fatalf("retry did not produce a session swap: %#v", swap)
	}
	if got := swap.sess.State().RuntimeBinding; got.Tier != "t1" {
		swap.sess.CloseDiscardingStaged()
		t.Fatalf("test setup did not cut away the move made in the retried turn: %+v", got)
	}
	continuation := m.onSessionSwap(swap)
	if continuation == nil {
		t.Fatal("retry swap lost its replay continuation")
	}
	m.interrupt()

	assertLiveAndReopenedBinding(t, m, session.RuntimeBinding{
		Tier: second.ID, Target: second.Target.ID(), Pinned: true,
	})
}

func TestTierForSessionStateUsesDurableTierAndExactTarget(t *testing.T) {
	shared := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "shared"}
	fallback := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "fallback"}
	cfg := &config.Config{Tiers: []config.Tier{
		{ID: "t1", Target: shared, Fallbacks: []provider.RouteTarget{fallback}},
		{ID: "t2", Target: shared},
	}}

	got, configured, err := tierForSessionState(cfg, session.State{RuntimeBinding: session.RuntimeBinding{
		Tier: "t2", Target: shared.ID(), Pinned: true,
	}})
	if err != nil || !configured || got.ID != "t2" || got.Target.ID() != shared.ID() {
		t.Fatalf("shared target resumed as %+v configured=%v err=%v", got, configured, err)
	}
	got, configured, err = tierForSessionState(cfg, session.State{RuntimeBinding: session.RuntimeBinding{
		Tier: "t1", Target: fallback.ID(),
	}})
	if err != nil || !configured || got.ID != "t1" || got.Target.ID() != fallback.ID() {
		t.Fatalf("fallback resumed as %+v configured=%v err=%v", got, configured, err)
	}
	got, configured, err = tierForSessionState(cfg, session.State{RuntimeBinding: session.RuntimeBinding{
		Tier: "removed", Target: fallback.ID(),
	}})
	if err != nil || configured || got.ID != "removed" || got.Target.ID() != fallback.ID() {
		t.Fatalf("removed tier resumed as %+v configured=%v err=%v", got, configured, err)
	}
}

func TestTierForSessionStateDoesNotBorrowFallbacksAcrossRungCapMigration(t *testing.T) {
	for _, test := range []struct {
		name                     string
		configuredCap, recordCap int
	}{
		{name: "cap added after session", configuredCap: 4096},
		{name: "cap removed after session", recordCap: 4096},
		{name: "cap changed after session", configuredCap: 8192, recordCap: 4096},
	} {
		t.Run(test.name, func(t *testing.T) {
			configuredTarget := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "same"}
			configuredTarget.Params.MaxOutputTokens = test.configuredCap
			fallback := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "backup"}
			fallback.Params.MaxOutputTokens = test.configuredCap
			recordedTarget := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "same"}
			recordedTarget.Params.MaxOutputTokens = test.recordCap
			cfg := &config.Config{Tiers: []config.Tier{{
				ID: "t1", Label: "light", Target: configuredTarget, Fallbacks: []provider.RouteTarget{fallback},
			}}}

			got, configured, err := tierForSessionState(cfg, session.State{RuntimeBinding: session.RuntimeBinding{
				Tier: "t1", Target: recordedTarget.ID(), Pinned: true,
			}})
			if err != nil || !configured {
				t.Fatalf("migrated binding = %+v configured=%v err=%v", got, configured, err)
			}
			if got.ID != "t1" || got.Target.ID() != recordedTarget.ID() || len(got.Fallbacks) != 0 {
				t.Fatalf("migrated binding borrowed current rung policy: %+v", got)
			}
		})
	}
}

func TestTUIManualPinSurvivesReopen(t *testing.T) {
	m := testModel(t)
	second := config.Tier{ID: "t2", Target: provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "strong"}}
	m.app.config.Tiers = append(m.app.config.Tiers, second)
	_, operation, sourceID, err := m.startOperation("tier switch")
	if err != nil {
		t.Fatal(err)
	}
	m.onTierSwitch(tierSwitchMsg{tier: second, client: m.app.loop.Binding().Provider, operation: operation, sourceID: sourceID})
	id := m.app.loop.Session.ID()
	if err := m.app.loop.Session.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := m.app.store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.State().RuntimeBinding; got.Tier != "t2" || got.Target != second.Target.ID() || !got.Pinned {
		t.Fatalf("reopened TUI pin = %+v", got)
	}
}

func TestTUITierAutoSurvivesReopen(t *testing.T) {
	m := testModel(t)
	if err := m.app.loop.Session.AppendRuntimeBinding(m.app.tier.ID, m.app.tier.Target.ID(), true); err != nil {
		t.Fatal(err)
	}
	m.app.sticky = route.NewSticky(route.Policy{}, 0)
	m.app.sticky.Pin(0)
	cmdTier(m, "auto")
	id := m.app.loop.Session.ID()
	if err := m.app.loop.Session.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := m.app.store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.State().RuntimeBinding; got.Tier != "t1" || got.Target != m.app.tier.Target.ID() || got.Pinned {
		t.Fatalf("reopened TUI automatic posture = %+v", got)
	}
}
