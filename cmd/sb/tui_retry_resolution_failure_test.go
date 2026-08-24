package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestRetryIntentResolutionFailureStopsEveryTurnEnding(t *testing.T) {
	tests := []struct {
		name    string
		turnErr error
	}{
		{name: "success"},
		{name: "provider error", turnErr: errors.New("injected provider failure")},
		{name: "cancellation", turnErr: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := testModel(t)
			_, intent := bindRetryRecoveryChild(t, m, true, true)
			child := m.app.loop.Session
			m.app.lifetime = newTUILifetime()
			m.activeRetryIntent = intent.ID
			m.busy = true
			m.callTokens, m.ctxWindow = 90, 100 // would auto-compact after a normal success
			m.queue = []string{"queued after retry"}
			deferredRan := false
			m.deferredStartup = func() tea.Cmd {
				deferredRan = true
				return func() tea.Msg { return noticeMsg{text: "must not run"} }
			}
			ctx, cancel := context.WithCancel(context.Background())
			m.turnCtx, m.turnCancel = ctx, cancel

			// Closing the authoritative WAL is a deterministic completion-append
			// fault. State remains readable, but CompleteRetryIntent cannot make
			// the started handoff disappear.
			if err := child.Close(); err != nil {
				t.Fatal(err)
			}
			cmd := m.onTurnDone(turnDoneMsg{
				generation: m.turnGeneration,
				err:        tt.turnErr,
				after:      child.State(),
			})
			if cmd == nil {
				t.Fatal("retry handoff resolution failure returned no quit command")
			}
			if _, ok := cmd().(tea.QuitMsg); !ok {
				t.Fatal("retry handoff resolution failure did not return tea.Quit")
			}
			if !m.quitting || m.shutdownErr == nil {
				t.Fatalf("retry handoff failure did not arm fatal shutdown: quitting=%v err=%v", m.quitting, m.shutdownErr)
			}
			for _, want := range []string{"durable recovery handoff could not be cleared", "Restart Switchboard", "without duplicating provider or tool work"} {
				if !strings.Contains(m.shutdownErr.Error(), want) {
					t.Fatalf("fatal retry error omitted %q: %v", want, m.shutdownErr)
				}
			}
			if tt.turnErr != nil && !errors.Is(m.shutdownErr, tt.turnErr) {
				t.Fatalf("fatal retry error lost turn outcome %v: %v", tt.turnErr, m.shutdownErr)
			}
			if deferredRan || m.deferredStartup != nil || len(m.queue) != 0 {
				t.Fatalf("fatal retry advanced continuation: deferredRan=%v deferred=%v queue=%v", deferredRan, m.deferredStartup != nil, m.queue)
			}
			if m.nextQueuedTurn() != nil {
				t.Fatal("fatal retry scheduled work after shutdown")
			}
			select {
			case <-m.app.lifetime.Done():
			default:
				t.Fatal("fatal retry left the application lifetime open")
			}
			if err := child.AppendNote("info", "must not append"); err == nil {
				t.Fatal("fatal retry left the authoritative child writable")
			}

			// Quit is a command and a buffered input message can arrive first.
			// The model-level gate must refuse it without restoring the queue.
			m.ta.SetValue("new work after fatal retry")
			_, updateCmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
			if updateCmd == nil {
				t.Fatal("fatal retry did not keep tea.Quit armed")
			}
			if _, ok := updateCmd().(tea.QuitMsg); !ok {
				t.Fatal("buffered input crossed fatal retry shutdown")
			}
		})
	}
}

