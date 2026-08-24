package eval

import (
	"context"
	"testing"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

func escalationHarness(t *testing.T, routed RoutedArmFor, messages ...provider.Message) (*escalator, *agent.Loop) {
	t.Helper()
	workspace := t.TempDir()
	capability := execution.TestingVerifiedCapability()
	registry, err := tools.NewRegistry(workspace, capability)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(workspace + "/sessions")
	if err != nil {
		t.Fatal(err)
	}
	start := routed.Ladder[0]
	sess, err := store.Create(workspace, start.Target.ID(), routed.Catalog.Revision)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	for _, message := range messages {
		if err := sess.AppendMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	loop := &agent.Loop{
		Provider: start.Provider,
		Target:   start.Target,
		Tools:    registry,
		Session:  sess,
		Observer: agent.NopObserver{},
		Catalog:  routed.Catalog,
		System:   []provider.Block{provider.Text{Text: "evaluation system"}},
	}
	installEvalResolvers(loop, start, routed.Catalog)
	escalation := &escalator{
		sticky: router.NewSticky(router.Policy{
			EscalateAfter: 0.5, MinimumDwell: 1,
		}, 0),
		detect:  router.NewDetector(),
		ladder:  routed.Ladder,
		catalog: routed.Catalog,
		routed:  routed,
	}
	escalation.attach(loop)
	escalation.sticky.CallServed()
	escalation.sticky.Observe(router.DiffGrew)
	return escalation, loop
}

func TestEscalationPreparesFallbackBeforeAtomicBind(t *testing.T) {
	cat := evalCatalog(t)
	lowTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}
	primaryTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"}
	fallbackTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-4-8"}
	primaryTarget.Params.MaxOutputTokens = 100
	fallbackTarget.Params.MaxOutputTokens = 100
	low := &recordingProvider{probe: liveProbe()}
	unreachable := &recordingProvider{probe: provider.ProbeResult{Detail: "offline"}}
	fallbackProbe := liveProbe()
	fallbackProbe.ContextWindow = 50_000
	fallbackProbe.WindowEnforced = true
	fallback := &recordingProvider{probe: fallbackProbe}
	routed := RoutedArmFor{Catalog: cat, Ladder: []Arm{
		{Name: "low", Target: lowTarget, Provider: low},
		{
			Name: "high", Target: primaryTarget, Provider: unreachable,
			Fallbacks: []Fallback{{Target: fallbackTarget, Provider: fallback}},
		},
	}}
	escalation, loop := escalationHarness(t, routed, provider.UserText("continue"))

	escalation.assess(context.Background())

	if err := escalation.fidelityError(); err != nil {
		t.Fatalf("prepared fallback was refused: %v", err)
	}
	if got := loop.Binding().Target.ID(); got != fallbackTarget.ID() {
		t.Fatalf("bound target = %s, want fallback %s", got, fallbackTarget.ID())
	}
	if escalation.sticky.Rank() != 1 || escalation.moves != 1 {
		t.Fatalf("sticky rank/moves = %d/%d, want 1/1", escalation.sticky.Rank(), escalation.moves)
	}
	if got := loop.ContextWindow(fallbackTarget); got != 50_000 {
		t.Fatalf("loop context window after move = %d, want probed 50000", got)
	}
	if got := loop.OutputAllowance(fallbackTarget, 0); got != 100 {
		t.Fatalf("loop output allowance after move = %d, want explicit 100", got)
	}
}

func TestInfeasibleEscalationStaysWithoutChangingBindingOrStickyRank(t *testing.T) {
	cat := evalCatalog(t)
	lowTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}
	live := &recordingProvider{probe: liveProbe()}
	unreachable := &recordingProvider{probe: provider.ProbeResult{Detail: "offline"}}
	toolLess := &recordingProvider{probe: provider.ProbeResult{
		Reachable: true, ModelPresent: true, Tools: provider.ToolsNone,
	}}
	paidTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"}
	info, _, ok := cat.Lookup(paidTarget)
	if !ok {
		t.Fatal("paid target is absent from catalog")
	}
	tightTarget := paidTarget
	tightTarget.Params.MaxOutputTokens = info.ContextWindow
	visionlessTarget := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "visionless"}

	tests := []struct {
		name         string
		high         Arm
		requirements router.Requirements
		budgets      router.Budgets
		message      provider.Message
	}{
		{
			name: "reachability",
			high: Arm{Name: "high", Target: paidTarget, Provider: unreachable},
		},
		{
			name: "tools",
			high: Arm{Name: "high", Target: paidTarget, Provider: toolLess},
		},
		{
			name:         "destination policy",
			high:         Arm{Name: "high", Target: paidTarget, Provider: live},
			requirements: router.Requirements{ApprovedProviders: []string{"ollama"}},
		},
		{
			name:    "hard budget",
			high:    Arm{Name: "high", Target: paidTarget, Provider: live},
			budgets: router.Budgets{MaxCost: 1, MaxCostSet: true},
		},
		{
			name: "input plus reserved output context",
			high: Arm{Name: "high", Target: tightTarget, Provider: live},
		},
		{
			name: "vision",
			high: Arm{Name: "high", Target: visionlessTarget, Provider: live},
			message: provider.Message{Role: provider.RoleUser, Content: []provider.Block{
				provider.Text{Text: "inspect this"},
				provider.Image{MediaType: "image/png", Data: []byte("not-an-image")},
			}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message := test.message
			if len(message.Content) == 0 {
				message = provider.UserText("continue")
			}
			routed := RoutedArmFor{
				Catalog: cat,
				Ladder: []Arm{
					{Name: "low", Target: lowTarget, Provider: live},
					test.high,
				},
				Requirements: test.requirements,
				Budgets:      test.budgets,
			}
			escalation, loop := escalationHarness(t, routed, message)

			escalation.assess(context.Background())

			if err := escalation.fidelityError(); err != nil {
				t.Fatalf("production-equivalent stay contaminated the run: %v", err)
			}
			if got := loop.Binding().Target.ID(); got != lowTarget.ID() {
				t.Fatalf("binding changed to %s after rejected move", got)
			}
			if escalation.sticky.Rank() != 0 || escalation.moves != 0 {
				t.Fatalf("rejected move changed rank/moves to %d/%d",
					escalation.sticky.Rank(), escalation.moves)
			}
		})
	}
}
