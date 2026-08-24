package eval

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/costmodel"
	"github.com/switchboard-code/switchboard/internal/prefix"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/provider/anthropic"
	"github.com/switchboard-code/switchboard/internal/router"
)

type captureRouter struct {
	input router.Input
}

func (r *captureRouter) Route(input router.Input) (router.Decision, error) {
	r.input = input
	chosen := input.Candidates[len(input.Candidates)-1]
	return router.Decision{
		Tier: chosen.Tier, Target: chosen.Target.ID(), EstimatedCost: chosen.Estimate,
	}, nil
}

type recordingProvider struct {
	mu       sync.Mutex
	probe    provider.ProbeResult
	probeErr error
	turns    [][]provider.Event
	requests []provider.Request
	probes   int
}

func (p *recordingProvider) Name() string { return "recording" }

func (p *recordingProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probes++
	return p.probe, p.probeErr
}

func (p *recordingProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}

func (p *recordingProvider) Stream(_ context.Context, _ provider.RouteTarget, request provider.Request) (provider.EventStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, request)
	if len(p.turns) == 0 {
		return nil, errors.New("recording provider ran out of turns")
	}
	events := p.turns[0]
	p.turns = p.turns[1:]
	return &recordingStream{events: events}, nil
}

type recordingStream struct {
	events []provider.Event
	index  int
}

