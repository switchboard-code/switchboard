package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/config"
	route "github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/session"
)

// The policy moving the primary mid-task is the product's central bet, and a
// bet is something a user gets to decline.
func TestRoutingOffStopsMovesAndSurvivesTheSession(t *testing.T) {
	m := testModel(t)
	m.app.config.Path = filepath.Join(t.TempDir(), config.FileName)
	m.app.sticky = route.NewSticky(route.Policy{}, 0)

	moved := 0
	m.app.watcher = newWatcher(nil, m.app.sticky, 3,
		func(ctx context.Context, rank int, why string) (func() bool, func(), bool) {
			moved++
			return func() bool { return true }, nil, true
		})

	if !m.app.config.RouteAutoOn() {
		t.Fatal("routing is on unless the user says otherwise")
	}

	cmd := cmdRouting(m, "off")
	if n, ok := cmd().(noticeMsg); !ok || n.level == "error" {
		t.Fatalf("/routing off failed: %#v", cmd())
	}
	if !m.app.watcher.isPaused() {
		t.Fatal("the watcher is still allowed to move the primary")
	}

	// Evidence enough to escalate, which now changes nothing.
	for i := 0; i < 12; i++ {
		m.app.watcher.observe([]route.Signal{route.RepeatedToolCall, route.ToolErrorSpike})
		m.app.watcher.assess(context.Background())
	}
	if moved != 0 {
		t.Fatalf("routing off still moved the primary %d times", moved)
	}

	// The choice outlives the process.
	saved, err := config.LoadFile(m.app.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.RouteAutoOn() {
		t.Fatal("routing off did not persist")
	}

	// And it is reversible.
	if n, ok := cmdRouting(m, "on")().(noticeMsg); !ok || n.level == "error" {
		t.Fatalf("/routing on failed: %#v", n)
	}
	if m.app.watcher.isPaused() {
		t.Fatal("routing on left the watcher paused")
	}
	if standing := m.routingStanding(); !strings.Contains(standing, "routing is on") {
		t.Errorf("status = %q", standing)
	}
}

func TestRoutingSaveFailureLeavesLivePostureUnchanged(t *testing.T) {
	for _, initial := range []bool{false, true} {
		initial := initial
		t.Run(map[bool]string{false: "off", true: "on"}[initial], func(t *testing.T) {
			m := testModel(t)
			m.app.config.Path = t.TempDir() // a directory cannot be replaced by the config file
			m.app.config.RouteAuto = &initial
			m.app.sticky = route.NewSticky(route.Policy{}, 0)
			m.app.watcher = newWatcher(nil, m.app.sticky, 1, nil)
			m.app.watcher.setPaused(!initial)

			requested := !initial
			msg, ok := cmdRouting(m, map[bool]string{false: "off", true: "on"}[requested])().(noticeMsg)
			if !ok || msg.level != "error" || !strings.Contains(msg.text, "saving the routing setting failed") {
				t.Fatalf("failed save notice = %#v", msg)
			}
			if m.app.config.RouteAutoOn() != initial {
				t.Fatalf("failed save changed live config from %v to %v", initial, m.app.config.RouteAutoOn())
			}
			if m.app.watcher.isPaused() != !initial {
				t.Fatalf("failed save changed watcher pause from %v to %v", !initial, m.app.watcher.isPaused())
			}
		})
	}
}

// A relief substitutes another rung mid-turn, which is exactly the move
// routing off reserves for the user.
func TestRoutingOffRefusesRelief(t *testing.T) {
	m := testModel(t)
	off := false
	m.app.config.RouteAuto = &off

	_, _, err := m.app.relief(context.Background(), agent.ReliefAvailability, errors.New("the target stopped answering"))
	if err == nil || !strings.Contains(err.Error(), "routing is off") {
		t.Fatalf("relief with routing off = %v, want a refusal naming the setting", err)
	}
}

func TestReliefCarriesAndRecordsSuccessfulFallbackNote(t *testing.T) {
	m, expected := fallbackReliefModel(t)

	binding, note, err := m.app.relief(context.Background(), agent.ReliefAvailability, errors.New("primary timed out"))
	if err != nil {
		t.Fatal(err)
	}
	if note != expected {
		t.Fatalf("relief note = %q, want exact probe note %q", note, expected)
	}
	if binding.Target.ModelID != "backup" {
		t.Fatalf("relief bound %s, want backup", binding.Target.Display())
	}
	state := m.app.loop.Session.State()
	if state.RuntimeBinding.Tier != "t2" || state.RuntimeBinding.Target != binding.Target.ID() || state.RuntimeBinding.Note != nil {
		t.Fatalf("durable relief binding = %+v", state.RuntimeBinding)
	}
	timeline, err := session.ReadTimeline(m.app.loop.Session.Path())
	if err != nil {
		t.Fatal(err)
	}
	if countTimelineNotes(timeline, expected) != 1 {
		t.Fatalf("fallback note was not recorded exactly once: %+v", timeline)
	}
}

func TestReliefFailureRecordsNeitherFallbackNoteNorBinding(t *testing.T) {
	m, _ := fallbackReliefModel(t)
	m.app.config.Tiers[1].Fallbacks[0].ModelID = "also-missing"
	before := m.app.loop.Session.State().RuntimeBinding

	_, note, err := m.app.relief(context.Background(), agent.ReliefAvailability, errors.New("primary timed out"))
	if err == nil || note != "" {
		t.Fatalf("failed relief = note %q error %v", note, err)
	}
	if got := m.app.loop.Session.State().RuntimeBinding; got != before {
		t.Fatalf("failed relief changed binding from %+v to %+v", before, got)
	}
	timeline, readErr := session.ReadTimeline(m.app.loop.Session.Path())
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, item := range timeline {
		if item.Note != nil && strings.Contains(item.Note.Text, "served by its fallback") {
			t.Fatalf("failed relief recorded fallback note %q", item.Note.Text)
		}
	}
}

func TestReliefStaleProbeRecordsNeitherFallbackNoteNorBinding(t *testing.T) {
	m, expected := fallbackReliefModel(t)
	before := m.app.loop.Session.State().RuntimeBinding
	m.app.reliefAfterProbe = func() {
		// Reinstalling even the same endpoint retires every prepared provider
		// capability from the prior generation.
		m.app.providers.adoptOllamaHost(m.app.providers.localServer().BaseURL())
	}

	_, note, err := m.app.relief(context.Background(), agent.ReliefAvailability, errors.New("primary timed out"))
	var stale *providerReconfiguredError
	if !errors.As(err, &stale) || note != "" {
		t.Fatalf("stale relief = note %q error %v, want generation refusal", note, err)
	}
	if got := m.app.loop.Session.State().RuntimeBinding; got != before {
		t.Fatalf("stale relief changed binding from %+v to %+v", before, got)
	}
	timeline, readErr := session.ReadTimeline(m.app.loop.Session.Path())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if countTimelineNotes(timeline, expected) != 0 {
		t.Fatal("stale relief attributed the retired generation's fallback note")
	}
}

func fallbackReliefModel(t *testing.T) (*tuiModel, string) {
	t.Helper()
	server := fakeOllama(t, "current", "backup")
	current := ollamaTier("t1", "current")
	candidate := ollamaTier("t2", "missing", "backup")
	cfg := &config.Config{Tiers: []config.Tier{current, candidate}}
	registry := newProviders(server.URL, cfg)
	served, client, err := registry.probeTier(context.Background(), current)
	if err != nil {
		t.Fatal(err)
	}

	m := testModel(t)
	m.app.config = cfg
	m.app.catalog = catalogWithLocalModels(t,
		localModelSpec{name: "current", contextWindow: 100_000},
		localModelSpec{name: "missing", contextWindow: 100_000},
		localModelSpec{name: "backup", contextWindow: 100_000},
	)
	m.app.providers = registry
	m.app.tier = served
	m.app.sticky = route.NewSticky(route.Policy{}, 0)
	m.app.bindRuntime(served, client)
	if err := m.app.loop.Session.AppendRuntimeBinding(served.ID, served.Target.ID(), false); err != nil {
		t.Fatal(err)
	}

	_, _, expected, err := registry.probeTierFallback(context.Background(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	return m, expected
}

func countTimelineNotes(timeline []session.Timeline, text string) int {
	count := 0
	for _, item := range timeline {
		if item.Note != nil && item.Note.Text == text {
			count++
		}
	}
	return count
}
