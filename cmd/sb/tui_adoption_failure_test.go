package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

func stagedSwapSession(t *testing.T, m *tuiModel) *session.Session {
	t.Helper()
	sess, err := m.app.store.CreateStaged(
		m.app.loop.Session.State().Workspace,
		m.app.tier.Target.ID(),
		m.app.catalog.Revision,
	)
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// poisonSessionRollback gives BindSession real reconciliation work, then
// closes the WAL. A rollback to this source deterministically fails instead of
// succeeding from its still-readable in-memory state.
func poisonSessionRollback(source *session.Session) error {
	if err := source.AppendMessage(provider.UserText("unfinished source turn")); err != nil {
		return err
	}
	if err := source.AppendMessage(provider.Message{
		Role: provider.RoleAssistant,
		Content: []provider.Block{provider.ToolUse{
			ID: "rollback-fault-call", Name: "read",
		}},
	}); err != nil {
		return err
	}
	return source.Close()
}

func assertFatalAdoptionStop(t *testing.T, m *tuiModel, source, child, sibling *session.Session, cmd tea.Cmd, want string) {
	t.Helper()
	if cmd == nil {
		t.Fatal("fatal adoption failure returned no quit command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("fatal adoption failure did not return tea.Quit")
	}
	if !m.quitting || m.shutdownErr == nil || !strings.Contains(m.shutdownErr.Error(), want) {
		t.Fatalf("fatal adoption state: quitting=%v err=%v, want %q", m.quitting, m.shutdownErr, want)
	}
	if len(m.queue) != 1 || m.queue[0] != "queued after adoption" {
		t.Fatalf("fatal adoption consumed queued work: %#v", m.queue)
	}
	if next := m.nextQueuedTurn(); next != nil {
		t.Fatal("fatal adoption scheduled queued work after shutdown")
	}
	// Quit is a command, so one already-buffered input event can reach Update
	// before its QuitMsg. The shutdown gate must reject that event too.
	m.ta.SetValue("new work after shutdown")
	_, updateCmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if updateCmd == nil {
		t.Fatal("shutdown input did not keep the quit command armed")
	}
	if _, ok := updateCmd().(tea.QuitMsg); !ok {
		t.Fatal("shutdown input was handled instead of returning tea.Quit")
	}
	if len(m.queue) != 1 {
		t.Fatalf("shutdown input changed the queued work: %#v", m.queue)
	}
	if err := child.AppendNote("info", "must not append"); err == nil {
		t.Fatal("failed-adoption child remained writable")
	}
	if err := source.AppendNote("info", "must not append"); err == nil {
		t.Fatal("failed-adoption source remained writable")
	}
	assertRetainedUnpublishedStage(t, m.app.store, m.app.workspace, child.ID(), child.Path())
	if sibling != nil {
		if err := sibling.AppendNote("info", "must not append"); err == nil {
			t.Fatal("unpublished race sibling remained writable")
		}
		assertRetainedUnpublishedStage(t, m.app.store, m.app.workspace, sibling.ID(), sibling.Path())
	}
}

func TestSessionAdoptionRollbackFailureStopsAllQueuedWork(t *testing.T) {
	for _, variant := range []string{"ordinary adoption", "compaction", "race winner"} {
		t.Run(variant, func(t *testing.T) {
			m := testModel(t)
			source := m.app.loop.Session
			child := stagedSwapSession(t, m)
			var sibling *session.Session
			msg := sessionSwapMsg{
				sess: child, tier: m.app.tier, client: m.app.loop.Binding().Provider,
				publishDurably: func(*session.Session) (session.PublicationOutcome, error) {
					return session.PublicationOutcome{}, errors.Join(
						errors.New("injected invisible publication failure"),
						poisonSessionRollback(source),
					)
				},
			}
			switch variant {
			case "ordinary adoption":
				msg.fresh = true
			case "compaction":
				msg.keepFold = true
			case "race winner":
				sibling = stagedSwapSession(t, m)
				msg.keepFold = true
				msg.publishAfter = sibling
			}
			m.queue = []string{"queued after adoption"}

			cmd := m.onSessionSwap(msg)
			assertFatalAdoptionStop(t, m, source, child, sibling, cmd,
				"session publication failed and source-session rollback also failed")
		})
	}
}

func TestRetryRollbackFailureStopsAllQueuedWork(t *testing.T) {
	t.Run("prepared publication transaction", func(t *testing.T) {
		m := testModel(t)
		appendTurn(t, m, "question", "answer")
		swap := mustStageRetry(t, m)
		source := swap.retry.source
		swap.publishDurably = func(*session.Session) (session.PublicationOutcome, error) {
			return session.PublicationOutcome{}, errors.Join(
				errors.New("injected retry publication failure"),
				poisonSessionRollback(source),
			)
		}
		m.queue = []string{"queued after adoption"}

		cmd := m.onSessionSwap(swap)
		assertFatalAdoptionStop(t, m, source, swap.sess, nil, cmd,
			"retry restore failed and source-session rollback also failed")
	})

	t.Run("restore before publication", func(t *testing.T) {
		m, path, _, _ := retryFileModel(t)
		swap := mustStageRetry(t, m)
		source := swap.retry.source
		if err := poisonSessionRollback(source); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("external edit after preparation"), 0o644); err != nil {
			t.Fatal(err)
		}
		m.queue = []string{"queued after adoption"}

		cmd := m.onSessionSwap(swap)
		assertFatalAdoptionStop(t, m, source, swap.sess, nil, cmd,
			"retry restore failed and source-session rollback also failed")
		if got, err := os.ReadFile(path); err != nil || string(got) != "external edit after preparation" {
			t.Fatalf("failed retry changed the external post-image: %q, %v", got, err)
		}
	})

	// Keep the nil-undo compatibility path fail-closed too. Normal v1.21 retries
	// prepare an empty durable transaction when no file checkpoint exists, but a
	// replayed/legacy caller can still reach this explicit publication branch.
	t.Run("direct retry publication", func(t *testing.T) {
		m := testModel(t)
		source := m.app.loop.Session
		child := stagedSwapSession(t, m)
		msg := sessionSwapMsg{
			sess: child, tier: m.app.tier, client: m.app.loop.Binding().Provider,
			retry: &retryAdoption{source: source, destination: m.app.tier.ID},
			publishDurably: func(*session.Session) (session.PublicationOutcome, error) {
				return session.PublicationOutcome{}, errors.Join(
					errors.New("injected direct retry publication failure"),
					poisonSessionRollback(source),
				)
			},
		}
		m.queue = []string{"queued after adoption"}

		cmd := m.onSessionSwap(msg)
		assertFatalAdoptionStop(t, m, source, child, nil, cmd,
			"retry publication failed and source-session rollback also failed")
	})
}
