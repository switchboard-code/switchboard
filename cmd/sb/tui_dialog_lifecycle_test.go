package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/tools"
)

type droppingTUIMessageSender struct {
	sent chan tea.Msg
}

type exitBlockingRaceProvider struct {
	started  chan struct{}
	finished chan struct{}
}

func newExitBlockingRaceProvider() *exitBlockingRaceProvider {
	return &exitBlockingRaceProvider{started: make(chan struct{}, 1), finished: make(chan struct{}, 1)}
}

func (*exitBlockingRaceProvider) Name() string { return "exit-blocking-race" }

func (p *exitBlockingRaceProvider) Stream(ctx context.Context, _ provider.RouteTarget, _ provider.Request) (provider.EventStream, error) {
	return &exitBlockingRaceStream{ctx: ctx, started: p.started, finished: p.finished}, nil
}

func (*exitBlockingRaceProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}

func (*exitBlockingRaceProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true, Tools: provider.ToolsSerial}, nil
}

type exitBlockingRaceStream struct {
	ctx      context.Context
	started  chan<- struct{}
	finished chan<- struct{}
}

func (s *exitBlockingRaceStream) Next() (provider.Event, error) {
	s.started <- struct{}{}
	<-s.ctx.Done()
	s.finished <- struct{}{}
	return provider.Event{}, s.ctx.Err()
}

func (*exitBlockingRaceStream) Close() error { return nil }

func newDroppingTUIMessageSender() *droppingTUIMessageSender {
	return &droppingTUIMessageSender{sent: make(chan tea.Msg, 1)}
}

func (s *droppingTUIMessageSender) Send(msg tea.Msg) {
	s.sent <- msg
}

func waitDroppedTUIMessage(t *testing.T, sent <-chan tea.Msg) tea.Msg {
	t.Helper()
	select {
	case msg := <-sent:
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("modal request did not reach the dropping sender")
		return nil
	}
}

