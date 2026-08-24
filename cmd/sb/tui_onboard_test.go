package main

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
)

// step delivers a message and runs any command it produces, following the
// wizard's message chain the way the Bubble Tea runtime would.
func step(t *testing.T, m *onboardModel, msg tea.Msg) {
	t.Helper()
	for msg != nil {
		_, cmd := m.Update(msg)
		if cmd == nil {
			return
		}
		msg = cmd()
		if _, quit := msg.(tea.QuitMsg); quit {
			return
		}
	}
}

func TestOnboardingBindsT1ForALocalModel(t *testing.T) {
	cfg := &config.Config{Path: filepath.Join(t.TempDir(), config.FileName)}
	m := &onboardModel{
		reg: newProviders("http://127.0.0.1:1", cfg),
		cat: &catalog.Catalog{Revision: "test"},
		cfg: cfg,
		th:  darkTheme(),
	}

	choices := map[string]modelChoice{
		"ollama/small local": {ref: "ollama/small", surface: "local", desc: "pulled locally"},
	}
	step(t, m, onboardChoicesMsg{
		items:   []pickerItem{{id: "ollama/small local", label: "ollama/small"}},
		choices: choices,
	})
	if m.dlg == nil {
		t.Fatal("the model picker never opened")
	}

	// Async pickers start neutral; navigation makes the choice deliberate.
	step(t, m, tea.KeyMsg{Type: tea.KeyDown})
	step(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if _, ok := m.dlg.(*textDialog); !ok {
		t.Fatalf("an unlisted local model should ask for a wire cap, dialog is %T", m.dlg)
	}
	step(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("4096")})
	step(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.cancelled || m.err != nil {
		t.Fatalf("wizard failed: cancelled=%v err=%v", m.cancelled, m.err)
	}
	// A ladder is the point of the tool, so binding one rung asks about the
	// next rather than dropping the user into a session.
	if m.quitting {
		t.Fatal("the wizard should offer another rung, not finish on the first one")
	}
	if m.step != stepMore {
		t.Fatalf("after a bind the wizard is at step %v, want the ladder question", m.step)
	}

	saved, err := config.LoadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	tier, ok := saved.Tier("t1")
	if !ok {
		t.Fatal("t1 was not persisted")
	}
	if tier.Target.ModelID != "small" || tier.Target.Provider != "ollama" {
		t.Fatalf("t1 bound to %+v, want ollama/small", tier.Target)
	}
	if tier.Target.Params.MaxOutputTokens != 4096 {
		t.Fatalf("t1 max output = %d, want the chosen 4096", tier.Target.Params.MaxOutputTokens)
	}

	// The rung is already on disk, so choosing to stop is the end of setup
	// rather than a cancellation of it.
	step(t, m, tea.KeyMsg{Type: tea.KeyDown})
	step(t, m, tea.KeyMsg{Type: tea.KeyDown})
	step(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.quitting || m.cancelled || m.err != nil {
		t.Fatalf("starting the session should end setup cleanly: quitting=%v cancelled=%v err=%v",
			m.quitting, m.cancelled, m.err)
	}
}

func TestOnboardingSummariesShowExactSurfaceAndRungCap(t *testing.T) {
	target := provider.RouteTarget{
		Provider: "openai", Surface: "subscription", ModelID: "shared-model",
		Params: provider.Params{MaxOutputTokens: 8192},
	}
	cfg := &config.Config{Tiers: []config.Tier{{ID: "t1", Target: target}}}
	summary := ladderSummary(cfg)
	for _, want := range []string{"t1", "openai/subscription/shared-model", "max:8192"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("ladder summary omitted %q: %q", want, summary)
		}
	}

	m := &onboardModel{
		cfg: cfg, th: darkTheme(),
		choice: modelChoice{
			ref: "openai/shared-model", provider: "openai", surface: "subscription",
			effortLevels: []string{"high"},
		},
	}
	effort := m.effortOrBind()().(pickerMsg)
	if !strings.Contains(effort.title, "openai/subscription/shared-model") {
		t.Fatalf("onboarding effort picker hid serving surface: %q", effort.title)
	}
	m.Update(onboardBoundMsg{tier: "t1"})
	joined := strings.Join(m.lines, "\n")
	for _, want := range []string{"openai/subscription/shared-model", "max:8192", "primary and fallbacks"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("onboarding bound line omitted %q: %q", want, joined)
		}
	}
}