func (s *recordingStream) Next() (provider.Event, error) {
	if s.index >= len(s.events) {
		return provider.Event{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (*recordingStream) Close() error { return nil }

func liveProbe() provider.ProbeResult {
	return provider.ProbeResult{Reachable: true, ModelPresent: true, Tools: provider.ToolsParallel}
}

func completedTurn() []provider.Event {
	return []provider.Event{
		{Type: provider.EventTextDelta, Index: 0, Text: "done"},
		{Type: provider.EventDone, StopReason: provider.StopEndTurn, Usage: provider.Usage{InputTokens: 20, OutputTokens: 2}},
	}
}

func readTurn(id string) []provider.Event {
	call := provider.ToolUse{ID: id, Name: "read", Input: json.RawMessage(`{"path":"note.txt"}`)}
	return []provider.Event{
		{Type: provider.EventToolUse, Index: 0, ToolUse: &call},
		{Type: provider.EventDone, StopReason: provider.StopToolUse, Usage: provider.Usage{InputTokens: 20, OutputTokens: 2}},
	}
}

func evalCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

func TestRoutedRunScoresTheExactRequestTheSelectedProviderReceives(t *testing.T) {
	cat := evalCatalog(t)
	selector := &captureRouter{}
	lowProvider := &recordingProvider{probe: liveProbe()}
	highProvider := &recordingProvider{probe: liveProbe(), turns: [][]provider.Event{completedTurn()}}
	arms := []Arm{
		{Name: "low", Target: provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}, Provider: lowProvider},
		{Name: "high", Target: provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"}, Provider: highProvider},
	}
	routed := RoutedArmFor{Catalog: cat, Router: selector, Ladder: arms}
	task := Task{
		ID: "assembled", Provenance: HandWritten, Prompt: "choose deliberately",
		Setup:  func(string) error { return nil },
		Verify: func(context.Context, string) (bool, string, error) { return true, "", nil },
	}

	got := routed.Run(context.Background(), Runner{Catalog: cat}, task, 7)
	if !got.Solved || got.Failure != "" {
		t.Fatalf("run = %#v", got)
	}
	if got.Target != arms[1].Target.ID() || got.EstimatedTarget != got.Target {
		t.Fatalf("targets = actual %q estimated %q, want %q", got.Target, got.EstimatedTarget, arms[1].Target.ID())
	}
	if len(highProvider.requests) != 1 {
		t.Fatalf("selected provider requests = %d, want 1", len(highProvider.requests))
	}
	request := highProvider.requests[0]
	if len(request.System) == 0 || len(request.Tools) == 0 || len(request.Messages) != 1 {
		t.Fatalf("execution request was not fully assembled: %#v", request)
	}
	if request.Messages[0].Text() != task.Prompt {
		t.Fatalf("execution prompt = %q, want %q", request.Messages[0].Text(), task.Prompt)
	}
	wantPrompt := prefix.RequestTokens(request)
	wantContext := prefix.RequestTokenCeiling(request)
	if selector.input.Session.PromptTokens != wantPrompt || selector.input.Session.ContextTokens != wantContext {
		t.Fatalf("session tokens = (%d, %d), want (%d, %d)",
			selector.input.Session.PromptTokens, selector.input.Session.ContextTokens, wantPrompt, wantContext)
	}
	bindings := map[string]provider.Provider{
		"low": lowProvider, "high": highProvider,
	}
	for _, candidate := range selector.input.Candidates {
		if candidate.PromptTokens != wantPrompt || candidate.ContextTokens != wantContext {
			t.Errorf("candidate %s tokens = (%d, %d), want (%d, %d)",
				candidate.Tier, candidate.PromptTokens, candidate.ContextTokens, wantPrompt, wantContext)
		}
		if candidate.ReservedOutputTokens != provider.EffectiveOutputTokenAllowance(
			bindings[candidate.Tier], candidate.Target, candidate.Info.MaxOutput) {
			t.Errorf("candidate %s output reserve = %d", candidate.Tier, candidate.ReservedOutputTokens)
		}
	}
}

func TestCandidatePricesTheConcreteAnthropicOutputAllowance(t *testing.T) {
	cat := evalCatalog(t)
	client := anthropic.New()
	cases := []struct {
		name     string
		model    string
		reason   *provider.Reasoning
		explicit int
		want     int
	}{
		{name: "default", model: "claude-haiku-4-5", want: 8_192},
		{
			name: "adaptive effort has no token budget", model: "claude-opus-5",
			reason: &provider.Reasoning{Enabled: true, Effort: "high"}, want: 8_192,
		},
		{
			name: "token budget raises the wire allowance", model: "claude-haiku-4-5",
			reason: &provider.Reasoning{Enabled: true, Effort: "high"}, want: 16_384 + 8_192,
		},
		{
			name: "explicit custom cap", model: "claude-opus-5",
			reason: &provider.Reasoning{Enabled: true, Effort: "xhigh"}, explicit: 1_234, want: 1_234,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := anthropic.Target(tc.model)
			target.Params.Reasoning = tc.reason
			target.Params.MaxOutputTokens = tc.explicit
			info, _, ok := cat.Lookup(target)
			if !ok {
				t.Fatalf("test target %s is absent from catalog", target.Display())
			}

			const promptTokens, contextTokens = 300, 500
			candidate := candidateForRequest(
				Arm{Name: "only", Target: target, Provider: client},
				0, info, promptTokens, contextTokens)
			if candidate.ReservedOutputTokens != tc.want {
				t.Fatalf("reserved output = %d, want concrete wire allowance %d",
					candidate.ReservedOutputTokens, tc.want)
			}
			wantCost := costmodel.Estimator{}.Turn(costmodel.Inputs{
				Target: target, Info: info, PrefixTokens: contextTokens,
				OutputTokens:   tc.want,
				Eligible:       info.Cache.UsageAccounting == catalog.AccountingSeparate,
				TokensAreExact: true,
			}).High
			if candidate.CeilingCost != wantCost {
				t.Fatalf("ceiling cost = %s, want exact allowance cost %s",
					candidate.CeilingCost, wantCost)
			}
			catalogCost := costmodel.Estimator{}.Turn(costmodel.Inputs{
				Target: target, Info: info, PrefixTokens: contextTokens,
				OutputTokens:   info.MaxOutput,
				Eligible:       info.Cache.UsageAccounting == catalog.AccountingSeparate,
				TokensAreExact: true,
			}).High
			if candidate.CeilingCost >= catalogCost {
				t.Fatalf("ceiling still prices catalog maximum: concrete=%s catalog=%s",
					candidate.CeilingCost, catalogCost)
			}
		})
	}
}

func TestCandidateRejectsAnthropicExplicitCapReasoningConflict(t *testing.T) {
	cat := evalCatalog(t)
	client := anthropic.New()
	target := anthropic.Target("claude-haiku-4-5")
	target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}
	target.Params.MaxOutputTokens = 4096
	info, _, ok := cat.Lookup(target)
	if !ok {
		t.Fatalf("test target %s is absent from catalog", target.Display())
	}
	candidate := candidateForRequest(
		Arm{Name: "only", Target: target, Provider: client}, 0, info, 100, 100)
	if candidate.ReservedOutputTokens != math.MaxInt {
		t.Fatalf("invalid Anthropic parameters reserved %d, want no finite allowance", candidate.ReservedOutputTokens)
	}
	decision, err := (router.Heuristic{}).Route(router.Input{Candidates: []router.Candidate{candidate}, Pin: "only"})
	if err == nil {
		t.Fatal("eval admitted conflicting Anthropic cap and reasoning")
	}
	text := err.Error() + " " + strings.Join(decision.Infeasible, " ")
	for _, want := range []string{"configured max_output 4096", "raise max_output", "lower or disable reasoning"} {
		if !strings.Contains(text, want) {
			t.Fatalf("eval explicit-cap conflict omitted %q: %s", want, text)
		}
	}
}

func TestEvalBudgetPricesTheBoundProviderOutputAllowance(t *testing.T) {
	cat := evalCatalog(t)
	target := anthropic.Target("claude-haiku-4-5")
	target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}
	client := anthropic.New()
	info, _, ok := cat.Lookup(target)
	if !ok {
		t.Fatalf("test target %s is absent from catalog", target.Display())
	}

	const contextTokens = 500
	bound := candidateForRequest(
		Arm{Name: "only", Target: target, Provider: client},
		0, info, contextTokens, contextTokens).CeilingCost
	unbound := candidateForRequest(
		Arm{Name: "only", Target: target},
		0, info, contextTokens, contextTokens).CeilingCost
	if bound >= unbound {
		t.Fatalf("test does not distinguish bound allowance from catalog fallback: bound=%s unbound=%s",
			bound, unbound)
	}

	routed := RoutedArmFor{Catalog: cat, Ladder: []Arm{{
		Name: "only", Target: target, Provider: client,
	}}}
	_, loop := escalationHarness(t, routed, provider.UserText("continue"))
	guard := newEvalBudget(router.Budgets{MaxCost: bound, MaxCostSet: true}, cat, loop)
	if err := guard.before(contextTokens, 1); err != nil {
		t.Fatalf("per-call budget rejected the exact bound-provider allowance: %v", err)
	}
}