func TestStoppedTUILifetimeRefusesModalWaitersBeforeSend(t *testing.T) {
	lifetime := newTUILifetime()
	lifetime.stop()
	sender := &droppingTUIMessageSender{sent: make(chan tea.Msg, 2)}

	asker := &tuiAsker{p: sender, lifetimeDone: lifetime.Done()}
	if _, err := asker.Ask(context.Background(), permission.Request{Tool: "exec"}, permission.Outcome{Decision: permission.Ask}); !errors.Is(err, context.Canceled) {
		t.Fatalf("permission ask against stopped TUI = %v", err)
	}
	questioner := &tuiQuestioner{p: sender, lifetimeDone: lifetime.Done()}
	if _, err := questioner.AskUser(context.Background(), tools.Question{
		Question: "which?", Options: []tools.QuestionOption{{Label: "one"}, {Label: "two"}},
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("question against stopped TUI = %v", err)
	}
	select {
	case msg := <-sender.sent:
		t.Fatalf("stopped TUI accepted modal message %#v", msg)
	default:
	}
}

func TestAbnormalTUIExitReleasesPermissionRequestDroppedBeforeBroker(t *testing.T) {
	m := testModel(t)
	lifetime := newTUILifetime()
	m.app.lifetime = lifetime
	sender := newDroppingTUIMessageSender()
	asker := &tuiAsker{p: sender, lifetimeDone: lifetime.Done()}

	done := make(chan error, 1)
	go func() {
		_, err := asker.Ask(context.Background(), permission.Request{Tool: "exec"}, permission.Outcome{Decision: permission.Ask})
		done <- err
	}()
	if _, ok := waitDroppedTUIMessage(t, sender.sent).(askMsg); !ok {
		t.Fatal("permission waiter sent the wrong Bubble Tea message")
	}

	terminalErr := errors.New("terminal disconnected")
	if err := runTUIProgram(tuiProgramFunc(func() (tea.Model, error) { return m, terminalErr }), m); !errors.Is(err, terminalErr) {
		t.Fatalf("runTUIProgram error = %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("dropped permission waiter error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dropped permission waiter retained the turn goroutine after TUI exit")
	}
	if m.dlg != nil || len(m.dialogQueue) != 0 {
		t.Fatalf("dropped permission request fabricated broker state: current=%T queued=%d", m.dlg, len(m.dialogQueue))
	}

	// A relay can outlive the Program through MCP machinery. Once stopped it
	// must refuse before trying another Send to that dead program.
	if _, err := asker.Ask(context.Background(), permission.Request{Tool: "exec"}, permission.Outcome{Decision: permission.Ask}); !errors.Is(err, context.Canceled) {
		t.Fatalf("post-exit permission ask error = %v", err)
	}
	select {
	case msg := <-sender.sent:
		t.Fatalf("post-exit permission ask sent %#v", msg)
	default:
	}
}

func TestAbnormalTUIExitReleasesQuestionDroppedBeforeBroker(t *testing.T) {
	m := testModel(t)
	lifetime := newTUILifetime()
	m.app.lifetime = lifetime
	sender := newDroppingTUIMessageSender()
	questioner := &tuiQuestioner{p: sender, lifetimeDone: lifetime.Done()}

	done := make(chan error, 1)
	go func() {
		_, err := questioner.AskUser(context.Background(), tools.Question{
			Question: "which?", Options: []tools.QuestionOption{{Label: "one"}, {Label: "two"}},
		})
		done <- err
	}()
	if _, ok := waitDroppedTUIMessage(t, sender.sent).(questionMsg); !ok {
		t.Fatal("question waiter sent the wrong Bubble Tea message")
	}

	if err := runTUIProgram(tuiProgramFunc(func() (tea.Model, error) { return m, nil }), m); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("dropped question waiter error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dropped question waiter retained the turn goroutine after TUI exit")
	}
	if _, err := questioner.AskUser(context.Background(), tools.Question{
		Question: "again?", Options: []tools.QuestionOption{{Label: "one"}, {Label: "two"}},
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("post-exit question error = %v", err)
	}
	select {
	case msg := <-sender.sent:
		t.Fatalf("post-exit question sent %#v", msg)
	default:
	}
}

func TestAbnormalTUIExitReleasesTurnLockWhenPermissionMessageWasDropped(t *testing.T) {
	m := testModel(t)
	lifetime := newTUILifetime()
	m.app.lifetime = lifetime
	sender := newDroppingTUIMessageSender()
	m.app.loop.Asker = &tuiAsker{p: sender, lifetimeDone: lifetime.Done()}
	m.app.loop.Provider = &racedProvider{turns: []racedTurn{
		racedToolCall("write", `{"path":"must-not-exist.txt","content":"not written"}`),
	}}

	turnDone := make(chan error, 1)
	go func() { turnDone <- m.app.loop.Turn(context.Background(), "try a write") }()
	if _, ok := waitDroppedTUIMessage(t, sender.sent).(askMsg); !ok {
		t.Fatal("running turn sent the wrong Bubble Tea message")
	}
	if err := runTUIProgram(tuiProgramFunc(func() (tea.Model, error) { return m, nil }), m); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-turnDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("turn released with %v, want cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("abnormal TUI exit left the turn goroutine blocked")
	}

	// BindSession takes the loop's session ownership mutex. Completing it proves
	// the cancelled modal did not merely release Ask while stranding the turn.
	fresh, err := m.app.store.Create(t.TempDir(), m.app.loop.Target.ID(), "post-exit bind")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fresh.CloseDiscardingStaged() })
	bindDone := make(chan error, 1)
	go func() { bindDone <- m.app.loop.BindSession(fresh) }()
	select {
	case err := <-bindDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("abnormal TUI exit left the loop's session lock held")
	}
}

func TestModalBrokerPreservesArrivalOrderAndEveryWaiter(t *testing.T) {
	m := testModel(t)
	m.openDialog(&pickerDialog{title: "already open", items: []pickerItem{{id: "local", label: "local"}}})

	approval := make(chan permission.Response, 1)
	answer := make(chan tools.Answer, 1)
	m.Update(askMsg{
		req: permission.Request{Tool: "exec"}, out: permission.Outcome{Decision: permission.Ask},
		respond: approval,
	})
	m.Update(questionMsg{
		q:       tools.Question{Question: "which?", Options: []tools.QuestionOption{{Label: "one"}}},
		respond: answer,
	})
	m.Update(m.bindAsyncResult().bindPicker(pickerMsg{title: "last", items: []pickerItem{{id: "last", label: "last"}}}))

	if _, ok := m.dlg.(*pickerDialog); !ok || len(m.dialogQueue) != 3 {
		t.Fatalf("broker state = %T + %d queued, want picker + 3", m.dlg, len(m.dialogQueue))
	}
	if got := stripANSI(m.inputZoneView()); !strings.Contains(got, "3 more prompts waiting") {
		t.Fatalf("queued prompts are invisible:\n%s", got)
	}

	m.key(tea.KeyMsg{Type: tea.KeyEsc})
	if _, ok := m.dlg.(*permissionDialog); !ok {
		t.Fatalf("first queued dialog = %T, want permission", m.dlg)
	}
	m.key(tea.KeyMsg{Type: tea.KeyEsc})
	if got := <-approval; got.Approved {
		t.Fatalf("dismissed approval = %+v", got)
	}
	if _, ok := m.dlg.(*questionDialog); !ok {
		t.Fatalf("second queued dialog = %T, want question", m.dlg)
	}
	m.key(tea.KeyMsg{Type: tea.KeyEsc})
	if got := <-answer; !got.Declined {
		t.Fatalf("dismissed question = %+v, want declined", got)
	}
	if d, ok := m.dlg.(*pickerDialog); !ok || d.title != "last" {
		t.Fatalf("third queued dialog = %#v, want last picker", m.dlg)
	}
	m.key(tea.KeyMsg{Type: tea.KeyEsc})
	if m.dlg != nil || len(m.dialogQueue) != 0 {
		t.Fatalf("broker did not drain: current=%T queued=%d", m.dlg, len(m.dialogQueue))
	}
}

