package main

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/config"
)

func TestAsyncPickerRejectsOlderGenerationInSameSession(t *testing.T) {
	m := testModel(t)
	older := m.bindAsyncResult()
	current := m.bindAsyncResult()

	stale := older.bindPicker(pickerMsg{
		title: "stale picker",
		items: []pickerItem{{id: "stale", label: "stale"}},
	})
	_, _ = m.Update(stale)
	if m.dlg != nil {
		t.Fatal("an older asynchronous picker replaced the current UI request")
	}

	fresh := current.bindPicker(pickerMsg{
		title: "current picker",
		items: []pickerItem{{id: "current", label: "current"}},
	})
	_, _ = m.Update(fresh)
	picker, ok := m.dlg.(*pickerDialog)
	if !ok || picker.title != "current picker" {
		t.Fatalf("current picker = %#v, want current request", m.dlg)
	}
}

func TestSessionSwapRejectsLatePickerAndCustomExpansion(t *testing.T) {
	m := testModel(t)
	old := m.bindAsyncResult()
	oldSessionID := currentSessionID(m)

	next, err := m.app.store.Create(m.app.workspace, m.app.tier.Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	binding := m.app.loop.Binding()
	if cmd := m.onSessionSwap(sessionSwapMsg{
		sess: next, tier: m.app.tier, client: binding.Provider, fresh: true,
	}); cmd != nil {
		t.Fatal("idle session swap unexpectedly returned a continuation")
	}
	t.Cleanup(func() { _ = m.app.loop.Session.Close() })
	if currentSessionID(m) == oldSessionID {
		t.Fatal("test did not replace the active session")
	}

	_, _ = m.Update(old.bindPicker(pickerMsg{
		title: "old session picker",
		items: []pickerItem{{id: "old", label: "old"}},
	}))
	if m.dlg != nil {
		t.Fatal("a picker assembled for the replaced session opened after the swap")
	}
	_, _ = m.Update(old.bindText(textPromptMsg{title: "old session text"}))
	if m.dlg != nil {
		t.Fatal("a text continuation assembled for the replaced session opened after the swap")
	}
	_, _ = m.Update(old.bindSecret(secretPromptMsg{storeName: "old session store"}))
	if m.dlg != nil {
		t.Fatal("a credential continuation assembled for the replaced session opened after the swap")
	}

	beforeEntries := len(m.tr.entries)
	_, cmd := m.Update(expandedCustomMsg{
		prompt: "PROVIDER-EXPANDED-CUSTOM-BODY", authored: "/custom:review exact args",
		generation: old.generation, sessionID: old.sessionID,
	})
	if cmd != nil || m.busy || m.turnPlanning || len(m.tr.entries) != beforeEntries {
		t.Fatalf("late custom expansion crossed the swap: cmd=%v busy=%v planning=%v entries=%d->%d",
			cmd != nil, m.busy, m.turnPlanning, beforeEntries, len(m.tr.entries))
	}
}

func TestUnboundPickerFailsClosedInSessionModel(t *testing.T) {
	m := testModel(t)
	_, _ = m.Update(pickerMsg{title: "local", items: []pickerItem{{id: "ok", label: "ok"}}})
	if m.dlg != nil {
		t.Fatalf("unbound picker opened in a live session: %T", m.dlg)
	}
	_, _ = m.Update(textPromptMsg{title: "unbound text"})
	if m.dlg != nil {
		t.Fatalf("unbound text prompt opened in a live session: %T", m.dlg)
	}
	_, _ = m.Update(secretPromptMsg{storeName: "unbound secret"})
	if m.dlg != nil {
		t.Fatalf("unbound secret prompt opened in a live session: %T", m.dlg)
	}
}

func TestAsyncNestedDialogsRejectOlderGeneration(t *testing.T) {
	m := testModel(t)
	older := m.bindAsyncResult()
	current := m.bindAsyncResult()

	_, _ = m.Update(older.bindText(textPromptMsg{title: "stale text"}))
	_, _ = m.Update(older.bindSecret(secretPromptMsg{storeName: "stale secret"}))
	if m.dlg != nil {
		t.Fatalf("older nested dialog opened: %T", m.dlg)
	}

	_, _ = m.Update(current.bindText(textPromptMsg{title: "current text"}))
	if d, ok := m.dlg.(*textDialog); !ok || d.title != "current text" {
		t.Fatalf("current nested dialog = %#v, want current text prompt", m.dlg)
	}
}

func TestAsyncModelsResultCannotCrossTurnOrSessionOperationOwnership(t *testing.T) {
	for _, test := range []struct {
		name  string
		claim func(*testing.T, *tuiModel) func()
	}{
		{
			name: "turn planning",
			claim: func(t *testing.T, m *tuiModel) func() {
				_, _ = m.startPlanning()
				return m.finishPlanning
			},
		},
		{
			name: "clear operation",
			claim: func(t *testing.T, m *tuiModel) func() {
				_, generation, _, err := m.startOperation("clear")
				if err != nil {
					t.Fatal(err)
				}
				return func() { m.finishOperation(generation, false) }
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := modelsTestModel(t)
			beforeTarget := m.app.config.Tiers[0].Target.ID()
			gather := cmdModels(m, "")
			release := test.claim(t, m)
			defer release()

			raw := gather()
			late, ok := raw.(pickerMsg)
			if !ok {
				t.Fatalf("/models discovery returned %T, want picker", raw)
			}
			_, _ = m.Update(late)
			if m.dlg != nil {
				t.Fatalf("late /models result opened %T after ownership changed", m.dlg)
			}
			if len(m.app.config.Tiers) != 1 || m.app.config.Tiers[0].Target.ID() != beforeTarget {
				t.Fatalf("late /models result changed the ladder: %+v", m.app.config.Tiers)
			}
		})
	}
}

func TestOpenModelsPickerCannotPublishAfterTurnStarts(t *testing.T) {
	m := modelsTestModel(t)
	beforeTarget := m.app.config.Tiers[0].Target.ID()
	msg, ok := cmdModels(m, "")().(pickerMsg)
	if !ok {
		t.Fatal("/models did not return a picker")
	}
	_, _ = m.Update(msg)
	if _, ok := m.dlg.(*pickerDialog); !ok {
		t.Fatalf("/models dialog = %T, want picker", m.dlg)
	}

	_, _ = m.startPlanning() // a scheduled/user turn claims the session
	defer m.finishPlanning()
	_ = m.key(tea.KeyMsg{Type: tea.KeyDown})
	cmd := m.key(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("stale picker did not report that its publication was refused")
	}
	notice, ok := cmd().(noticeMsg)
	if !ok || notice.level != "warn" {
		t.Fatalf("stale picker result = %#v, want warning", notice)
	}
	if m.dlg != nil {
		t.Fatalf("stale picker remained open as %T", m.dlg)
	}
	if len(m.app.config.Tiers) != 1 || m.app.config.Tiers[0].Target.ID() != beforeTarget {
		t.Fatalf("stale /models picker changed the ladder: %+v", m.app.config.Tiers)
	}
}

func TestLateOutputCapPromptCannotOpenDuringTurnPlanning(t *testing.T) {
	m := modelsTestModel(t)
	binding := m.bindAsyncResult()
	prompt := modelOutputCapPromptCmd(
		modelChoice{ref: "ollama/custom", provider: "ollama", surface: "local"},
		"", "", binding.bindText, func(modelChoice) tea.Cmd {
			t.Fatal("stale output-cap prompt advanced")
			return nil
		},
	)
	_, _ = m.startPlanning()
	defer m.finishPlanning()

	_, _ = m.Update(prompt())
	if m.dlg != nil {
		t.Fatalf("late output-cap prompt opened as %T", m.dlg)
	}
}

func TestModelsDiscoveryUsesImmutableConfigAndProviderSnapshots(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		close(started)
		<-release
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer server.Close()

	m := modelsTestModel(t)
	m.app.providers = newProviders(server.URL, m.app.config)
	discover := cmdModels(m, "") // snapshots while t1 and no custom address exist
	result := make(chan tea.Msg, 1)
	go func() { result <- discover() }()
	<-started

	stopWriting := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-stopWriting:
				return
			default:
				m.app.config.Tiers = nil
				m.app.config.Providers = map[string]config.ProviderSettings{
					config.ProviderSurfaceKey("openaicompat", "generic"): {BaseURL: "http://changed.invalid"},
				}
				runtime.Gosched()
			}
		}
	}()
	close(release)
	raw := <-result
	close(stopWriting)
	<-writerDone

	picker, ok := raw.(pickerMsg)
	if !ok {
		t.Fatalf("snapshot discovery returned %T, want picker", raw)
	}
	foundRemove := false
	for _, item := range picker.items {
		if item.id == removeRungID {
			foundRemove = true
			break
		}
	}
	if !foundRemove {
		t.Fatal("discovery observed the concurrently changed live tiers instead of its launch snapshot")
	}
}