func TestUnsafePromptOnlyPickFailsClosed(t *testing.T) {
	_, _, err := (RoutedArmFor{}).Pick(Task{Prompt: "not an assembled request"})
	if !errors.Is(err, errRoutedFidelity) {
		t.Fatalf("Pick error = %v, want routed fidelity refusal", err)
	}
}

func TestRoutedPickUsesFirstLiveFeasibleFallback(t *testing.T) {
	cat := evalCatalog(t)
	primary := &recordingProvider{probe: provider.ProbeResult{Detail: "offline"}}
	fallback := &recordingProvider{probe: liveProbe()}
	primaryTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}
	fallbackTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"}
	primaryTarget.Params.MaxOutputTokens = 100
	fallbackTarget.Params.MaxOutputTokens = 100
	routed := RoutedArmFor{Catalog: cat, Ladder: []Arm{{
		Name: "low", Target: primaryTarget, Provider: primary,
		Fallbacks: []Fallback{{Target: fallbackTarget, Provider: fallback}},
	}}}
	request := provider.Request{Messages: []provider.Message{provider.UserText("small fix")}}

	arm, decision, err := routed.PickRequest(context.Background(), Task{Prompt: "small fix"}, request)
	if err != nil {
		t.Fatal(err)
	}
	if arm.Target.ID() != fallbackTarget.ID() || decision.Target != fallbackTarget.ID() {
		t.Fatalf("fallback was not bound: arm=%s decision=%s", arm.Target.ID(), decision.Target)
	}
	if primary.probes != 1 || fallback.probes != 1 {
		t.Fatalf("primary/fallback probes = %d/%d, want one outage probe followed by one fallback probe",
			primary.probes, fallback.probes)
	}
}