func TestAsyncPickerRequiresDeliberateSelectionBeforeEnter(t *testing.T) {
	m := testModel(t)
	picked := ""
	msg := m.bindAsyncResult().bindPicker(pickerMsg{
		title: "delayed choice",
		items: []pickerItem{
			{id: "first", label: "first"},
			{id: "second", label: "second"},
		},
		action: func(id string) tea.Cmd {
			picked = id
			return nil
		},
	})
	m.Update(msg)
	d, ok := m.dlg.(*pickerDialog)
	if !ok || d.sel != -1 {
		t.Fatalf("async picker = %#v, want an unselected picker", m.dlg)
	}

	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	if picked != "" || m.dlg != d {
		t.Fatalf("stray Enter picked %q and left dialog %T", picked, m.dlg)
	}
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeySpace},
		{Type: tea.KeyCtrlU},
		{Type: tea.KeyCtrlW},
	} {
		m.key(key)
		m.key(tea.KeyMsg{Type: tea.KeyEnter})
		if picked != "" || m.dlg != d || d.sel != -1 {
			t.Fatalf("empty filter shortcut %q armed selection %d and picked %q", key.String(), d.sel, picked)
		}
	}

	m.key(tea.KeyMsg{Type: tea.KeyDown})
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	if picked != "first" || m.dlg != nil {
		t.Fatalf("deliberate navigation picked %q and left dialog %T", picked, m.dlg)
	}
}

func TestCtrlCCancelsCurrentAndQueuedDialogsThroughTheirCleanup(t *testing.T) {
	m := testModel(t)
	ctx, cancel := context.WithCancel(context.Background())
	m.busy = true
	m.turnCtx = ctx
	m.turnCancel = cancel

	approval := make(chan permission.Response, 1)
	answer := make(chan tools.Answer, 1)
	pickerCancelled := false
	m.openDialog(newPermissionDialog(
		permission.Request{Tool: "exec"}, permission.Outcome{Decision: permission.Ask}, approval))
	m.openDialog(&pickerDialog{
		title: "cleanup", items: []pickerItem{{id: "one", label: "one"}},
		onCancel: func() tea.Cmd {
			pickerCancelled = true
			return nil
		},
	})
	m.openDialog(newQuestionDialog(
		tools.Question{Question: "which?", Options: []tools.QuestionOption{{Label: "one"}}}, answer))

	m.key(tea.KeyMsg{Type: tea.KeyCtrlC})

	if got := <-approval; got.Approved {
		t.Fatalf("ctrl-c approved: %+v", got)
	}
	if got := <-answer; !got.Declined {
		t.Fatalf("ctrl-c question answer = %+v, want declined", got)
	}
	if !pickerCancelled {
		t.Fatal("ctrl-c skipped the queued picker's onCancel cleanup")
	}
	if m.dlg != nil || len(m.dialogQueue) != 0 || m.dialogToken != nil {
		t.Fatalf("ctrl-c left broker state: current=%T queued=%d token=%v", m.dlg, len(m.dialogQueue), m.dialogToken != nil)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("ctrl-c dismissed prompts but did not interrupt their running turn")
	}
}

