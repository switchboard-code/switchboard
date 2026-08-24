//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/watch"
)

// The round boundary is the whole contract: the verifier runs only when the
// turn has captured files it has not checked, and what it returns rides the
// injection seam as a user-role message.
func TestWatchRoundRunsOnlyAfterCapturedEdits(t *testing.T) {
	m := testModel(t)
	rec := checkpoint.NewRecorder()
	m.app.undo = rec
	m.app.watchSt.arm(watch.New("echo '--- FAIL: TestAlpha'; exit 1", t.TempDir()))
	m.app.watchSt.beginTurn(context.Background(), currentSessionID(m))

	if msgs := m.app.watchRound(); len(msgs) != 0 {
		t.Fatal("the verifier ran with nothing captured")
	}

	rec.Begin("a turn")
	f := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec.Record(f)

	msgs := m.app.watchRound()
	if len(msgs) != 1 {
		t.Fatalf("want one injected message, got %d", len(msgs))
	}
	text := ""
	for _, b := range msgs[0].Content {
		if tb, ok := b.(provider.Text); ok {
			text = tb.Text
		}
	}
	if msgs[0].Role != provider.RoleUser || !strings.Contains(text, "--- FAIL: TestAlpha") {
		t.Fatalf("the injection is not a user-role failure report: %+v", msgs[0])
	}

	// The same evidence does not run the verifier twice.
	if msgs := m.app.watchRound(); len(msgs) != 0 {
		t.Fatal("the verifier ran again with no new captures")
	}
}

func TestWatchExecutesRawSecretCommandButOnlyReportsRedactedCopies(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "runner-input")
	raw := "printf %s " + shellSingleQuote(testGitHubToken) + " > " + shellSingleQuote(marker) +
		"; echo '--- FAIL: WatchRawCommand'; exit 1"

	m := testModel(t)
	recorder := checkpoint.NewRecorder()
	m.app.undo = recorder
	cmdWatch(m, raw)
	armed := m.app.watchSt.armed()
	if armed == nil || armed.Command() != raw {
		t.Fatal("watch did not retain the exact command for execution")
	}
	m.app.watchSt.beginTurn(context.Background(), currentSessionID(m))

	recorder.Begin("secret watch command")
	changed := filepath.Join(dir, "changed.txt")
	if err := os.WriteFile(changed, []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder.Record(changed)

	messages := m.app.watchRound()
	if len(messages) != 1 {
		t.Fatalf("watch injected %d messages, want 1", len(messages))
	}
	runnerInput, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("raw watch command did not reach the runner: %v", err)
	}
	if string(runnerInput) != testGitHubToken {
		t.Fatal("runner received a redacted or otherwise changed watch command")
	}

	var injected strings.Builder
	for _, block := range messages[0].Content {
		if text, ok := block.(provider.Text); ok {
			injected.WriteString(text.Text)
		}
	}
	if strings.Contains(injected.String(), testGitHubToken) {
		t.Fatal("the model-facing watch report retained the raw credential")
	}
	if !strings.Contains(injected.String(), "[redacted: a GitHub token]") {
		t.Fatalf("model-facing report did not identify the redaction:\n%s", injected.String())
	}

	visible := strings.Join(m.tr.flat, "\n") + "\n" + m.View()
	if strings.Contains(visible, testGitHubToken) {
		t.Fatal("the watch arm notice retained the raw credential")
	}
	durable, err := os.ReadFile(m.app.loop.Session.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(durable), testGitHubToken) {
		t.Fatal("the session log retained the raw watch credential")
	}
}