func TestRoutedPickDoesNotUseFallbackAfterReachablePrimaryHardRefusal(t *testing.T) {
	cat := evalCatalog(t)
	primaryProbe := liveProbe()
	primaryProbe.VisionKnown = true
	primaryProbe.Vision = false
	primary := &recordingProvider{probe: primaryProbe}
	fallback := &recordingProvider{probe: liveProbe()}
	primaryTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}
	fallbackTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"}
	routed := RoutedArmFor{Catalog: cat, Ladder: []Arm{{
		Name: "only", Target: primaryTarget, Provider: primary,
		Fallbacks: []Fallback{{Target: fallbackTarget, Provider: fallback}},
	}}}
	request := provider.Request{Messages: []provider.Message{{
		Role: provider.RoleUser,
		Content: []provider.Block{
			provider.Text{Text: "inspect"},
			provider.Image{MediaType: "image/png", Data: []byte("fixture")},
		},
	}}}

	_, _, err := routed.PickRequest(context.Background(), Task{Prompt: "inspect"}, request)
	if !errors.Is(err, errRoutedFidelity) || !strings.Contains(err.Error(), "primary") ||
		!strings.Contains(err.Error(), "cannot read images") {
		t.Fatalf("reachable primary hard refusal = %v", err)
	}
	if primary.probes != 1 || fallback.probes != 0 {
		t.Fatalf("primary/fallback probes = %d/%d, availability fallback escaped a hard refusal",
			primary.probes, fallback.probes)
	}
}

func TestRoutedPickRejectsFallbackCapMismatchBeforeProbing(t *testing.T) {
	cat := evalCatalog(t)
	valid := &recordingProvider{probe: liveProbe()}
	primary := &recordingProvider{probe: liveProbe()}
	fallback := &recordingProvider{probe: liveProbe()}
	validTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-4-8"}
	primaryTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}
	fallbackTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"}
	primaryTarget.Params.MaxOutputTokens = 100
	fallbackTarget.Params.MaxOutputTokens = 200
	routed := RoutedArmFor{Catalog: cat, Ladder: []Arm{
		{Name: "valid", Target: validTarget, Provider: valid},
		{
			Name: "invalid", Target: primaryTarget, Provider: primary,
			Fallbacks: []Fallback{{Target: fallbackTarget, Provider: fallback}},
		},
	}}

	_, _, err := routed.PickRequest(context.Background(), Task{Prompt: "small fix"}, provider.Request{
		Messages: []provider.Message{provider.UserText("small fix")},
	})
	if !errors.Is(err, errRoutedFidelity) || !strings.Contains(err.Error(), "fallback 1 has max_output 200") ||
		!strings.Contains(err.Error(), "rung's 100") {
		t.Fatalf("fallback cap mismatch = %v", err)
	}
	if valid.probes != 0 || primary.probes != 0 || fallback.probes != 0 {
		t.Fatalf("cap mismatch probed valid/primary/fallback = %d/%d/%d",
			valid.probes, primary.probes, fallback.probes)
	}
}

