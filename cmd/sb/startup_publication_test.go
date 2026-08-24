package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/session"
)

type tuiProgramFunc func() (tea.Model, error)

func (f tuiProgramFunc) Run() (tea.Model, error) { return f() }

func TestNewStartupSessionCannotWinLatestBeforeAssemblyPublishes(t *testing.T) {
	server := fakeOllama(t, "startup-model")
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	tier := ollamaTier("t1", "startup-model")
	cfg := &config.Config{Tiers: []config.Tier{tier}}
	cat := catalogWithLocalModels(t, localModelSpec{name: "startup-model", contextWindow: 100_000})

	prior, err := store.Create(workspace, tier.Target.ID(), cat.Revision)
	if err != nil {
		t.Fatal(err)
	}
	priorID := prior.ID()
	if err := prior.Close(); err != nil {
		t.Fatal(err)
	}

	opts := options{}
	var chosen router.Decision
	fresh, _, _, resumed, _, err := openSession(
		context.Background(), store, newProviders(server.URL, cfg), cfg, cat, workspace, &opts, &chosen,
	)
	if err != nil {
		t.Fatal(err)
	}
	if resumed || !fresh.PublicationPending() {
		t.Fatalf("new startup session resumed=%v pending=%v", resumed, fresh.PublicationPending())
	}
	latest, err := store.Latest(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if latest.ID() != priorID {
		t.Fatalf("Latest before assembly commit = %s, want prior %s", latest.ID(), priorID)
	}
	_ = latest.Close()
	path := fresh.Path()
	if err := fresh.CloseDiscardingStaged(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(fresh.ID()); err == nil {
		t.Fatalf("discarded startup stage %s remained openable at %s", fresh.ID(), path)
	}
}

func TestRejectedHeadlessStartupCannotReplaceLatest(t *testing.T) {
	savedWorkflows := subagentWorkflows
	subagentWorkflows = nil
	t.Cleanup(func() { subagentWorkflows = savedWorkflows })

	tests := []struct {
		name     string
		prompt   string
		workflow string
		wantErr  string
	}{
		{
			name:    "secret refused",
			prompt:  "do not send " + testGitHubToken,
			wantErr: "-allow-secrets",
		},
		{
			name:     "workflow missing",
			workflow: "does-not-exist",
			wantErr:  "no workflows are defined",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := session.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			workspace := t.TempDir()
			prior, err := store.Create(workspace, "scripted/local/startup", "rev")
			if err != nil {
				t.Fatal(err)
			}
			priorID := prior.ID()
			if err := prior.Close(); err != nil {
				t.Fatal(err)
			}

			fresh, err := store.CreateStaged(workspace, "scripted/local/startup", "rev")
			if err != nil {
				t.Fatal(err)
			}
			defer fresh.CloseDiscardingStaged()
			err = publishSessionAfterStartupPreflight(fresh, tt.prompt, false, tt.workflow)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("startup preflight error = %v, want text %q", err, tt.wantErr)
			}
			if !fresh.PublicationPending() {
				t.Fatal("rejected startup session was published")
			}

			latest, err := store.Latest(workspace)
			if err != nil {
				t.Fatal(err)
			}
			if latest.ID() != priorID {
				t.Fatalf("Latest after rejected startup = %s, want prior %s", latest.ID(), priorID)
			}
			_ = latest.Close()
		})
	}
}

func TestTUIStartupFailureLeavesFreshSessionStaged(t *testing.T) {
	store, workspace, priorID, fresh := startupPublicationFixture(t)
	initialRan := false
	m := &tuiModel{
		startupPublication: fresh,
		initialCmd: func() tea.Msg {
			initialRan = true
			return nil
		},
	}
	terminalErr := errors.New("injected terminal initialization failure")
	err := runTUIProgram(tuiProgramFunc(func() (tea.Model, error) {
		// Bubble Tea returns before Init when opening the terminal fails.
		return m, terminalErr
	}), m)
	if !errors.Is(err, terminalErr) {
		t.Fatalf("runTUIProgram error = %v, want injected terminal error", err)
	}
	if initialRan {
		t.Fatal("terminal startup failure ran withheld TUI startup work")
	}
	assertStartupStageHidden(t, store, workspace, priorID, fresh)
}