func TestCancelledAsyncWaiterIsRemovedFromModalQueue(t *testing.T) {
	m := testModel(t)
	m.openDialog(&pickerDialog{title: "current", items: []pickerItem{{id: "one", label: "one"}}})
	token := &dialogToken{}
	approval := make(chan permission.Response, 1)
	m.openDialogFor(newPermissionDialog(
		permission.Request{Tool: "exec"}, permission.Outcome{Decision: permission.Ask}, approval), token)

	m.Update(cancelDialogMsg{token: token})

	if len(m.dialogQueue) != 0 {
		t.Fatalf("cancelled waiter left %d queued dialogs", len(m.dialogQueue))
	}
	if d, ok := m.dlg.(*pickerDialog); !ok || d.title != "current" {
		t.Fatalf("cancelling a waiter disturbed current dialog: %#v", m.dlg)
	}
	if got := <-approval; got.Approved {
		t.Fatalf("cancelled waiter approved: %+v", got)
	}
}

func TestAsyncCancellationTargetsOnlyItsOwnQueuedDialog(t *testing.T) {
	m := testModel(t)
	m.openDialog(&pickerDialog{title: "current", items: []pickerItem{{id: "one", label: "one"}}})
	firstToken, secondToken := &dialogToken{}, &dialogToken{}
	if firstToken == secondToken {
		t.Fatal("distinct dialog tokens compared equal")
	}
	first, second := make(chan permission.Response, 1), make(chan permission.Response, 1)
	m.openDialogFor(newPermissionDialog(
		permission.Request{Tool: "first"}, permission.Outcome{Decision: permission.Ask}, first), firstToken)
	m.openDialogFor(newPermissionDialog(
		permission.Request{Tool: "second"}, permission.Outcome{Decision: permission.Ask}, second), secondToken)

	m.Update(cancelDialogMsg{token: secondToken})

	if len(m.dialogQueue) != 1 || m.dialogQueue[0].token != firstToken {
		t.Fatalf("cancelling second left queue %#v, want only first", m.dialogQueue)
	}
	if got := <-second; got.Approved {
		t.Fatalf("cancelled second waiter approved: %+v", got)
	}
	select {
	case got := <-first:
		t.Fatalf("cancelling second resolved first waiter: %+v", got)
	default:
	}
}

func TestCancelDialogsAlsoDrainsAModalOpenedByCleanup(t *testing.T) {
	m := testModel(t)
	firstCancelled, secondCancelled := false, false
	second := &pickerDialog{
		title: "second", items: []pickerItem{{id: "two", label: "two"}},
		onCancel: func() tea.Cmd {
			secondCancelled = true
			return nil
		},
	}
	m.openDialog(&pickerDialog{
		title: "first", items: []pickerItem{{id: "one", label: "one"}},
		onCancel: func() tea.Cmd {
			firstCancelled = true
			m.openDialog(second)
			return nil
		},
	})

	m.cancelDialogs()

	if !firstCancelled || !secondCancelled {
		t.Fatalf("recursive cleanup = first:%v second:%v", firstCancelled, secondCancelled)
	}
	if m.dlg != nil || len(m.dialogQueue) != 0 {
		t.Fatalf("cleanup-opened modal survived: current=%T queued=%d", m.dlg, len(m.dialogQueue))
	}
}