func TestRoutedPickEnforcesPolicyBudgetAndContextEnvelope(t *testing.T) {
	cat := evalCatalog(t)
	providerOK := &recordingProvider{probe: liveProbe()}
	target := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}
	request := provider.Request{Messages: []provider.Message{provider.UserText("small fix")}}

	tests := []struct {
		name   string
		arm    Arm
		policy RoutedArmFor
	}{
		{
			name: "destination policy",
			arm:  Arm{Name: "only", Target: target, Provider: providerOK},
			policy: RoutedArmFor{Requirements: router.Requirements{
				ApprovedProviders: []string{"kimi"},
			}},
		},
		{
			name:   "hard budget",
			arm:    Arm{Name: "only", Target: target, Provider: providerOK},
			policy: RoutedArmFor{Budgets: router.Budgets{MaxCost: 1, MaxCostSet: true}},
		},
	}
	info, _, ok := cat.Lookup(target)
	if !ok {
		t.Fatal("test target is absent from catalog")
	}
	tight := target
	tight.Params.MaxOutputTokens = info.ContextWindow
	tests = append(tests, struct {
		name   string
		arm    Arm
		policy RoutedArmFor
	}{name: "input plus output context envelope", arm: Arm{Name: "only", Target: tight, Provider: providerOK}})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.policy.Catalog = cat
			test.policy.Ladder = []Arm{test.arm}
			_, _, err := test.policy.PickRequest(context.Background(), Task{Prompt: "small fix"}, request)
			if !errors.Is(err, errRoutedFidelity) {
				t.Fatalf("error = %v, want routed fidelity refusal", err)
			}
		})
	}
}

func TestExplicitOutputAboveCatalogRaisesOpeningAndPerCallBudgetBound(t *testing.T) {
	cat := evalCatalog(t)
	baseTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}
	info, _, ok := cat.Lookup(baseTarget)
	if !ok {
		t.Fatal("test target is absent from catalog")
	}
	explicitTarget := baseTarget
	explicitTarget.Params.MaxOutputTokens = info.MaxOutput + 10_000
	server := &recordingProvider{probe: liveProbe()}
	request := provider.Request{Messages: []provider.Message{provider.UserText("small fix")}}
	promptTokens := prefix.RequestTokens(request)
	contextTokens := prefix.RequestTokenCeiling(request)
	oldBound := candidateForRequest(
		Arm{Name: "only", Target: baseTarget}, 0, info, promptTokens, contextTokens).CeilingCost
	explicitBound := candidateForRequest(
		Arm{Name: "only", Target: explicitTarget}, 0, info, promptTokens, contextTokens).CeilingCost
	if explicitBound <= oldBound {
		t.Fatalf("explicit output did not raise ceiling: old=%s explicit=%s", oldBound, explicitBound)
	}

	routed := RoutedArmFor{
		Catalog: cat,
		Ladder:  []Arm{{Name: "only", Target: explicitTarget, Provider: server}},
		Budgets: router.Budgets{MaxCost: oldBound, MaxCostSet: true},
	}
	if _, _, err := routed.PickRequest(context.Background(), Task{Prompt: "small fix"}, request); !errors.Is(err, errRoutedFidelity) {
		t.Fatalf("opening admitted explicit output above budget: %v", err)
	}

	// The loop rechecks the same concrete adapter allowance before every call,
	// not just at opening selection.
	withoutBudget := routed
	withoutBudget.Budgets = router.Budgets{}
	_, loop := escalationHarness(t, withoutBudget, provider.UserText("continue"))
	callRequest := provider.Request{
		System: loop.System, Tools: loop.Tools.Definitions(), Messages: loop.Session.State().Messages,
	}
	callContext := prefix.RequestTokenCeiling(callRequest)
	oldCallBound := candidateForRequest(
		Arm{Name: "only", Target: baseTarget}, 0, info, callContext, callContext).CeilingCost
	guard := newEvalBudget(
		router.Budgets{MaxCost: oldCallBound, MaxCostSet: true}, cat, loop)
	if err := guard.before(callContext, 1); !errors.Is(err, errRoutedFidelity) {
		t.Fatalf("per-call guard admitted explicit output above budget: %v", err)
	}
}

