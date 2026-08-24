package main

import (
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/advisor"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
	route "github.com/switchboard-code/switchboard/internal/router"
)

func providerLeaseSeamModel(t *testing.T) (*tuiModel, *providers, [2]config.Tier, [2]provider.Provider) {
	t.Helper()
	const (
		firstModel  = "lease-seam-a"
		secondModel = "lease-seam-b"
	)
	server := capabilityOllama(t, map[string]bool{firstModel: true, secondModel: true})
	tiers := [2]config.Tier{
		{ID: "t1", Target: provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: firstModel, Params: provider.Params{MaxOutputTokens: 100}}},
		{ID: "t2", Target: provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: secondModel, Params: provider.Params{MaxOutputTokens: 100}}},
	}
	cfg := &config.Config{Tiers: []config.Tier{tiers[0], tiers[1]}}
	cat := catalogWithLocalModels(t,
		localModelSpec{name: firstModel, contextWindow: 32_768, vision: true, priceMaxInput: 32_768},
		localModelSpec{name: secondModel, contextWindow: 32_768, vision: true, priceMaxInput: 32_768},
	)
	registry := newProviders(server.URL, cfg)
	var refs [2]provider.Provider
	for i, tier := range tiers {
		_, ref, err := registry.probeTier(t.Context(), tier)
		if err != nil {
			t.Fatal(err)
		}
		refs[i] = ref
	}

	m := testModel(t)
	m.app.workspace = t.TempDir()
	m.app.config = cfg
	m.app.catalog = cat
	m.app.providers = registry
	m.app.budget = &budgetState{}
	m.app.caches = newCacheSet(tiers[0].Target, nil)
	m.app.sticky = route.NewSticky(route.Policy{}, 0)
	m.app.tier = tiers[0]
	m.app.runtimeTier = tiers[0]
	m.app.bindRuntime(tiers[0], refs[0])
	m.tierLine = m.app.tierLine()
	return m, registry, tiers, refs
}

func TestAbnormalTUIExitCleansQueuedStaleSessionReprobe(t *testing.T) {
	m, registry, tiers, refs := providerLeaseSeamModel(t)
	m.trackOperationTasks = true
	_, operation, sourceID, err := m.startOperation("clear")
	if err != nil {
		t.Fatal(err)
	}
	staged, err := m.app.store.CreateStaged(m.app.workspace, tiers[0].Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	stagedID := staged.ID()
	stagedPath := staged.Path()
	released := make(chan struct{})
	registry.reset()
	cmd := m.onSessionSwap(sessionSwapMsg{
		sess: staged, tier: tiers[0], client: refs[0],
		operation: operation, sourceID: sourceID,
		release: func() { close(released) },
	})
	if cmd == nil {
		t.Fatal("stale session client was adopted instead of queuing a reprobe")
	}

	if err := runTUIProgram(tuiProgramFunc(func() (tea.Model, error) { return m, nil }), m); err != nil {
		t.Fatal(err)
	}
	select {
	case <-released:
	default:
		t.Fatal("queued reprobe retained its advisor barrier")
	}
	assertRetainedUnpublishedStage(t, m.app.store, m.app.workspace, stagedID, stagedPath)
	if msg := cmd(); msg != nil {
		t.Fatalf("abandoned queued reprobe returned %#v", msg)
	}
}

func exactLeaseOpening() provider.Message {
	return provider.Message{
		Role: provider.RoleUser,
		Content: []provider.Block{
			provider.Text{Text: "expanded prompt with immutable attachment"},
			provider.Image{MediaType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}},
		},
		Authored:      "typed @diagram.png",
		AuthoredKnown: true,
		ContinuityRef: strings.Repeat("a", 32),
	}
}

func TestStaleTurnAndOverrideResultsReplanExactOpening(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*tuiModel, config.Tier, provider.Provider, provider.Message) teaCommandResult
	}{
		{
			name: "automatic turn",
			run: func(m *tuiModel, tier config.Tier, stale provider.Provider, opening provider.Message) teaCommandResult {
				_, generation := m.startPlanning()
				cmd := m.onTurnPlan(turnPlanMsg{generation: generation, opening: opening,
					prompt: opening.Text(), tier: tier, client: stale})
				if cmd == nil {
					t.Fatal("stale turn plan was adopted instead of replanned")
				}
				return teaCommandResult{msg: cmd()}
			},
		},
		{
			name: "tier override",
			run: func(m *tuiModel, tier config.Tier, stale provider.Provider, opening provider.Message) teaCommandResult {
				_, generation := m.startPlanning()
				cmd := m.onOverrideProbe(overrideProbeMsg{generation: generation, opening: opening,
					prompt: opening.Text(), tier: tier, client: stale})
				if cmd == nil {
					t.Fatal("stale override probe was adopted instead of replanned")
				}
				return teaCommandResult{msg: cmd()}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, registry, tiers, refs := providerLeaseSeamModel(t)
			opening := exactLeaseOpening()
			registry.reset()
			result := tc.run(m, tiers[0], refs[0], opening).msg
			switch refreshed := result.(type) {
			case turnPlanMsg:
				if refreshed.err != nil {
					t.Fatal(refreshed.err)
				}
				if !reflect.DeepEqual(refreshed.opening, opening) {
					t.Fatalf("replanned opening changed:\n got %#v\nwant %#v", refreshed.opening, opening)
				}
				if !registry.preparedClientCurrent(refreshed.client) {
					t.Fatal("turn replan returned stale provider proof")
				}
			case overrideProbeMsg:
				if refreshed.err != nil {
					t.Fatal(refreshed.err)
				}
				if !reflect.DeepEqual(refreshed.opening, opening) {
					t.Fatalf("override replan changed exact opening:\n got %#v\nwant %#v", refreshed.opening, opening)
				}
				if !registry.preparedClientCurrent(refreshed.client) {
					t.Fatal("override replan returned stale provider proof")
				}
			default:
				t.Fatalf("replan returned %T", result)
			}
			m.finishPlanning()
		})
	}
}