func TestSessionBoundaryDiscardsAStagedWatchObservationBeforeBaselineCommit(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	w := watch.New("echo '--- FAIL: TestOld'; exit 1", t.TempDir())
	m.app.watchSt.arm(w)
	m.app.watchSt.beginTurn(context.Background(), currentSessionID(m))
	ticket, _, ok := m.app.watchSt.due(1)
	if !ok {
		t.Fatal("watch run was not due")
	}

	observation := w.Observe(context.Background())
	m.app.watchSt.sessionBoundary("replacement-session", false)
	if _, committed := m.app.watchSt.commit(ticket, observation); committed {
		t.Fatal("an observation staged by the prior session advanced the baseline")
	}

	rep := w.Run(context.Background())
	if !rep.FirstRun || len(rep.New) != 1 || rep.Persisting != 0 {
		t.Fatalf("the rejected observation contaminated the new baseline: %+v", rep)
	}
}

func TestWatchSerializesSlowTurnEndBeforeTheNextRound(t *testing.T) {
	dir := t.TempDir()
	firstStarted := filepath.Join(dir, "first-started")
	secondStarted := filepath.Join(dir, "second-started")
	releaseFirst := filepath.Join(dir, "release-first")
	command := "if [ ! -e " + shellSingleQuote(firstStarted) + " ]; then " +
		": > " + shellSingleQuote(firstStarted) + "; " +
		"while [ ! -e " + shellSingleQuote(releaseFirst) + " ]; do sleep 0.01; done; " +
		"echo '--- FAIL: FirstInvocation'; exit 1; " +
		"else : > " + shellSingleQuote(secondStarted) + "; " +
		"echo '--- FAIL: SecondInvocation'; exit 1; fi"

	ws := &watchState{}
	ws.arm(watch.New(command, dir))
	ws.beginTurn(context.Background(), "session")
	old, _, ok := ws.due(1)
	if !ok {
		t.Fatal("turn-end watch run was not due")
	}
	oldCtx, ok := ws.backgroundContext(old)
	if !ok {
		t.Fatal("turn-end watch did not get a detached context")
	}

	type result struct {
		report watch.Report
		ok     bool
	}
	run := func(ticket watchRunTicket, ctx context.Context) <-chan result {
		ch := make(chan result, 1)
		go func() {
			defer ws.finish(ticket)
			report, current := ws.execute(ticket, ctx)
			ch <- result{report: report, ok: current}
		}()
		return ch
	}
	oldResult := run(old, oldCtx)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(firstStarted); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("first verifier invocation did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// This is the production interleaving: the detached verifier from the old
	// turn is still running when a captured edit reaches the new turn's round
	// boundary. The newer invocation must queue instead of observing or
	// committing against the shared Watch concurrently.
	newTurnCtx, cancelNewTurn := context.WithCancel(context.Background())
	defer cancelNewTurn()
	ws.beginTurn(newTurnCtx, "session")
	newer, newerCtx, ok := ws.due(1)
	if !ok {
		t.Fatal("next-round watch run was not due")
	}
	newerResult := run(newer, newerCtx)
	select {
	case <-newerResult:
		t.Fatal("newer verifier invocation overtook the slow turn-end run")
	case <-time.After(150 * time.Millisecond):
	}
	if _, err := os.Stat(secondStarted); !os.IsNotExist(err) {
		t.Fatalf("newer verifier started before its predecessor finished: %v", err)
	}

	if err := os.WriteFile(releaseFirst, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantResult := func(name string, ch <-chan result) result {
		t.Helper()
		select {
		case got := <-ch:
			if !got.ok {
				t.Fatalf("%s invocation was discarded", name)
			}
			return got
		case <-time.After(5 * time.Second):
			t.Fatalf("%s invocation did not finish", name)
			return result{}
		}
	}
	first := wantResult("first", oldResult)
	second := wantResult("second", newerResult)
	if len(first.report.New) != 1 || !strings.Contains(first.report.New[0].Line, "FirstInvocation") {
		t.Fatalf("first report = %+v", first.report)
	}
	if len(second.report.New) != 1 || !strings.Contains(second.report.New[0].Line, "SecondInvocation") {
		t.Fatalf("second report = %+v", second.report)
	}
	if ws.lastCommitted != newer.invocation {
		t.Fatalf("committed invocation = %d, want %d", ws.lastCommitted, newer.invocation)
	}
}

func TestCancelledRoundWatchRemainsDueForTurnEndRetry(t *testing.T) {
	dir := t.TempDir()
	firstStarted := filepath.Join(dir, "first-started")
	command := "if [ ! -e " + shellSingleQuote(firstStarted) + " ]; then " +
		": > " + shellSingleQuote(firstStarted) + "; while :; do sleep 1; done; " +
		"else echo '--- FAIL: RetriedAtTurnEnd'; exit 1; fi"

	m := testModel(t)
	recorder := checkpoint.NewRecorder()
	m.app.undo = recorder
	m.app.watchSt.arm(watch.New(command, dir))
	turnCtx, cancelTurn := context.WithCancel(context.Background())
	m.app.watchSt.beginTurn(turnCtx, currentSessionID(m))

	recorder.Begin("cancelled watch turn")
	changed := filepath.Join(dir, "changed.txt")
	if err := os.WriteFile(changed, []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder.Record(changed)

	roundDone := make(chan []provider.Message, 1)
	go func() { roundDone <- m.app.watchRound() }()
	waitForWatchPath(t, firstStarted)
	cancelTurn()
	select {
	case messages := <-roundDone:
		if len(messages) != 0 {
			t.Fatalf("cancelled round watch injected %d message(s)", len(messages))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled round watch did not release its goroutine")
	}

	m.app.watchSt.mu.Lock()
	lastPending := m.app.watchSt.lastPending
	lastCommitted := m.app.watchSt.lastCommitted
	m.app.watchSt.mu.Unlock()
	if lastPending != 0 || lastCommitted != 0 {
		t.Fatalf("cancelled round consumed verifier evidence: pending=%d committed=%d", lastPending, lastCommitted)
	}

	turnEnd := m.watchTurnEnd()
	if turnEnd == nil {
		t.Fatal("cancelled verifier captures were not due at turn end")
	}
	result := make(chan tea.Msg, 1)
	go func() { result <- turnEnd() }()
	var report watchReportMsg
	select {
	case message := <-result:
		var ok bool
		report, ok = message.(watchReportMsg)
		if !ok {
			t.Fatalf("turn-end retry returned %T", message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("turn-end verifier retry did not finish")
	}
	if !report.turnEnd || report.rep.Err != nil || !report.rep.FirstRun || len(report.rep.New) != 1 ||
		!strings.Contains(report.rep.New[0].Line, "RetriedAtTurnEnd") {
		t.Fatalf("turn-end retry report = %+v", report)
	}
	m.onWatchReport(report)

	m.app.watchSt.mu.Lock()
	pendingCancels := len(m.app.watchSt.pendingCancels)
	tail := m.app.watchSt.runTail
	m.app.watchSt.mu.Unlock()
	if pendingCancels != 0 {
		t.Fatalf("turn-end retry retained %d detached cancellation(s)", pendingCancels)
	}
	select {
	case <-tail:
	default:
		t.Fatal("turn-end retry retained an open FIFO link")
	}
}

func TestTurnEndWatchFinishesAndFoldsBeforeAQueuedTurnStarts(t *testing.T) {
	m := testModel(t)
	m.queue = []string{"queued follow-up"}
	run := startBlockingTurnEndWatch(t, m, "QueuedWatchFailure", false)

	if !m.operationActive || m.operationName != "watch verifier" || !m.busy || m.turnPlanning {
		t.Fatalf("turn-end gate = active:%v name:%q busy:%v planning:%v",
			m.operationActive, m.operationName, m.busy, m.turnPlanning)
	}
	if cmd := m.enqueue("typed while watch was running", ""); cmd != nil {
		t.Fatal("a prompt typed during the final verifier started immediately")
	}
	if len(m.queue) != 2 {
		t.Fatalf("turn-end verifier did not retain prompt order: %#v", m.queue)
	}

	done := finishBlockingTurnEndWatch(t, run)
	_, next := m.Update(done)
	if next == nil || m.operationActive || !m.turnPlanning || !m.busy {
		t.Fatalf("queued continuation = cmd:%v operation:%v planning:%v busy:%v",
			next != nil, m.operationActive, m.turnPlanning, m.busy)
	}
	if len(m.queue) != 1 || m.queue[0] != "typed while watch was running" {
		t.Fatalf("queued continuation order = %#v", m.queue)
	}
	planned, ok := next().(turnPlanMsg)
	if !ok {
		t.Fatalf("queued continuation returned an unexpected message")
	}
	if !strings.Contains(planned.prompt, "queued follow-up") || !strings.Contains(planned.prompt, "QueuedWatchFailure") {
		t.Fatalf("queued opening did not carry the completed verifier fold:\n%s", planned.prompt)
	}
	m.finishPlanning()
	assertTurnEndWatchReleased(t, m)
}

func TestTurnEndWatchFinishesBeforeDeferredStartup(t *testing.T) {
	m := testModel(t)
	deferredCalled := false
	backgroundRan := make(chan struct{}, 1)
	m.deferredStartup = func() tea.Cmd {
		deferredCalled = true
		return func() tea.Msg {
			backgroundRan <- struct{}{}
			return nil
		}
	}
	run := startBlockingTurnEndWatch(t, m, "DeferredWatchFailure", false)
	if deferredCalled {
		t.Fatal("deferred startup began before the final verifier finished")
	}

	done := finishBlockingTurnEndWatch(t, run)
	_, background := m.Update(done)
	if !deferredCalled || background == nil || m.operationActive || m.busy {
		t.Fatalf("deferred release = called:%v cmd:%v operation:%v busy:%v",
			deferredCalled, background != nil, m.operationActive, m.busy)
	}
	background()
	select {
	case <-backgroundRan:
	default:
		t.Fatal("deferred startup command did not run")
	}
	if folded := m.watchContext("later prompt"); !strings.Contains(folded, "DeferredWatchFailure") {
		t.Fatalf("deferred startup lost the unconsumed verifier fold:\n%s", folded)
	}
	assertTurnEndWatchReleased(t, m)
}

func TestTurnEndWatchFinishesBeforeAutoCompaction(t *testing.T) {
	m := testModel(t)
	if err := m.app.loop.Session.AppendMessage(provider.UserText("conversation to compact")); err != nil {
		t.Fatal(err)
	}
	m.app.config.CompactAuto = true
	m.app.config.CompactAtPercent = 85
	m.ctxWindow = 100_000
	m.callTokens = 90_000
	run := startBlockingTurnEndWatch(t, m, "AutoCompactWatchFailure", false)

	if m.operationName != "watch verifier" || strings.Contains(strings.Join(m.tr.flat, "\n"), "compacting automatically") {
		t.Fatalf("auto-compact overtook final watch: operation=%q", m.operationName)
	}
	done := finishBlockingTurnEndWatch(t, run)
	_, compact := m.Update(done)
	if compact == nil || !m.operationActive || m.operationName != "compact" {
		t.Fatalf("auto-compact release = cmd:%v active:%v operation:%q",
			compact != nil, m.operationActive, m.operationName)
	}
	if !strings.Contains(strings.Join(m.tr.flat, "\n"), "compacting automatically") {
		t.Fatal("auto-compact did not begin after the verifier completed")
	}
	if folded := m.watchContext("post-compact continuation"); !strings.Contains(folded, "AutoCompactWatchFailure") {
		t.Fatalf("auto-compact lost the verifier fold before session adoption:\n%s", folded)
	}
	m.finishOperation(m.operationGeneration, false)
	assertTurnEndWatchReleased(t, m)
}

func TestTurnEndWatchCancellationReleasesItsGateAndQueuedTurn(t *testing.T) {
	m := testModel(t)
	m.queue = []string{"continue after cancellation"}
	run := startBlockingTurnEndWatch(t, m, "MustNotCommit", false)
	m.interrupt()

	var done tea.Msg
	select {
	case done = <-run.done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled final verifier retained its command goroutine")
	}
	_, next := m.Update(done)
	if next == nil || m.operationActive || !m.turnPlanning {
		t.Fatalf("cancelled gate release = cmd:%v operation:%v planning:%v",
			next != nil, m.operationActive, m.turnPlanning)
	}
	if folded := m.watchContext("later"); strings.Contains(folded, "MustNotCommit") {
		t.Fatalf("cancelled verifier committed a partial observation:\n%s", folded)
	}
	m.finishPlanning()
	assertTurnEndWatchReleased(t, m)
}

func TestAbnormalTUIExitCancelsTurnEndWatchCommand(t *testing.T) {
	m := testModel(t)
	run := startBlockingTurnEndWatch(t, m, "MustNotSurviveExit", false)
	terminalErr := errors.New("terminal disconnected")
	if err := runTUIProgram(tuiProgramFunc(func() (tea.Model, error) { return m, terminalErr }), m); !errors.Is(err, terminalErr) {
		t.Fatalf("runTUIProgram error = %v", err)
	}
	select {
	case <-run.done:
	case <-time.After(5 * time.Second):
		t.Fatal("abnormal TUI exit retained the final verifier goroutine")
	}
	assertTurnEndWatchReleased(t, m)
}

type blockingTurnEndWatchRun struct {
	m       *tuiModel
	release string
	done    <-chan tea.Msg
}

func startBlockingTurnEndWatch(t *testing.T, m *tuiModel, failure string, suppressAutoCompact bool) blockingTurnEndWatchRun {
	t.Helper()
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	release := filepath.Join(dir, "release")
	command := ": > " + shellSingleQuote(started) + "; " +
		"while [ ! -e " + shellSingleQuote(release) + " ]; do sleep 0.01; done; " +
		"echo '--- FAIL: " + failure + "'; exit 1"

	recorder := checkpoint.NewRecorder()
	m.app.undo = recorder
	m.app.watchSt.arm(watch.New(command, dir))
	turnCtx, cancelTurn := context.WithCancel(context.Background())
	m.turnGeneration++
	m.turnCtx = turnCtx
	m.turnCancel = cancelTurn
	m.busy = true
	m.started = time.Now()
	m.turnStarted = m.app.tier
	m.app.watchSt.beginTurn(turnCtx, currentSessionID(m))
	recorder.Begin("completed turn")
	changed := filepath.Join(dir, "changed.txt")
	if err := os.WriteFile(changed, []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder.Record(changed)

	cmd := m.onTurnDone(turnDoneMsg{
		generation:          m.turnGeneration,
		after:               m.app.loop.Session.State(),
		suppressAutoCompact: suppressAutoCompact,
	})
	if cmd == nil {
		t.Fatal("completed turn did not start its due verifier")
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	waitForWatchPath(t, started)
	return blockingTurnEndWatchRun{m: m, release: release, done: done}
}

func finishBlockingTurnEndWatch(t *testing.T, run blockingTurnEndWatchRun) tea.Msg {
	t.Helper()
	if err := os.WriteFile(run.release, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case done := <-run.done:
		if _, ok := done.(turnEndWatchDoneMsg); !ok {
			t.Fatalf("turn-end gate returned %T", done)
		}
		return done
	case <-time.After(5 * time.Second):
		t.Fatal("turn-end verifier did not finish")
		return nil
	}
}

func assertTurnEndWatchReleased(t *testing.T, m *tuiModel) {
	t.Helper()
	m.app.watchSt.mu.Lock()
	pendingCancels := len(m.app.watchSt.pendingCancels)
	tail := m.app.watchSt.runTail
	m.app.watchSt.mu.Unlock()
	if pendingCancels != 0 {
		t.Fatalf("turn-end watch retained %d cancellation(s)", pendingCancels)
	}
	select {
	case <-tail:
	default:
		t.Fatal("turn-end watch retained an open FIFO link")
	}
}

func waitForWatchPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("watch command did not create %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
