package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
	route "github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/session"
)

type fallbackOrderingProvider struct {
	inspect  func() bool
	observed chan bool
	calls    atomic.Int32
}

func (p *fallbackOrderingProvider) Name() string { return "fallback-ordering" }

func (p *fallbackOrderingProvider) Stream(context.Context, provider.RouteTarget, provider.Request) (provider.EventStream, error) {
	p.calls.Add(1)
	if p.observed != nil {
		ok := true
		if p.inspect != nil {
			ok = p.inspect()
		}
		p.observed <- ok
	}
	return &racedStream{events: racedText("done").events}, nil
}

func (*fallbackOrderingProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}

func (*fallbackOrderingProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true}, nil
}

func timelineNoteCount(t *testing.T, path, text string) int {
	t.Helper()
	timeline, err := session.ReadTimeline(path)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, item := range timeline {
		if item.Note != nil && item.Note.Text == text {
			count++
		}
	}
	return count
}

func timelineNoteContainingCount(t *testing.T, path, text string) int {
	t.Helper()
	timeline, err := session.ReadTimeline(path)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, item := range timeline {
		if item.Note != nil && strings.Contains(item.Note.Text, text) {
			count++
		}
	}
	return count
}

func TestTUIOpeningFallbackIsDurableAndVisibleBeforeProviderCall(t *testing.T) {
	m := testModel(t)
	const note = "t1 is served by its fallback ollama/local/backup: primary unavailable"
	providerCalled := make(chan bool, 1)
	client := &fallbackOrderingProvider{
		observed: providerCalled,
		inspect: func() bool {
			return strings.Contains(strings.Join(m.tr.flat, "\n"), note)
		},
	}
	opening, err := stampTurnOpening(m.app.loop.Session, turnOpening("inspect this", nil))
	if err != nil {
		t.Fatal(err)
	}
	_, generation := m.startPlanning()
	m.onTurnPlan(turnPlanMsg{
		generation: generation,
		opening:    opening,
		tier:       m.app.tier,
		client:     client,
		note:       note,
	})

	select {
	case visible := <-providerCalled:
		if !visible {
			t.Fatal("provider call began before the fallback note reached the transcript")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fallback turn did not reach the provider")
	}
	owner := m.primaryTurn
	if owner == nil {
		t.Fatal("fallback turn did not acquire a primary-turn owner")
	}
	select {
	case <-owner.done:
	case <-time.After(2 * time.Second):
		t.Fatal("fallback turn did not finish")
	}
	if got := timelineNoteCount(t, m.app.loop.Session.Path(), note); got != 1 {
		t.Fatalf("fallback timeline note count = %d, want one", got)
	}
	if got := m.app.loop.Session.State().RuntimeBinding.Target; got != m.app.tier.Target.ID() {
		t.Fatalf("runtime target = %s, want %s", got, m.app.tier.Target.ID())
	}
}

func TestTUIOpeningFallbackAppendFailureSendsNothing(t *testing.T) {
	m := testModel(t)
	beforeTier, beforeBinding := m.app.tier, m.app.loop.Binding()
	path := m.app.loop.Session.Path()
	if err := m.app.loop.Session.Close(); err != nil {
		t.Fatal(err)
	}
	client := &fallbackOrderingProvider{}
	_, generation := m.startPlanning()
	m.onTurnPlan(turnPlanMsg{
		generation: generation,
		opening:    provider.UserText("must not leave"),
		tier:       m.app.tier,
		client:     client,
		note:       "unrecordable opening fallback",
	})
	if got := client.calls.Load(); got != 0 {
		t.Fatalf("provider calls = %d, want zero", got)
	}
	if m.primaryTurn != nil || m.app.tier.ID != beforeTier.ID || m.app.loop.Binding().Target.ID() != beforeBinding.Target.ID() {
		t.Fatalf("failed note append changed or launched runtime: owner=%v tier=%s target=%s",
			m.primaryTurn != nil, m.app.tier.ID, m.app.loop.Binding().Target.ID())
	}
	if got := timelineNoteCount(t, path, "unrecordable opening fallback"); got != 0 {
		t.Fatalf("failed append left %d fallback notes", got)
	}
}

func TestTUIOneTurnFallbackAppendFailureSendsNothing(t *testing.T) {
	m := testModel(t)
	destination := config.Tier{ID: "t2", Target: provider.RouteTarget{
		Provider: "ollama", Surface: "local", ModelID: "backup",
	}}
	m.app.config.Tiers = append(m.app.config.Tiers, destination)
	beforeTier, beforeBinding := m.app.tier, m.app.loop.Binding()
	if err := m.app.loop.Session.Close(); err != nil {
		t.Fatal(err)
	}
	client := &fallbackOrderingProvider{}
	_, generation := m.startPlanning()
	m.onOverrideProbe(overrideProbeMsg{
		generation: generation,
		opening:    provider.UserText("must not leave"),
		tier:       destination,
		client:     client,
		note:       "unrecordable one-turn fallback",
	})
	if got := client.calls.Load(); got != 0 {
		t.Fatalf("provider calls = %d, want zero", got)
	}
	if m.restoreTier != nil || m.primaryTurn != nil || m.app.tier.ID != beforeTier.ID ||
		m.app.loop.Binding().Target.ID() != beforeBinding.Target.ID() {
		t.Fatalf("failed temporary note append changed runtime: restore=%v owner=%v tier=%s target=%s",
			m.restoreTier != nil, m.primaryTurn != nil, m.app.tier.ID, m.app.loop.Binding().Target.ID())
	}
}

func TestTUITierSwitchFallbackAppendFailureLeavesBindingUntouched(t *testing.T) {
	m := testModel(t)
	destination := config.Tier{ID: "t2", Target: provider.RouteTarget{
		Provider: "ollama", Surface: "local", ModelID: "backup",
	}}
	m.app.config.Tiers = append(m.app.config.Tiers, destination)
	before := m.app.loop.Binding()
	_, operation, sourceID, err := m.startOperation("tier switch")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.app.loop.Session.Close(); err != nil {
		t.Fatal(err)
	}
	m.onTierSwitch(tierSwitchMsg{
		tier: destination, client: &fallbackOrderingProvider{}, note: "unrecordable tier fallback",
		operation: operation, sourceID: sourceID,
	})
	if m.app.tier.ID != "t1" || m.app.loop.Binding().Target.ID() != before.Target.ID() || m.operationActive {
		t.Fatalf("failed switch changed runtime: tier=%s target=%s operation=%v",
			m.app.tier.ID, m.app.loop.Binding().Target.ID(), m.operationActive)
	}
}

func TestSessionSwapFallbackAppendFailureDoesNotAdopt(t *testing.T) {
	m := testModel(t)
	source := m.app.loop.Session
	child, err := m.app.store.CreateStaged(m.app.workspace, m.app.tier.Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	m.onSessionSwap(sessionSwapMsg{
		sess: child, tier: m.app.tier, client: m.app.loop.Binding().Provider,
		note: "unrecordable resumed fallback", warnNote: true,
	})
	if m.app.loop.Session != source || m.quitting {
		t.Fatalf("failed resume note append adopted or stopped the app: source=%v quitting=%v",
			m.app.loop.Session == source, m.quitting)
	}
}

func TestREPLTierOverrideFallbackRendersBeforeSendAndSurvivesTimeline(t *testing.T) {
	r, capture, output := newOverrideREPL(t, "small", "backup")
	const fragment = "t2 is served by its fallback"
	seen := make(chan bool, 1)
	capture.mu.Lock()
	capture.onChat = func() { seen <- strings.Contains(output(), fragment) }
	capture.mu.Unlock()
	r.command(context.Background(), "/t2 use the fallback")
	select {
	case visible := <-seen:
		if !visible {
			t.Fatal("REPL provider call began before its fallback warning was rendered")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("REPL fallback request was not sent")
	}
	if got := timelineNoteContainingCount(t, r.loop.Session.Path(), fragment); got != 1 {
		t.Fatalf("REPL temporary fallback timeline count = %d, want one", got)
	}
}

func TestREPLTierOverrideFallbackAppendFailureSendsNothing(t *testing.T) {
	r, capture, output := newOverrideREPL(t, "small", "backup")
	path := r.loop.Session.Path()
	if err := r.loop.Session.Close(); err != nil {
		t.Fatal(err)
	}
	r.command(context.Background(), "/t2 use the fallback")
	if len(capture.bodies) != 0 {
		t.Fatalf("provider requests = %d, want zero", len(capture.bodies))
	}
	if !strings.Contains(output(), "fallback substitution was not recorded") {
		t.Fatalf("append refusal was not actionable:\n%s", output())
	}
	if got := timelineNoteContainingCount(t, path, "t2 is served by its fallback"); got != 0 {
		t.Fatalf("failed REPL append left %d fallback notes", got)
	}
}

func TestREPLAutomaticFallbackAppendFailureDoesNotRebind(t *testing.T) {
	r, _, _ := newOverrideREPL(t, "small", "backup")
	beforeTier, beforeBinding := r.tier, r.loop.Binding()
	beforeRuntime := r.loop.Session.State().RuntimeBinding
	if err := r.loop.Session.Close(); err != nil {
		t.Fatal(err)
	}
	destination := r.config.Tiers[1]
	destination.Target = destination.Fallbacks[0]
	err := r.acceptTurnResolution(context.Background(), destination, &fallbackOrderingProvider{},
		"unrecordable automatic fallback", turnPlan{})
	if err == nil || !strings.Contains(err.Error(), "saving automatic tier selection") {
		t.Fatalf("automatic fallback append error = %v", err)
	}
	if r.tier.ID != beforeTier.ID || r.loop.Binding().Target.ID() != beforeBinding.Target.ID() ||
		r.loop.Session.State().RuntimeBinding != beforeRuntime {
		t.Fatalf("failed automatic note changed runtime: tier=%s target=%s binding=%+v",
			r.tier.ID, r.loop.Binding().Target.ID(), r.loop.Session.State().RuntimeBinding)
	}
}

func TestREPLMidturnFallbackAppendFailureLeavesStickyMoveUncommitted(t *testing.T) {
	r, _, output := newOverrideREPL(t, "small", "backup")
	r.sticky = route.NewSticky(route.Policy{MinimumDwell: 1}, 0)
	r.sticky.Observe(route.RepeatedToolCall)
	r.sticky.CallServed()
	move := r.sticky.Assess(1)
	bind, after, ok := r.moveTo(context.Background(), 1, move.Rationale)
	if !ok || bind == nil {
		t.Fatal("fallback move was not prepared")
	}
	path := r.loop.Session.Path()
	if err := r.loop.Session.Close(); err != nil {
		t.Fatal(err)
	}
	if r.sticky.ApplyChecked(move, bind) {
		t.Fatal("fallback move committed after its atomic note append failed")
	}
	if after != nil && strings.Contains(output(), "served by its fallback") {
		t.Fatal("failed fallback move was rendered as committed")
	}
	if r.sticky.Rank() != 0 || r.tier.ID != "t1" {
		t.Fatalf("failed move changed sticky/runtime tier: rank=%d tier=%s", r.sticky.Rank(), r.tier.ID)
	}
	if got := timelineNoteContainingCount(t, path, "served by its fallback"); got != 0 {
		t.Fatalf("failed midturn append left %d fallback notes", got)
	}
}

func TestRaceFallbackBindingsAreRecordedBeforeEitherArmCanRun(t *testing.T) {
	m := raceModel(t)
	pa, pb := &racedProvider{turns: []racedTurn{racedText("a")}}, &racedProvider{turns: []racedTurn{racedText("b")}}
	armA, err := assembleRaceArm(m.app, m.app.config.Tiers[0], pa, nil)
	if err != nil {
		t.Fatal(err)
	}
	armB, err := assembleRaceArm(m.app, m.app.config.Tiers[1], pb, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = armA.sess.CloseDiscardingStaged()
		_ = armB.sess.CloseDiscardingStaged()
	})
	notes := [2]string{"arm a fallback", "arm b fallback"}
	if err := recordRaceFallbackBindings([2]*raceArm{armA, armB}, notes); err != nil {
		t.Fatal(err)
	}
	if pa.calls != 0 || pb.calls != 0 {
		t.Fatalf("recording notes called providers: a=%d b=%d", pa.calls, pb.calls)
	}
	for i, arm := range []*raceArm{armA, armB} {
		if outcome, err := arm.sess.PublishDurably(); err != nil || !outcome.Visible || !outcome.Durable {
			t.Fatalf("publishing arm %d for timeline replay: outcome=%+v err=%v", i, outcome, err)
		}
		if got := timelineNoteCount(t, arm.sess.Path(), notes[i]); got != 1 {
			t.Fatalf("arm %d fallback timeline count = %d, want one", i, got)
		}
	}
}

func TestRaceFallbackAppendFailureSendsNeitherArm(t *testing.T) {
	m := raceModel(t)
	pa, pb := &racedProvider{turns: []racedTurn{racedText("a")}}, &racedProvider{turns: []racedTurn{racedText("b")}}
	armA, err := assembleRaceArm(m.app, m.app.config.Tiers[0], pa, nil)
	if err != nil {
		t.Fatal(err)
	}
	armB, err := assembleRaceArm(m.app, m.app.config.Tiers[1], pb, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := armB.sess.Close(); err != nil {
		t.Fatal(err)
	}
	recordErr := recordRaceFallbackBindings([2]*raceArm{armA, armB}, [2]string{"arm a fallback", "arm b fallback"})
	if recordErr == nil {
		t.Fatal("closed arm accepted its fallback binding note")
	}
	_, operation, sourceID, err := m.startOperation("race setup")
	if err != nil {
		t.Fatal(err)
	}
	m.onRaceSetup(raceSetupMsg{
		operation: operation, sourceID: sourceID, arms: [2]*raceArm{armA, armB}, err: recordErr,
	})
	if pa.calls != 0 || pb.calls != 0 || m.race != nil {
		t.Fatalf("failed race note launched work: a=%d b=%d race=%v", pa.calls, pb.calls, m.race != nil)
	}
}

func TestPersistRuntimeBindingFallbackRefusesInvalidComposite(t *testing.T) {
	if err := persistRuntimeBindingFallback(nil, config.Tier{}, false, "note"); err == nil {
		t.Fatal("nil session accepted a fallback binding")
	}
}