// teaCommandResult avoids importing Bubble Tea solely to name its Msg
// interface in the table above.
type teaCommandResult struct{ msg any }

func TestEveryProviderBearingAsyncAdoptionSeamRefreshesStaleProof(t *testing.T) {
	t.Run("tier switch", func(t *testing.T) {
		m, registry, tiers, refs := providerLeaseSeamModel(t)
		ctx, operation, sourceID, err := m.startOperation("tier switch")
		if err != nil {
			t.Fatal(err)
		}
		registry.reset()
		cmd := m.onTierSwitch(tierSwitchMsg{tier: tiers[1], requested: tiers[1], client: refs[1],
			operation: operation, sourceID: sourceID})
		if cmd == nil {
			t.Fatal("stale tier switch was adopted")
		}
		msg := cmd().(tierSwitchMsg)
		if msg.err != nil || !registry.preparedClientCurrent(msg.client) {
			t.Fatalf("refreshed tier switch = %#v", msg)
		}
		m.finishOperation(operation, false)
		_ = ctx
	})

	t.Run("session swap", func(t *testing.T) {
		m, registry, tiers, refs := providerLeaseSeamModel(t)
		_, operation, sourceID, err := m.startOperation("clear")
		if err != nil {
			t.Fatal(err)
		}
		staged, err := m.app.store.CreateStaged(m.app.workspace, tiers[0].Target.ID(), "test")
		if err != nil {
			t.Fatal(err)
		}
		registry.reset()
		cmd := m.onSessionSwap(sessionSwapMsg{sess: staged, tier: tiers[0], client: refs[0],
			operation: operation, sourceID: sourceID})
		if cmd == nil {
			t.Fatal("stale session client was adopted")
		}
		msg := cmd().(sessionSwapMsg)
		if msg.err != nil || !registry.preparedClientCurrent(msg.client) {
			t.Fatalf("refreshed session swap = %#v", msg)
		}
		_ = staged.CloseDiscardingStaged()
		m.finishOperation(operation, false)
	})

	t.Run("race probe", func(t *testing.T) {
		m, registry, tiers, refs := providerLeaseSeamModel(t)
		_, operation, sourceID, err := m.startOperation("race probe")
		if err != nil {
			t.Fatal(err)
		}
		registry.reset()
		cmd := m.onRaceProbe(raceProbeMsg{operation: operation, sourceID: sourceID, prompt: "compare",
			requestedA: tiers[0], requestedB: tiers[1], a: tiers[0], b: tiers[1], ca: refs[0], cb: refs[1]})
		if cmd == nil {
			t.Fatal("stale race probes reached setup")
		}
		msg := cmd().(raceProbeMsg)
		if msg.err != nil || !registry.preparedClientCurrent(msg.ca) || !registry.preparedClientCurrent(msg.cb) {
			t.Fatalf("refreshed race probe = %#v", msg)
		}
		m.finishOperation(operation, false)
	})

	t.Run("race setup", func(t *testing.T) {
		m, registry, tiers, refs := providerLeaseSeamModel(t)
		_, operation, sourceID, err := m.startOperation("race setup")
		if err != nil {
			t.Fatal(err)
		}
		opening := exactLeaseOpening()
		registry.reset()
		probe := raceProbeMsg{operation: operation, sourceID: sourceID, prompt: "compare",
			requestedA: tiers[0], requestedB: tiers[1], a: tiers[0], b: tiers[1], ca: refs[0], cb: refs[1]}
		cmd := m.onRaceSetup(raceSetupMsg{operation: operation, sourceID: sourceID, probe: probe,
			prompt: opening.Text(), opening: opening, staleProviders: true})
		if cmd == nil {
			t.Fatal("stale race setup launched arms")
		}
		msg := cmd().(raceSetupMsg)
		if msg.err != nil {
			t.Fatal(msg.err)
		}
		if !reflect.DeepEqual(msg.opening, opening) {
			t.Fatal("race setup rebuilt the exact opening")
		}
		if !registry.preparedClientCurrent(msg.probe.ca) || !registry.preparedClientCurrent(msg.probe.cb) {
			t.Fatal("race setup retained stale probe proof")
		}
		for _, arm := range msg.arms {
			if arm != nil && arm.sess != nil {
				_ = arm.sess.CloseDiscardingStaged()
			}
		}
		if msg.release != nil {
			msg.release()
		}
		m.finishOperation(operation, false)
	})

	t.Run("advisor", func(t *testing.T) {
		m, registry, tiers, refs := providerLeaseSeamModel(t)
		_, operation, sourceID, err := m.startOperation("advisor on")
		if err != nil {
			t.Fatal(err)
		}
		staleAdvisor := advisor.New(m.app.watcher, refs[0], tiers[0].Target, func(string) {})
		registry.reset()
		cmd := m.onAdvisorReady(advisorReadyMsg{adv: staleAdvisor, tier: tiers[0], client: refs[0],
			action: "on", operation: operation, sourceID: sourceID})
		if cmd == nil {
			t.Fatal("stale advisor was installed")
		}
		msg := cmd().(advisorReadyMsg)
		if msg.err != nil || !registry.preparedClientCurrent(msg.client) {
			t.Fatalf("refreshed advisor = %#v", msg)
		}
		m.finishOperation(operation, false)
	})
}