func TestTUIStartupPublishesBeforeInitialWork(t *testing.T) {
	_, _, _, fresh := startupPublicationFixture(t)
	initialRan := false
	m := &tuiModel{startupPublication: fresh}
	m.initialCmd = func() tea.Msg {
		initialRan = true
		if fresh.PublicationPending() {
			t.Error("initial TUI work ran before session publication")
		}
		return nil
	}

	initCmd := m.Init()
	if initCmd == nil {
		t.Fatal("staged TUI Init returned no readiness command")
	}
	if initialRan {
		t.Fatal("staged TUI Init ran ordinary startup work")
	}
	_, cmd := m.Update(initCmd())
	if fresh.PublicationPending() {
		t.Fatal("trusted startup event did not publish session")
	}
	if cmd == nil {
		t.Fatal("successful publication dropped withheld startup work")
	}
	_ = cmd()
	if !initialRan {
		t.Fatal("withheld startup work did not run after publication")
	}
}

func TestTUIFirstWindowSizePublishesBeforeAcceptingInput(t *testing.T) {
	_, _, _, fresh := startupPublicationFixture(t)
	m := testModel(t)
	m.startupPublication = fresh
	before := m.ta.Value()
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ignored before startup")})
	if cmd != nil || m.ta.Value() != before {
		t.Fatal("TUI accepted input before startup publication")
	}

	_, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if fresh.PublicationPending() {
		t.Fatal("first trusted WindowSize event did not publish session")
	}
	if m.width != 100 || m.height != 30 {
		t.Fatalf("startup WindowSize = %dx%d, want 100x30", m.width, m.height)
	}
}

func TestTUIStartupPublicationFailureQuitsAndKeepsPriorLatest(t *testing.T) {
	store, workspace, priorID, fresh := startupPublicationFixture(t)
	if err := os.WriteFile(fresh.Path()+".published", []byte("foreign marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	initialRan := false
	m := &tuiModel{
		startupPublication: fresh,
		initialCmd: func() tea.Msg {
			initialRan = true
			return nil
		},
	}

	err := runTUIProgram(tuiProgramFunc(func() (tea.Model, error) {
		ready := m.Init()
		if ready == nil {
			t.Fatal("staged TUI Init returned no readiness command")
		}
		_, quit := m.Update(ready())
		if quit == nil {
			t.Fatal("publication failure did not request TUI quit")
		}
		_ = quit()
		return m, nil
	}), m)
	if err == nil || !strings.Contains(err.Error(), "publishing initialized TUI session") {
		t.Fatalf("runTUIProgram error = %v, want publication failure", err)
	}
	if initialRan {
		t.Fatal("publication failure ran withheld TUI startup work")
	}
	if !m.quitting {
		t.Fatal("publication failure did not put TUI in quitting state")
	}
	assertStartupStageHidden(t, store, workspace, priorID, fresh)
}

func startupPublicationFixture(t *testing.T) (*session.Store, string, string, *session.Session) {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	prior, err := store.Create(workspace, "test/local/prior", "rev")
	if err != nil {
		t.Fatal(err)
	}
	priorID := prior.ID()
	if err := prior.Close(); err != nil {
		t.Fatal(err)
	}
	fresh, err := store.CreateStaged(workspace, "test/local/fresh", "rev")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fresh.CloseDiscardingStaged() })
	return store, workspace, priorID, fresh
}

func assertStartupStageHidden(t *testing.T, store *session.Store, workspace, priorID string, fresh *session.Session) {
	t.Helper()
	if !fresh.PublicationPending() {
		t.Fatal("failed TUI startup published fresh session")
	}
	latest, err := store.Latest(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer latest.Close()
	if latest.ID() != priorID {
		t.Fatalf("Latest after failed TUI startup = %s, want prior %s", latest.ID(), priorID)
	}
}