func TestOnboardingEscapeCancelsCleanly(t *testing.T) {
	cfg := &config.Config{Path: filepath.Join(t.TempDir(), config.FileName)}
	m := &onboardModel{
		reg: newProviders("http://127.0.0.1:1", cfg),
		cat: &catalog.Catalog{Revision: "test"},
		cfg: cfg,
		th:  darkTheme(),
	}
	step(t, m, onboardChoicesMsg{
		items:   []pickerItem{{id: "x", label: "x"}},
		choices: map[string]modelChoice{"x": {ref: "ollama/x", surface: "local"}},
	})
	step(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if !m.cancelled {
		t.Fatal("escape at the picker should cancel setup")
	}
	if len(cfg.Tiers) != 0 {
		t.Fatal("a cancelled setup bound a tier anyway")
	}
}

func TestOnboardingChooserFitsShortTerminalAndFollowsSelection(t *testing.T) {
	items := make([]pickerItem, 24)
	for i := range items {
		items[i] = pickerItem{id: strconv.Itoa(i), label: "model-" + strconv.Itoa(i)}
	}
	for _, height := range []int{6, 10} {
		t.Run(strconv.Itoa(height), func(t *testing.T) {
			m := &onboardModel{th: darkTheme(), lines: []string{strings.Repeat("界", 60)}}
			m.dlg = &pickerDialog{title: "pick a model", items: items, sel: 23}
			m.Update(tea.WindowSizeMsg{Width: 20, Height: height})

			view := m.View()
			assertTUIViewBounds(t, view, 20, height)
			if !strings.Contains(stripANSI(view), "model-23") {
				t.Fatalf("selected model fell outside %dx%d onboarding view:\n%s", 20, height, view)
			}
		})
	}
}

func TestOnboardingHistoryFitsIrreducibleWidthOneGraphemes(t *testing.T) {
	m := &onboardModel{th: darkTheme(), lines: []string{strings.Repeat("界", 8)}}
	m.dlg = &pickerDialog{title: "pick", items: []pickerItem{{id: "one", label: "one"}}}
	m.Update(tea.WindowSizeMsg{Width: 1, Height: 10})
	assertTUIViewBounds(t, m.View(), 1, 10)

	m.quitting = true
	assertTUIViewBounds(t, m.View(), 1, 10)
}

func TestOnboardingChooserPreemptsTitleAtHeightOne(t *testing.T) {
	for _, width := range []int{1, 2, 10, 20} {
		m := &onboardModel{th: darkTheme()}
		m.dlg = &pickerDialog{title: "pick", items: []pickerItem{{id: "one", label: "one"}}}
		m.Update(tea.WindowSizeMsg{Width: width, Height: 1})
		view := m.View()
		assertTUIViewBounds(t, view, width, 1)
		plain := stripANSI(view)
		if !strings.Contains(plain, "▌") || (width >= 10 && !strings.Contains(plain, "one")) {
			t.Fatalf("%dx1 onboarding hid its active choice: %q", width, plain)
		}
	}
}

// First launch opens the connect checklist before any model is picked, and
// its exit row hands over to the model step.
func TestOnboardingStartsWithTheConnectChecklist(t *testing.T) {
	home := t.TempDir()
	isolateTestHome(t, home)

	cfg := &config.Config{Path: filepath.Join(home, config.FileName)}
	m := &onboardModel{
		reg: newProviders("http://127.0.0.1:1", cfg),
		cat: &catalog.Catalog{Revision: "test"},
		cfg: cfg,
		th:  darkTheme(),
	}

	msg, ok := m.Init()().(pickerMsg)
	if !ok {
		t.Fatalf("first launch should open the connect checklist, got %T", msg)
	}
	if !strings.Contains(msg.title, "connect") {
		t.Fatalf("unexpected first step: %q", msg.title)
	}
	var continueRow bool
	for _, it := range msg.items {
		continueRow = continueRow || (it.id == setupDoneID && it.label == "continue")
	}
	if !continueRow {
		t.Fatal("the checklist needs its handover row")
	}

	next := msg.action(setupDoneID)
	if next == nil {
		t.Fatal("continue produced nothing")
	}
	if m.step != stepModel {
		t.Fatal("continue should advance the wizard to the model step")
	}
}

// The ladder is what this tool is, so setup has to be able to build one. The
// wizard used to bind t1 and drop the user into a session, leaving every rung
// above it to a command they had not met yet.
func TestOnboardingBindsAsManyRungsAsAsked(t *testing.T) {
	cfg := &config.Config{Path: filepath.Join(t.TempDir(), config.FileName)}
	m := &onboardModel{
		reg: newProviders("http://127.0.0.1:1", cfg),
		cat: &catalog.Catalog{Revision: "test"},
		cfg: cfg,
		th:  darkTheme(),
	}

	bind := func(model string) {
		t.Helper()
		id := "ollama/" + model + " local"
		step(t, m, onboardChoicesMsg{
			items: []pickerItem{{id: id, label: "ollama/" + model}},
			choices: map[string]modelChoice{id: {
				ref: "ollama/" + model, surface: "local", catalogMaxOutput: 4096,
			}},
		})
		step(t, m, tea.KeyMsg{Type: tea.KeyDown})
		step(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	}

	bind("small")
	// Take the "add another rung" row, which is the first offered.
	step(t, m, tea.KeyMsg{Type: tea.KeyDown})
	step(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	bind("medium")
	step(t, m, tea.KeyMsg{Type: tea.KeyDown})
	step(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	bind("large")

	if m.quitting {
		t.Fatal("the wizard closed while rungs were still being added")
	}
	step(t, m, tea.KeyMsg{Type: tea.KeyDown})
	step(t, m, tea.KeyMsg{Type: tea.KeyDown})
	step(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.quitting || m.cancelled || m.err != nil {
		t.Fatalf("setup did not end cleanly: quitting=%v cancelled=%v err=%v",
			m.quitting, m.cancelled, m.err)
	}

	saved, err := config.LoadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Tiers) != 3 {
		t.Fatalf("the ladder has %d rungs, want the three that were bound: %v", len(saved.Tiers), saved.Tiers)
	}
	// Rungs fill from the bottom, because a session opens on t1 and climbs.
	for i, want := range []string{"small", "medium", "large"} {
		tier := saved.Tiers[i]
		if tier.ID != "t"+strconv.Itoa(i+1) || tier.Target.ModelID != want {
			t.Fatalf("rung %d is %s/%s, want t%d/%s", i, tier.ID, tier.Target.ModelID, i+1, want)
		}
	}
}

// Escape after a rung is bound means the ladder is done. The rung is already
// written, and calling that a cancelled setup would refuse to start a session
// against a configuration that exists and is valid.
func TestBackingOutAfterARungKeepsTheLadder(t *testing.T) {
	cfg := &config.Config{Path: filepath.Join(t.TempDir(), config.FileName)}
	m := &onboardModel{
		reg: newProviders("http://127.0.0.1:1", cfg),
		cat: &catalog.Catalog{Revision: "test"},
		cfg: cfg,
		th:  darkTheme(),
	}
	id := "ollama/small local"
	step(t, m, onboardChoicesMsg{
		items: []pickerItem{{id: id, label: "ollama/small"}},
		choices: map[string]modelChoice{id: {
			ref: "ollama/small", surface: "local", catalogMaxOutput: 4096,
		}},
	})
	step(t, m, tea.KeyMsg{Type: tea.KeyDown})
	step(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	step(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.cancelled {
		t.Fatal("backing out of the ladder question discarded a saved rung")
	}
	if !m.quitting {
		t.Fatal("backing out should end setup")
	}
	saved, err := config.LoadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := saved.Tier("t1"); !ok {
		t.Fatal("t1 did not survive backing out")
	}
}
