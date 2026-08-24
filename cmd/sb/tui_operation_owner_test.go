package main

import (
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

func operationTestModel(t *testing.T) *tuiModel {
	t.Helper()
	m := testModel(t)
	m.trackOperationTasks = true
	return m
}

func TestAbnormalTUIExitJoinsOperationMeterSettlement(t *testing.T) {
	m := operationTestModel(t)
	m.app.budget = &budgetState{}
	ctx, generation, sourceID, err := m.startOperation("metered audit")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	settled := make(chan error, 1)
	cmd := m.ownOperationCmd(generation, func() tea.Msg {
		finish, err := beginMeteredCall(
			m.app.budget, m.app.catalog, m.app.loop.Session, m.app.tier.Target,
			provider.Request{Messages: []provider.Message{provider.UserText("audit")}},
			session.UsagePurposeAudit,
		)
		if err != nil {
			settled <- err
			return noticeMsg{operation: generation, sourceID: sourceID, level: "error", text: err.Error()}
		}
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		err = finish(provider.Usage{InputTokens: 11, OutputTokens: 3}, nil)
		settled <- err
		return noticeMsg{operation: generation, sourceID: sourceID}
	})
	commandDone := make(chan tea.Msg, 1)
	go func() { commandDone <- cmd() }()
	select {
	case <-started:
	case err := <-settled:
		t.Fatalf("meter admission failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("operation did not admit its metered call")
	}

	exited := make(chan error, 1)
	go func() {
		exited <- runTUIProgram(tuiProgramFunc(func() (tea.Model, error) { return m, nil }), m)
	}()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("TUI exit did not cancel the operation")
	}
	select {
	case err := <-exited:
		t.Fatalf("runTUIProgram returned before meter settlement: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-exited:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runTUIProgram did not join the metered operation")
	}
	select {
	case err := <-settled:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("runTUIProgram returned before the meter callback finished")
	}
	select {
	case <-commandDone:
	default:
		t.Fatal("runTUIProgram returned before the operation command exited")
	}
	state := m.app.loop.Session.State()
	if state.Calls != 1 || state.Usage.InputTokens != 11 || state.Usage.OutputTokens != 3 {
		t.Fatalf("settled operation usage = calls:%d usage:%+v", state.Calls, state.Usage)
	}
	usages, err := session.ReadUsages(m.app.loop.Session.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(usages) != 1 || usages[0].Purpose != session.UsagePurposeAudit {
		t.Fatalf("operation usage records = %#v", usages)
	}
}

func TestAbnormalTUIExitClosesDroppedSessionSwapResult(t *testing.T) {
	m := operationTestModel(t)
	destination, err := m.app.store.Create(m.app.workspace, m.app.tier.Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	destinationID := destination.ID()
	ctx, generation, sourceID, err := m.startOperation("resume")
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	cmd := m.ownOperationCmd(generation, func() tea.Msg {
		return sessionSwapMsg{
			sess: destination, tier: m.app.tier, client: m.app.loop.Binding().Provider,
			operation: generation, sourceID: sourceID, release: func() { close(released) },
		}
	})
	if wrapped, ok := cmd().(operationTaskResultMsg); !ok || wrapped.msg == nil {
		t.Fatalf("operation result = %#v", wrapped)
	}
	if _, err := m.app.store.Open(destinationID); !errors.Is(err, session.ErrSessionLocked) {
		t.Fatalf("dropped destination before cleanup = %v, want session lock", err)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("operation cancelled before TUI exit: %v", err)
	}

	if err := runTUIProgram(tuiProgramFunc(func() (tea.Model, error) { return m, nil }), m); err != nil {
		t.Fatal(err)
	}
	select {
	case <-released:
	default:
		t.Fatal("dropped session swap retained its advisor barrier")
	}
	reopened, err := m.app.store.Open(destinationID)
	if err != nil {
		t.Fatalf("dropped destination remained locked: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAbnormalTUIExitClosesAndHidesDroppedRaceSetup(t *testing.T) {
	m := operationTestModel(t)
	ctx, generation, sourceID, err := m.startOperation("race setup")
	if err != nil {
		t.Fatal(err)
	}
	_ = ctx
	armA, err := m.app.store.CreateStaged(m.app.workspace, m.app.tier.Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	armB, err := m.app.store.CreateStaged(m.app.workspace, m.app.tier.Target.ID(), "test")
	if err != nil {
		_ = armA.CloseDiscardingStaged()
		t.Fatal(err)
	}
	stages := []struct{ id, path string }{
		{id: armA.ID(), path: armA.Path()},
		{id: armB.ID(), path: armB.Path()},
	}
	released := make(chan struct{})
	cmd := m.ownOperationCmd(generation, func() tea.Msg {
		return raceSetupMsg{
			operation: generation, sourceID: sourceID,
			arms:    [2]*raceArm{{sess: armA}, {sess: armB}},
			release: func() { close(released) },
		}
	})
	if _, ok := cmd().(operationTaskResultMsg); !ok {
		t.Fatal("race setup result was not owned")
	}
	if err := runTUIProgram(tuiProgramFunc(func() (tea.Model, error) { return m, nil }), m); err != nil {
		t.Fatal(err)
	}
	select {
	case <-released:
	default:
		t.Fatal("dropped race setup retained its advisor barrier")
	}
	for _, stage := range stages {
		assertRetainedUnpublishedStage(t, m.app.store, m.app.workspace, stage.id, stage.path)
	}
}

func TestOperationGenerationRetirementCleansUndeliveredResult(t *testing.T) {
	m := operationTestModel(t)
	destination, err := m.app.store.Create(m.app.workspace, m.app.tier.Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	id := destination.ID()
	_, generation, sourceID, err := m.startOperation("resume")
	if err != nil {
		t.Fatal(err)
	}
	cmd := m.ownOperationCmd(generation, func() tea.Msg {
		return sessionSwapMsg{sess: destination, operation: generation, sourceID: sourceID}
	})
	if _, ok := cmd().(operationTaskResultMsg); !ok {
		t.Fatal("session result was not owned")
	}
	if !m.finishOperation(generation, false) {
		t.Fatal("operation generation did not retire")
	}
	reopened, err := m.app.store.Open(id)
	if err != nil {
		t.Fatalf("retired generation retained its result lock: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAbnormalTUIExitCancelsUnstartedOperationCommand(t *testing.T) {
	m := operationTestModel(t)
	_, generation, _, err := m.startOperation("queued")
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	cmd := m.ownOperationCmd(generation, func() tea.Msg {
		ran = true
		return nil
	})
	if err := runTUIProgram(tuiProgramFunc(func() (tea.Model, error) { return m, nil }), m); err != nil {
		t.Fatal(err)
	}
	if msg := cmd(); msg != nil {
		t.Fatalf("cancelled queued command returned %#v", msg)
	}
	if ran {
		t.Fatal("queued operation command started after TUI exit")
	}
}

func TestAbnormalTUIExitCleansUnstartedSessionReprobeInputs(t *testing.T) {
	m := operationTestModel(t)
	destination, err := m.app.store.Create(m.app.workspace, m.app.tier.Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	id := destination.ID()
	_, generation, sourceID, err := m.startOperation("session provider refresh")
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	msg := sessionSwapMsg{
		sess: destination, operation: generation, sourceID: sourceID,
		release: func() { close(released) },
	}
	ran := false
	cmd := m.ownOperationCmdWithAbandon(generation, func() tea.Msg {
		ran = true
		return msg
	}, func() error {
		return cleanupDroppedOperationResult(msg)
	})

	if err := runTUIProgram(tuiProgramFunc(func() (tea.Model, error) { return m, nil }), m); err != nil {
		t.Fatal(err)
	}
	select {
	case <-released:
	default:
		t.Fatal("unstarted reprobe retained its advisor barrier")
	}
	if msg := cmd(); msg != nil {
		t.Fatalf("abandoned reprobe returned %#v", msg)
	}
	if ran {
		t.Fatal("abandoned reprobe ran after TUI exit")
	}
	reopened, err := m.app.store.Open(id)
	if err != nil {
		t.Fatalf("unstarted reprobe retained its destination lock: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}