func TestRaceDialogCancelRecordsDropAndReleasesTheSession(t *testing.T) {
	m := raceModel(t)
	armA, err := assembleRaceArm(m.app, m.app.config.Tiers[0], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armB, err := assembleRaceArm(m.app, m.app.config.Tiers[1], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armA.status, armB.status = "completed", "completed"
	run := &raceRun{typed: "compare", arms: [2]*raceArm{armA, armB}}
	run.labels = [2]string{"a · t1", "b · t2"}
	m.race = run
	m.busy = true
	original := m.app.loop.Session

	if cmd := newRaceDialog(m, run).cancel(); cmd != nil {
		t.Fatal("dropping an unqueued race unexpectedly started another command")
	}

	if m.race != nil || m.busy {
		t.Fatalf("race cancellation left ownership behind: race=%v busy=%v", m.race != nil, m.busy)
	}
	if m.app.loop.Session != original {
		t.Fatal("dropping both race arms replaced the source session")
	}
	if len(m.raceLog) == 0 || !strings.Contains(m.raceLog[len(m.raceLog)-1], "abandoned") {
		t.Fatalf("race cancellation was not recorded: %#v", m.raceLog)
	}
}

func TestExitCleanupCannotLaunchAQueuedTurnFromRaceCancellation(t *testing.T) {
	m := raceModel(t)
	armA, err := assembleRaceArm(m.app, m.app.config.Tiers[0], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armB, err := assembleRaceArm(m.app, m.app.config.Tiers[1], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armA.status, armB.status = "completed", "completed"
	run := &raceRun{typed: "compare", arms: [2]*raceArm{armA, armB}}
	run.labels = [2]string{"a · t1", "b · t2"}
	m.race = run
	m.busy = true
	m.queue = []string{"must never start during teardown"}
	m.openDialog(&pickerDialog{title: "palette", items: []pickerItem{{id: "exit", label: "exit"}}})
	m.openDialog(newRaceDialog(m, run))

	if cmd := cmdExit(m, ""); cmd == nil {
		t.Fatal("exit returned no quit sequence")
	}
	if !m.quitting || m.turnPlanning || m.operationActive {
		t.Fatalf("exit cleanup started work: quitting=%v planning=%v operation=%v", m.quitting, m.turnPlanning, m.operationActive)
	}
	if len(m.queue) != 0 || m.race != nil || m.busy || m.dlg != nil || len(m.dialogQueue) != 0 {
		t.Fatalf("exit cleanup left ownership: queue=%d race=%v busy=%v dialog=%T pending=%d",
			len(m.queue), m.race != nil, m.busy, m.dlg, len(m.dialogQueue))
	}
}

func TestAbnormalTUIExitCancelsJoinsAndSettlesActiveRaceArms(t *testing.T) {
	m := raceModel(t)
	m.app.lifetime = newTUILifetime()
	providerA := newExitBlockingRaceProvider()
	providerB := newExitBlockingRaceProvider()
	armA, err := assembleRaceArm(m.app, m.app.config.Tiers[0], providerA, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armB, err := assembleRaceArm(m.app, m.app.config.Tiers[1], providerB, agent.NopObserver{})
	if err != nil {
		_ = armA.sess.CloseDiscardingStaged()
		t.Fatal(err)
	}
	released := make(chan struct{})
	run := &raceRun{
		typed:          "compare before terminal loss",
		arms:           [2]*raceArm{armA, armB},
		before:         m.app.loop.Session.State(),
		send:           func(tea.Msg) {},
		releaseAdvisor: func() { close(released) },
	}
	run.labels = [2]string{"a · t1", "b · t2"}
	m.race = run
	m.busy = true
	m.launchRace(run, provider.UserText("compare before terminal loss"))
	waitExitRaceSignal(t, providerA.started, "branch A did not reach its provider")
	waitExitRaceSignal(t, providerB.started, "branch B did not reach its provider")

	terminalErr := errors.New("terminal disconnected")
	if err := runTUIProgram(tuiProgramFunc(func() (tea.Model, error) { return m, terminalErr }), m); !errors.Is(err, terminalErr) {
		t.Fatalf("runTUIProgram error = %v", err)
	}
	waitExitRaceSignal(t, providerA.finished, "branch A survived TUI exit")
	waitExitRaceSignal(t, providerB.finished, "branch B survived TUI exit")
	waitExitRaceSignal(t, released, "race teardown retained the advisor ledger barrier")
	if m.race != nil || m.busy {
		t.Fatalf("race teardown retained ownership: race=%v busy=%v", m.race != nil, m.busy)
	}
	if armA.status != "cancelled" || armB.status != "cancelled" {
		t.Fatalf("race teardown statuses = %q, %q", armA.status, armB.status)
	}
	if len(m.raceLog) == 0 || !strings.Contains(m.raceLog[len(m.raceLog)-1], "abandoned") {
		t.Fatalf("abnormal exit did not settle an abandoned verdict: %#v", m.raceLog)
	}
	for _, arm := range []*raceArm{armA, armB} {
		reopened, err := m.app.store.Open(arm.sess.ID())
		if err != nil {
			t.Fatalf("settled race branch %s remained locked or hidden: %v", arm.sess.ID(), err)
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func waitExitRaceSignal(t *testing.T, signal <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal(failure)
	}
}

func TestRaceDialogAsyncEnterDefaultsToNeither(t *testing.T) {
	m := raceModel(t)
	armA, err := assembleRaceArm(m.app, m.app.config.Tiers[0], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armB, err := assembleRaceArm(m.app, m.app.config.Tiers[1], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armA.status, armB.status = "completed", "completed"
	run := &raceRun{typed: "compare", arms: [2]*raceArm{armA, armB}}
	run.labels = [2]string{"a · t1", "b · t2"}
	m.race = run
	m.busy = true
	original := m.app.loop.Session
	d := newRaceDialog(m, run)
	if got := d.ids[d.sel]; got != "drop" {
		t.Fatalf("initial race choice = %q, want the non-adopting drop choice", got)
	}
	m.openDialog(d) // the verdict arrives later, while the keyboard is live

	for _, shortcut := range []rune{'a', 'y', '1', 'k'} {
		if cmd := m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{shortcut}}); cmd != nil {
			t.Fatalf("shortcut %q unexpectedly returned a command", shortcut)
		}
		if m.dlg != d || d.ids[d.sel] != "drop" || m.app.loop.Session != original {
			t.Fatalf("shortcut %q bypassed deliberate selection", shortcut)
		}
	}

	if cmd := m.key(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatal("dropping an unqueued race unexpectedly started another command")
	}
	if m.dlg != nil || m.race != nil || m.busy {
		t.Fatalf("safe Enter left ownership behind: dialog=%T race=%v busy=%v", m.dlg, m.race != nil, m.busy)
	}
	if m.app.loop.Session != original {
		t.Fatal("safe Enter adopted a race arm instead of retaining the source session")
	}
	if len(m.raceLog) == 0 || !strings.Contains(m.raceLog[len(m.raceLog)-1], "abandoned") {
		t.Fatalf("safe Enter was not recorded as abandoned: %#v", m.raceLog)
	}
}

func TestRaceVerdictSwapDoesNotCancelTheDialogBeingResolved(t *testing.T) {
	m := raceModel(t)
	armA, err := assembleRaceArm(m.app, m.app.config.Tiers[0], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armB, err := assembleRaceArm(m.app, m.app.config.Tiers[1], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armA.status, armB.status = "completed", "completed"
	run := &raceRun{typed: "compare", arms: [2]*raceArm{armA, armB}}
	run.labels = [2]string{"a · t1", "b · t2"}
	m.race = run
	m.busy = true
	d := newRaceDialog(m, run)
	m.openDialog(d)
	queuedCancelled := false
	m.openDialog(&pickerDialog{
		title: "old-session picker", items: []pickerItem{{id: "old", label: "old"}},
		onCancel: func() tea.Cmd {
			queuedCancelled = true
			return nil
		},
	})

	// The race dialog starts on neither. Move deliberately to branch B, then
	// resolve through the model's real key path. finishRace swaps sessions
	// synchronously, before key() has called completeDialog; session cleanup
	// must not interpret that still-active dialog as a second cancellation.
	m.key(tea.KeyMsg{Type: tea.KeyUp})
	m.key(tea.KeyMsg{Type: tea.KeyUp})
	if got := d.ids[d.sel]; got != "b" {
		t.Fatalf("selected %q, want branch b", got)
	}
	before := len(m.raceLog)
	m.key(tea.KeyMsg{Type: tea.KeyEnter})

	if got := len(m.raceLog) - before; got != 1 {
		t.Fatalf("one verdict appended %d race log entries: %#v", got, m.raceLog[before:])
	}
	if !queuedCancelled {
		t.Fatal("the winner swap did not cancel a picker queued for the old session")
	}
	if m.app.loop.Session != armB.sess {
		t.Fatal("branch B verdict did not leave branch B adopted")
	}
	if m.dlg != nil || m.race != nil || m.busy {
		t.Fatalf("verdict left ownership behind: dialog=%T race=%v busy=%v", m.dlg, m.race != nil, m.busy)
	}
}

func TestTextAndSecretDialogCancellationNeverSubmits(t *testing.T) {
	textSubmitted, secretSubmitted := false, false
	text := newTextDialog(textPromptMsg{
		title: "text", submit: func(string) tea.Cmd {
			textSubmitted = true
			return nil
		},
	})
	secret := newSecretDialog(
		credential.Ref{Provider: "test", Account: "account"}, "test store",
		func(string) tea.Cmd {
			secretSubmitted = true
			return nil
		})
	for _, d := range []dialog{text, secret} {
		d.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("do not submit")}, darkTheme())
		if cmd := d.cancel(); cmd != nil {
			t.Fatalf("%T cancellation returned an unexpected command", d)
		}
	}
	if textSubmitted || secretSubmitted {
		t.Fatalf("cancellation submitted input: text=%v secret=%v", textSubmitted, secretSubmitted)
	}
	if text.input.Value() != "" || secret.input.Value() != "" {
		t.Fatalf("cancellation retained input: text=%q secret=%q", text.input.Value(), secret.input.Value())
	}
}