func TestRetryIntentAbandonFailureStopsEveryPlanningEnding(t *testing.T) {
	tests := []struct {
		name string
		run  func(*tuiModel) tea.Cmd
	}{
		{
			name: "route refusal",
			run: func(m *tuiModel) tea.Cmd {
				return m.onOverrideProbe(overrideProbeMsg{
					generation: m.turnGeneration,
					err:        errors.New("injected retry route refusal"),
				})
			},
		},
		{
			name: "routing cancellation",
			run: func(m *tuiModel) tea.Cmd {
				return m.interrupt()
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := testModel(t)
			_, intent := bindRetryRecoveryChild(t, m, false, false)
			child := m.app.loop.Session
			m.app.lifetime = newTUILifetime()
			m.activeRetryIntent = intent.ID
			m.turnGeneration++
			m.busy = true
			m.turnPlanning = true
			m.callTokens, m.ctxWindow = 90, 100
			m.queue = []string{"queued after retry planning"}
			deferredRan := false
			m.deferredStartup = func() tea.Cmd {
				deferredRan = true
				return func() tea.Msg { return noticeMsg{text: "must not run"} }
			}
			ctx, cancel := context.WithCancel(context.Background())
			m.turnCtx, m.turnCancel = ctx, cancel

			// Pending retry planning resolves by appending an abandonment. Closing
			// the WAL faults that exact append before any provider call can start.
			if err := child.Close(); err != nil {
				t.Fatal(err)
			}
			cmd := tt.run(m)
			if cmd == nil {
				t.Fatal("retry abandonment failure returned no quit command")
			}
			if _, ok := cmd().(tea.QuitMsg); !ok {
				t.Fatal("retry abandonment failure did not return tea.Quit")
			}
			if !m.quitting || m.shutdownErr == nil {
				t.Fatalf("retry abandonment failure did not arm shutdown: quitting=%v err=%v", m.quitting, m.shutdownErr)
			}
			for _, want := range []string{"durable recovery handoff could not be cleared", "Restart Switchboard", "without duplicating provider or tool work"} {
				if !strings.Contains(m.shutdownErr.Error(), want) {
					t.Fatalf("fatal retry error omitted %q: %v", want, m.shutdownErr)
				}
			}
			if deferredRan || m.deferredStartup != nil || len(m.queue) != 0 {
				t.Fatalf("fatal retry advanced planning continuation: deferredRan=%v deferred=%v queue=%v",
					deferredRan, m.deferredStartup != nil, m.queue)
			}
			if m.nextQueuedTurn() != nil {
				t.Fatal("fatal retry scheduled queued work after planning failure")
			}
			select {
			case <-m.app.lifetime.Done():
			default:
				t.Fatal("fatal retry left the application lifetime open")
			}
			if err := child.AppendNote("info", "must not append"); err == nil {
				t.Fatal("fatal retry left the authoritative child writable")
			}
		})
	}
}

func TestRetryIntentExplicitAbandonFailureStopsAllWork(t *testing.T) {
	m := testModel(t)
	_, _ = bindRetryRecoveryChild(t, m, false, false)
	child := m.app.loop.Session
	m.app.lifetime = newTUILifetime()
	m.callTokens, m.ctxWindow = 90, 100
	m.queue = []string{"queued after explicit abandon"}
	deferredRan := false
	m.deferredStartup = func() tea.Cmd {
		deferredRan = true
		return func() tea.Msg { return noticeMsg{text: "must not run"} }
	}

	// /retry abandon owns the same synced abandonment record as a planning
	// refusal. If it cannot append, staying interactive would leave the process
	// one buffered event away from acting against a recovery-owned child.
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := cmdRetry(m, "abandon")
	if cmd == nil {
		t.Fatal("failed explicit retry abandonment returned no quit command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("failed explicit retry abandonment did not return tea.Quit")
	}
	if !m.quitting || m.shutdownErr == nil {
		t.Fatalf("failed explicit retry abandonment did not arm shutdown: quitting=%v err=%v", m.quitting, m.shutdownErr)
	}
	for _, want := range []string{"durable recovery handoff could not be cleared", "Restart Switchboard", "without duplicating provider or tool work"} {
		if !strings.Contains(m.shutdownErr.Error(), want) {
			t.Fatalf("fatal retry error omitted %q: %v", want, m.shutdownErr)
		}
	}
	if deferredRan || m.deferredStartup != nil || len(m.queue) != 0 {
		t.Fatalf("fatal retry advanced explicit-abandon continuation: deferredRan=%v deferred=%v queue=%v",
			deferredRan, m.deferredStartup != nil, m.queue)
	}
	if m.nextQueuedTurn() != nil {
		t.Fatal("fatal retry scheduled work after explicit abandonment failure")
	}
	select {
	case <-m.app.lifetime.Done():
	default:
		t.Fatal("fatal retry left the application lifetime open")
	}
}