func TestRoutedRunEscalatesOnlyAfterPreparedMoveAndMarksOpeningEstimateUnavailable(t *testing.T) {
	cat := evalCatalog(t)
	lowTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}
	highTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"}
	low := &recordingProvider{
		probe: liveProbe(),
		turns: [][]provider.Event{readTurn("one"), readTurn("two"), readTurn("three")},
	}
	high := &recordingProvider{probe: liveProbe(), turns: [][]provider.Event{completedTurn()}}
	routed := RoutedArmFor{Catalog: cat, Ladder: []Arm{
		{Name: "low", Target: lowTarget, Provider: low},
		{Name: "high", Target: highTarget, Provider: high},
	}}
	task := Task{
		ID: "move", Provenance: HandWritten, Prompt: "make the small fix",
		Setup: func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "note.txt"), []byte("hello\n"), 0o600)
		},
		Verify: func(context.Context, string) (bool, string, error) { return true, "", nil },
	}

	got := routed.Run(context.Background(), Runner{Catalog: cat, MaxRounds: 5}, task, 0)
	if !got.Solved || got.Failure != "" {
		t.Fatalf("escalated run = %#v", got)
	}
	if got.Target != highTarget.ID() || got.EstimatedTarget != lowTarget.ID() || got.Escalations != 1 {
		t.Fatalf("routing attribution = actual %s estimate %s moves %d",
			got.Target, got.EstimatedTarget, got.Escalations)
	}
	summary := Summarize(RoutedArm, []Run{got})
	if summary.EstimatesUnavailable != 1 || len(summary.EstimateError) != 0 {
		t.Fatalf("moved estimate was reconciled across targets: %#v", summary)
	}
}

func TestUnpreparableEscalationStaysAndLetsTheRoutedRunContinue(t *testing.T) {
	cat := evalCatalog(t)
	lowTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}
	highTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"}
	low := &recordingProvider{
		probe: liveProbe(),
		turns: [][]provider.Event{readTurn("one"), readTurn("two"), readTurn("three"), completedTurn()},
	}
	high := &recordingProvider{probe: liveProbe()}
	routed := RoutedArmFor{Catalog: cat, Ladder: []Arm{
		{Name: "low", Target: lowTarget, Provider: low},
		{Name: "high", Target: highTarget, Provider: high},
	}}
	verified := false
	task := Task{
		ID: "blocked-move", Provenance: HandWritten, Prompt: "make the small fix",
		Setup: func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "note.txt"), []byte("hello\n"), 0o600)
		},
		Verify: func(context.Context, string) (bool, string, error) {
			verified = true
			return true, "", nil
		},
	}

	// Opening selection sees the high rung as live. The move's second live
	// probe then fails, proving that the prepared destination—not stale opening
	// evidence—governs the atomic bind.
	high.probe = provider.ProbeResult{Detail: "went offline before the move"}
	selector := &captureRouter{}
	selectorRouteLow := router.Router(routerFunc(func(input router.Input) (router.Decision, error) {
		selector.input = input
		chosen := input.Candidates[0]
		// Make high live for opening resolution, then unavailable for the move.
		high.mu.Lock()
		high.probe = provider.ProbeResult{Detail: "went offline before the move"}
		high.mu.Unlock()
		return router.Decision{Tier: chosen.Tier, Target: chosen.Target.ID(), EstimatedCost: chosen.Estimate}, nil
	}))
	high.probe = liveProbe()
	routed.Router = selectorRouteLow

	got := routed.Run(context.Background(), Runner{Catalog: cat, MaxRounds: 5}, task, 0)
	if !got.Solved || got.Failure != "" || !verified {
		t.Fatalf("production-equivalent stay did not continue cleanly: run=%#v verified=%v", got, verified)
	}
	if got.Target != lowTarget.ID() || got.Escalations != 0 {
		t.Fatalf("rejected move changed target/rank: target=%s moves=%d", got.Target, got.Escalations)
	}
}

func TestFixedArmProbesOnlyItsPrimaryAndIgnoresRoutedFallbacks(t *testing.T) {
	cat := evalCatalog(t)
	primary := &recordingProvider{probe: liveProbe(), turns: [][]provider.Event{completedTurn()}}
	fallback := &recordingProvider{probe: liveProbe(), turns: [][]provider.Event{completedTurn()}}
	arm := Arm{
		Name:     "fixed",
		Target:   provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"},
		Provider: primary,
		Fallbacks: []Fallback{{
			Target:   provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"},
			Provider: fallback,
		}},
	}
	task := Task{
		ID: "fixed", Provenance: HandWritten, Prompt: "answer",
		Setup:  func(string) error { return nil },
		Verify: func(context.Context, string) (bool, string, error) { return true, "", nil },
	}

	got := (Runner{Catalog: cat}).Run(context.Background(), task, arm, 0)
	if !got.Solved || got.Target != arm.Target.ID() {
		t.Fatalf("fixed run = %#v", got)
	}
	if primary.probes != 1 || fallback.probes != 0 || len(fallback.requests) != 0 {
		t.Fatalf("fixed arm evidence/fallback use: primary probes=%d fallback probes=%d requests=%d",
			primary.probes, fallback.probes, len(fallback.requests))
	}
}

