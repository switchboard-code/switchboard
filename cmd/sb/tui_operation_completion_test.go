package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/watch"
)

func TestOwnedNilOperationResultDrainsQueuedPromptExactlyOnce(t *testing.T) {
	m := operationTestModel(t)
	_, generation, _, err := m.startOperation("nil completion")
	if err != nil {
		t.Fatal(err)
	}
	if queued := m.enqueue("queued after nil completion", ""); queued != nil {
		t.Fatal("prompt started while operation owned the lane")
	}
	command := m.ownOperationCmd(generation, func() tea.Msg { return nil })
	wrapped, ok := command().(operationTaskResultMsg)
	if !ok || wrapped.msg != nil {
		t.Fatalf("owned nil result = %#v", wrapped)
	}

	_, next := m.Update(wrapped)
	if next == nil || len(m.queue) != 0 || m.operationActive || !m.turnPlanning || !m.busy {
		t.Fatalf("nil completion did not launch one queued prompt: next=%v queue=%#v operation=%v planning=%v busy=%v",
			next != nil, m.queue, m.operationActive, m.turnPlanning, m.busy)
	}
	planned, ok := next().(turnPlanMsg)
	if !ok || !strings.Contains(planned.prompt, "queued after nil completion") {
		t.Fatalf("queued launch = %#v", planned)
	}
	if _, duplicate := m.Update(wrapped); duplicate != nil {
		t.Fatal("duplicate nil completion launched work twice")
	}
	m.finishPlanning()
}

func TestOwnedNilOperationResultWithStaleSourceCannotDrainQueue(t *testing.T) {
	m := operationTestModel(t)
	_, generation, _, err := m.startOperation("stale nil completion")
	if err != nil {
		t.Fatal(err)
	}
	if queued := m.enqueue("must remain queued", ""); queued != nil {
		t.Fatal("prompt started while operation owned the lane")
	}
	command := m.ownOperationCmd(generation, func() tea.Msg { return nil })
	wrapped, ok := command().(operationTaskResultMsg)
	if !ok {
		t.Fatalf("owned result = %#v", wrapped)
	}
	// Model a session-ownership stamp changing before delivery while the old
	// owner capsule is still the one presented to Update.
	m.operationSourceID += "-stale"
	_, next := m.Update(wrapped)
	if next != nil || len(m.queue) != 1 || m.operationActive || m.busy || m.turnPlanning {
		t.Fatalf("stale nil completion crossed ownership: next=%v queue=%#v operation=%v busy=%v planning=%v",
			next != nil, m.queue, m.operationActive, m.busy, m.turnPlanning)
	}
}

func prepareInvalidatedTurnEndWatch(t *testing.T, m *tuiModel) (watchRunTicket, tea.Cmd) {
	t.Helper()
	m.app.undo = checkpoint.NewRecorder()
	m.app.undo.Begin("edited turn")
	m.app.undo.Record(filepath.Join(t.TempDir(), "new.go"))
	m.app.watchSt.arm(watch.New("go test ./...", m.app.workspace))
	m.app.watchSt.beginTurn(context.Background(), currentSessionID(m))

	var ticket watchRunTicket
	cmd := m.startTurnEndWatchWithHook(false, func(prepared watchRunTicket) {
		ticket = prepared
		m.app.watchSt.disarm()
	})
	if ticket.invocation == 0 || ticket.sequence == nil {
		t.Fatal("turn-end watch did not prepare a ticket")
	}
	select {
	case <-ticket.sequence.done:
	default:
		t.Fatal("invalidated watch bind retained its FIFO claim")
	}
	m.app.watchSt.mu.Lock()
	pending := len(m.app.watchSt.pendingCancels)
	tail := m.app.watchSt.runTail
	m.app.watchSt.mu.Unlock()
	if pending != 0 || tail != nil {
		t.Fatalf("invalidated watch bind retained lifecycle state: pending=%d tail=%v", pending, tail != nil)
	}
	// Every cleanup path may defensively finish the same ticket. The sequence's
	// once must make repeated retirement harmless rather than close twice.
	m.app.watchSt.finish(ticket)
	m.app.watchSt.finish(ticket)
	return ticket, cmd
}

func TestInvalidatedTurnEndWatchBindContinuesQueuedPrompt(t *testing.T) {
	m := testModel(t)
	m.queue = []string{"queued after watch"}
	_, next := prepareInvalidatedTurnEndWatch(t, m)
	if next == nil || len(m.queue) != 0 || m.operationActive || !m.turnPlanning || !m.busy {
		t.Fatalf("invalidated watch bind stranded queue: next=%v queue=%#v operation=%v planning=%v busy=%v",
			next != nil, m.queue, m.operationActive, m.turnPlanning, m.busy)
	}
	planned, ok := next().(turnPlanMsg)
	if !ok || !strings.Contains(planned.prompt, "queued after watch") {
		t.Fatalf("queued continuation = %#v", planned)
	}
	m.finishPlanning()
}

func TestInvalidatedTurnEndWatchBindContinuesDeferredStartup(t *testing.T) {
	m := testModel(t)
	called := 0
	m.deferredStartup = func() tea.Cmd {
		called++
		return noticeCmd("", "deferred continued")
	}
	_, next := prepareInvalidatedTurnEndWatch(t, m)
	if next == nil || called != 1 || m.deferredStartup != nil || m.operationActive || m.busy {
		t.Fatalf("invalidated watch bind stranded deferred startup: next=%v called=%d deferred=%v operation=%v busy=%v",
			next != nil, called, m.deferredStartup != nil, m.operationActive, m.busy)
	}
	if msg, ok := next().(noticeMsg); !ok || msg.text != "deferred continued" {
		t.Fatalf("deferred continuation = %#v", msg)
	}
}

func TestInvalidatedTurnEndWatchBindContinuesAutoCompact(t *testing.T) {
	m := testModel(t)
	appendTurn(t, m, "question", "answer")
	m.app.config.CompactAuto = true
	m.app.config.CompactAtPercent = 85
	m.ctxWindow, m.callTokens = 100, 90
	_, next := prepareInvalidatedTurnEndWatch(t, m)
	if next == nil || !m.operationActive || m.operationName != "compact" || !m.busy || m.turnPlanning {
		t.Fatalf("invalidated watch bind stranded auto-compact: next=%v operation=%v name=%q busy=%v planning=%v",
			next != nil, m.operationActive, m.operationName, m.busy, m.turnPlanning)
	}
	m.finishOperation(m.operationGeneration, false)
}