func TestResolvedContextWindowMatchesProductionPrecedence(t *testing.T) {
	tests := []struct {
		name                      string
		declared, probed, catalog int
		enforced                  bool
		want                      int
	}{
		{name: "enforced live beats declaration", declared: 32_000, probed: 16_000, catalog: 128_000, enforced: true, want: 16_000},
		{name: "declaration beats metadata hint", declared: 32_000, probed: 64_000, catalog: 128_000, want: 32_000},
		{name: "metadata hint beats catalog", probed: 64_000, catalog: 128_000, want: 64_000},
		{name: "catalog is fallback", catalog: 128_000, want: 128_000},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := resolvedContextWindow(test.declared, test.probed, test.enforced, test.catalog); got != test.want {
				t.Fatalf("resolved context = %d, want %d", got, test.want)
			}
		})
	}
}

func TestOpeningRouteUsesLiveNegativeVisionAndEnforcedContextEvidence(t *testing.T) {
	cat := evalCatalog(t)
	target := anthropic.Target("claude-opus-5")
	info, _, ok := cat.Lookup(target)
	if !ok || !info.Vision || info.ContextWindow <= 100 {
		t.Fatalf("fixture needs a catalogued vision target with a broad context: %+v", info)
	}

	visionProbe := liveProbe()
	visionProbe.VisionKnown = true
	visionProbe.Vision = false
	visionless := RoutedArmFor{
		Catalog:      cat,
		Ladder:       []Arm{{Name: "only", Target: target, Provider: &recordingProvider{probe: visionProbe}}},
		Requirements: router.Requirements{NeedsVision: true},
	}
	_, _, err := visionless.PickRequest(context.Background(), Task{Prompt: "inspect"}, provider.Request{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: []provider.Block{
			provider.Text{Text: "inspect"}, provider.Image{MediaType: "image/png", Data: []byte("x")},
		}}},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot read images") {
		t.Fatalf("live text-only evidence did not override catalog vision: %v", err)
	}

	tightTarget := target
	tightTarget.Params.MaxOutputTokens = 100
	windowProbe := liveProbe()
	windowProbe.ContextWindow = 100
	windowProbe.WindowEnforced = true
	tight := RoutedArmFor{Catalog: cat, Ladder: []Arm{{
		Name: "only", Target: tightTarget, Provider: &recordingProvider{probe: windowProbe}, ContextWindow: 10_000,
	}}}
	_, _, err = tight.PickRequest(context.Background(), Task{Prompt: "answer"}, provider.Request{
		Messages: []provider.Message{provider.UserText("answer")},
	})
	if err == nil || !strings.Contains(err.Error(), "holds 100 tokens") {
		t.Fatalf("enforced live context did not override the declared/catalog window: %v", err)
	}
}

func TestFixedArmLoopUsesProbedContextBeforeStreaming(t *testing.T) {
	cat := evalCatalog(t)
	target := anthropic.Target("claude-haiku-4-5")
	target.Params.MaxOutputTokens = 64
	probe := liveProbe()
	probe.ContextWindow = 64
	probe.WindowEnforced = true
	primary := &recordingProvider{probe: probe, turns: [][]provider.Event{completedTurn()}}
	arm := Arm{Name: "fixed", Target: target, Provider: primary, ContextWindow: 100_000}
	task := Task{
		ID: "fixed-live-window", Provenance: HandWritten, Prompt: "answer",
		Setup: func(string) error { return nil }, Verify: func(context.Context, string) (bool, string, error) { return true, "", nil },
	}

	got := (Runner{Catalog: cat}).Run(context.Background(), task, arm, 0)
	if got.Failure != FailureTurn || !strings.Contains(got.Detail, "holds 64 tokens") {
		t.Fatalf("fixed run did not enforce probed context: %#v", got)
	}
	if primary.probes != 1 || len(primary.requests) != 0 {
		t.Fatalf("fixed run probes/requests = %d/%d, want 1/0", primary.probes, len(primary.requests))
	}
}

func TestLiveCustomCapFitsAtBoundaryAndRefusesOneTokenOver(t *testing.T) {
	target := provider.RouteTarget{
		Provider: "openaicompat", Surface: "generic", ModelID: "custom-not-in-catalog",
		Params: provider.Params{MaxOutputTokens: 4096},
	}
	probe := liveProbe()
	probe.ContextWindow = 32_768
	probe.WindowEnforced = true
	bound := &recordingProvider{probe: probe}
	resolved, info, err := resolveArmEvidence(context.Background(), evalCatalog(t), Arm{
		Name: "only", Target: target, Provider: bound,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.resolvedContextWindow != 32_768 || info.ContextWindow != 32_768 {
		t.Fatalf("resolved/live context = %d/%d, want 32768", resolved.resolvedContextWindow, info.ContextWindow)
	}

	for _, test := range []struct {
		name    string
		context int
		wantErr bool
	}{
		{name: "exact boundary", context: 28_672},
		{name: "one token over", context: 28_673, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := candidateForRequest(resolved, 0, info, test.context, test.context)
			candidate.CatalogKnown = false
			_, routeErr := (router.Heuristic{}).Route(router.Input{
				Candidates: []router.Candidate{candidate}, Pin: "only",
				Requirements: router.Requirements{NeedsTools: true},
			})
			if (routeErr != nil) != test.wantErr {
				t.Fatalf("context %d route error = %v, wantErr %v", test.context, routeErr, test.wantErr)
			}
		})
	}
}

func TestEvalLoopRechecksLiveCustomCapAfterToolRound(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "note.txt"), []byte("boundary evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	task := Task{ID: "post-tool-window", Provenance: HandWritten, Prompt: "read note.txt and continue"}
	prepared, err := prepareAttempt(task, workspace)
	if err != nil {
		t.Fatal(err)
	}
	opening := prefix.RequestTokenCeiling(prepared.openingRequest())
	const outputCap = 4096
	probe := liveProbe()
	probe.ContextWindow = opening + outputCap
	probe.WindowEnforced = true
	target := provider.RouteTarget{
		Provider: "openaicompat", Surface: "generic", ModelID: "custom-not-in-catalog",
		Params: provider.Params{MaxOutputTokens: outputCap},
	}
	bound := &recordingProvider{probe: probe, turns: [][]provider.Event{readTurn("read-boundary")}}
	arm, _, err := resolveArmEvidence(context.Background(), evalCatalog(t), Arm{
		Name: "fixed", Target: target, Provider: bound,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, runErr := (Runner{Catalog: evalCatalog(t), MaxRounds: 3}).attempt(context.Background(), arm, prepared, nil)
	var windowErr *agent.ContextWindowError
	if !errors.As(runErr, &windowErr) {
		t.Fatalf("post-tool refusal = %v, want ContextWindowError", runErr)
	}
	if windowErr.Window != opening+outputCap || windowErr.ReservedOutput != outputCap || windowErr.InputTokens <= opening {
		t.Fatalf("post-tool context refusal = %+v, opening input %d", windowErr, opening)
	}
	if len(bound.requests) != 1 {
		t.Fatalf("provider received %d requests, want only the fitting opening request", len(bound.requests))
	}
}

type routerFunc func(router.Input) (router.Decision, error)

func (f routerFunc) Route(input router.Input) (router.Decision, error) { return f(input) }

var _ router.Router = (*captureRouter)(nil)
var _ provider.Provider = (*recordingProvider)(nil)
