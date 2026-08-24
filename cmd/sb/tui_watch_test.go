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

	route "github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/watch"
)

func bindWatchReport(t *testing.T, m *tuiModel, msg watchReportMsg) watchReportMsg {
	t.Helper()
	if m.app.watchSt.armed() == nil {
		m.app.watchSt.arm(watch.New(msg.command, m.app.workspace))
	}
	m.app.watchSt.beginTurn(context.Background(), currentSessionID(m))
	ticket, _, ok := m.app.watchSt.due(1)
	if !ok {
		t.Fatal("watch report ticket was not due")
	}
	// This helper fabricates an already-completed verifier report, so release
	// the execution FIFO exactly as the production runner would.
	m.app.watchSt.finish(ticket)
	msg.ticket = ticket
	return msg
}

func TestWatchCommandArmsReportsAndDisarms(t *testing.T) {
	m := testModel(t)
	m.app.undo = nil

	// Arming needs the recorder: without it the watch would never be due
	// and would sit armed while silently doing nothing.
	cmdWatch(m, "go test ./...")
	if m.app.watchSt.armed() != nil {
		t.Fatal("armed without a checkpoint recorder")
	}

	m2 := testModel(t)
	m2.app.undo = checkpoint.NewRecorder()
	cmdWatch(m2, "go test ./...")
	w := m2.app.watchSt.armed()
	if w == nil || w.Command() != "go test ./..." {
		t.Fatalf("arming did not take: %+v", w)
	}
	view := m2.View()
	if !strings.Contains(view, "watch ✓") {
		t.Error("the status chip does not show the armed watch")
	}

	if cmd := cmdWatch(m2, "off"); cmd == nil {
		t.Fatal("disarming said nothing")
	}
	if m2.app.watchSt.armed() != nil {
		t.Error("off left the watch armed")
	}
	if strings.Contains(m2.View(), "watch ✓") {
		t.Error("the status chip survived disarming")
	}
}

func TestWatchCommandRedactsEveryPersistentAndVisibleCopy(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	raw := "GITHUB_TOKEN=" + testGitHubToken + " go test ./..."

	cmdWatch(m, raw)
	armed := m.app.watchSt.armed()
	if armed == nil {
		t.Fatal("watch command was not armed")
	}
	if armed.Command() != raw {
		t.Fatal("the executable watch command did not retain the exact user input")
	}

	// Exercise the arm notice, bare status, disarm note, and asynchronous
	// disarm notice, plus a runner error that repeats both the command and key.
	// None of these non-execution copies may retain the raw key.
	m.onWatchReport(bindWatchReport(t, m, watchReportMsg{
		command: raw,
		rep:     watch.Report{Err: errors.New("launch failed for " + testGitHubToken)},
	}))
	cmdWatch(m, "")
	off := cmdWatch(m, "off")
	if off == nil {
		t.Fatal("disarming returned no notice")
	}
	offMsg := off()
	msg, ok := offMsg.(noticeMsg)
	if !ok {
		t.Fatalf("disarming returned %T, want noticeMsg", offMsg)
	}
	m.Update(msg)

	visible := strings.Join(m.tr.flat, "\n") + "\n" + m.View()
	if strings.Contains(visible, testGitHubToken) {
		t.Fatal("a visible watch surface retained the raw credential")
	}
	if !strings.Contains(visible, "[redacted: a GitHub token]") {
		t.Fatalf("visible watch surfaces did not identify the redaction:\n%s", visible)
	}

	durable, err := os.ReadFile(m.app.loop.Session.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(durable), testGitHubToken) {
		t.Fatal("the session log retained the raw watch credential")
	}
	if !strings.Contains(string(durable), "[redacted: a GitHub token]") {
		t.Fatalf("watch notes did not persist the redacted spelling:\n%s", durable)
	}
}

func TestAbnormalTUIExitReleasesUnstartedTurnEndWatchTicket(t *testing.T) {
	m := operationTestModel(t)
	m.app.undo = checkpoint.NewRecorder()
	m.app.undo.Begin("edited turn")
	m.app.undo.Record(filepath.Join(t.TempDir(), "new.go"))
	m.app.watchSt.arm(watch.New("go test ./...", m.app.workspace))
	m.app.watchSt.beginTurn(context.Background(), currentSessionID(m))

	cmd := m.startTurnEndWatch(false)
	if cmd == nil {
		t.Fatal("turn-end watch did not start")
	}
	m.app.watchSt.mu.Lock()
	tail := m.app.watchSt.runTail
	pending := len(m.app.watchSt.pendingCancels)
	m.app.watchSt.mu.Unlock()
	if pending != 1 {
		t.Fatalf("pending turn-end watch cancellations = %d, want 1", pending)
	}

	if err := runTUIProgram(tuiProgramFunc(func() (tea.Model, error) { return m, nil }), m); err != nil {
		t.Fatal(err)
	}
	m.app.watchSt.mu.Lock()
	pending = len(m.app.watchSt.pendingCancels)
	m.app.watchSt.mu.Unlock()
	if pending != 0 {
		t.Fatalf("abandoned turn-end watch retained %d cancellation(s)", pending)
	}
	select {
	case <-tail:
	default:
		t.Fatal("abandoned turn-end watch retained its FIFO link")
	}
	if msg := cmd(); msg != nil {
		t.Fatalf("abandoned turn-end watch returned %#v", msg)
	}
}

func TestWatchInjectSpeaksOnlyOnAChange(t *testing.T) {
	// A repeat verdict is silence.
	if got := watchInjectText("go test", watch.Report{Persisting: 2, Signatures: []string{"a", "b"}}); got != "" {
		t.Errorf("a repeat verdict injected: %q", got)
	}

	// New failures name the command, the exit, and the lines.
	rep := watch.Report{
		ExitCode:   1,
		New:        []route.Failure{{Signature: "s1", Line: "--- FAIL: TestAlpha"}},
		Persisting: 1,
		Signatures: []string{"s1", "s0"},
	}
	text := watchInjectText("go test ./...", rep)
	for _, want := range []string{"[watch]", "go test ./...", "--- FAIL: TestAlpha", "1 earlier failure(s) persist"} {
		if !strings.Contains(text, want) {
			t.Errorf("inject text is missing %q:\n%s", want, text)
		}
	}

	// Green after red is the one green worth telling the model.
	green := watchInjectText("go test", watch.Report{Passed: true, WentGreen: true})
	if !strings.Contains(green, "now passes") {
		t.Errorf("going green was not announced: %q", green)
	}
}

func TestWatchInjectRedactsWhatTheGateWouldHold(t *testing.T) {
	token := "sk-ant-api03-" + "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefghijklmnop"
	rep := watch.Report{
		ExitCode: 1,
		New: []route.Failure{{
			Signature: "s1",
			Line:      "FAIL: env leaked " + token,
		}},
		Signatures: []string{"s1"},
	}
	text := watchInjectText("env", rep)
	if strings.Contains(text, "sk-ant-api03-abcdefghijklmnop") {
		t.Fatalf("a key rode the watch report to the model:\n%s", text)
	}

	// Scan the complete failure before the presentation cap. Truncating first
	// used to leave a short issuer prefix below ScanPrompt's length floor.
	rep.New[0].Line = strings.Repeat("x", 179) + " " + token
	text = watchInjectText("env", rep)
	if strings.Contains(text, "sk-ant-api03-") || !strings.Contains(text, "[redacted") {
		t.Fatalf("a boundary-straddling key fragment survived the watch cap:\n%s", text)
	}
}

func TestWatchNoticeRedactsBeforeItsShorterCap(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	cmdWatch(m, "go test")
	token := "sk-ant-api03-" + "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefghijklmnop"
	msg := bindWatchReport(t, m, watchReportMsg{command: "go test", rep: watch.Report{
		ExitCode:   1,
		New:        []route.Failure{{Signature: "secret", Line: strings.Repeat("x", 99) + " " + token}},
		Signatures: []string{"secret"},
	}})
	m.onWatchReport(msg)
	joined := strings.Join(m.tr.flat, "\n")
	if strings.Contains(joined, "sk-ant-api03-") || !strings.Contains(joined, "redacted") {
		t.Fatalf("the bounded watch notice retained a credential fragment:\n%s", joined)
	}
}

func TestWatchInjectCapsTheLinesItCarries(t *testing.T) {
	rep := watch.Report{ExitCode: 1}
	for _, l := range []string{"--- FAIL: A", "--- FAIL: B", "--- FAIL: C", "--- FAIL: D", "--- FAIL: E"} {
		rep.New = append(rep.New, route.Failure{Signature: l, Line: l})
		rep.Signatures = append(rep.Signatures, l)
	}
	text := watchInjectText("go test", rep)
	if strings.Contains(text, "--- FAIL: D") {
		t.Error("the cap did not hold")
	}
	if !strings.Contains(text, "2 more new failures") {
		t.Errorf("the dropped lines were not counted:\n%s", text)
	}
}

func TestWatchReportRendersOnceAndKeepsTheChipCurrent(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	cmdWatch(m, "go test")

	before := len(m.tr.entries)
	m.onWatchReport(bindWatchReport(t, m, watchReportMsg{command: "go test", rep: watch.Report{
		ExitCode:   1,
		New:        []route.Failure{{Signature: "s1", Line: "--- FAIL: TestAlpha"}},
		Signatures: []string{"s1"},
	}}))
	if len(m.tr.entries) == before {
		t.Error("a new failure rendered nothing")
	}
	if !strings.Contains(m.View(), "watch ✗1") {
		t.Error("the chip does not show the failure count")
	}

	// The same verdict again: chip stays, transcript stays quiet.
	before = len(m.tr.entries)
	m.onWatchReport(bindWatchReport(t, m, watchReportMsg{command: "go test", rep: watch.Report{
		ExitCode: 1, Persisting: 1, Signatures: []string{"s1"},
	}}))
	if len(m.tr.entries) != before {
		t.Error("a repeat verdict rendered a notice")
	}

	m.onWatchReport(bindWatchReport(t, m, watchReportMsg{command: "go test", rep: watch.Report{Passed: true, WentGreen: true}}))
	if !strings.Contains(m.View(), "watch ✓") {
		t.Error("the chip did not go green")
	}
}

func TestWatchTurnEndVerdictFoldsIntoTheNextPrompt(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	cmdWatch(m, "go test")

	m.onWatchReport(bindWatchReport(t, m, watchReportMsg{command: "go test", turnEnd: true, rep: watch.Report{
		ExitCode:   1,
		New:        []route.Failure{{Signature: "s1", Line: "--- FAIL: TestAlpha"}},
		Signatures: []string{"s1"},
	}}))
	prompt := m.watchContext("fix it")
	// The typed prompt leads and the report follows, so an opening never
	// leads with the injection label /retry's shape check reads.
	if !strings.Contains(prompt, "--- FAIL: TestAlpha") || !strings.HasPrefix(prompt, "fix it") {
		t.Fatalf("the verdict did not fold in behind the prompt:\n%s", prompt)
	}
	if again := m.watchContext("next"); again != "next" {
		t.Errorf("the fold was not drained: %q", again)
	}
}

func TestWatchTurnEndGreenTransitionFoldsIntoTheNextPrompt(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	cmdWatch(m, "go test")

	m.onWatchReport(bindWatchReport(t, m, watchReportMsg{
		command: "go test", turnEnd: true,
		rep: watch.Report{Passed: true, WentGreen: true},
	}))
	if prompt := m.watchContext("continue"); !strings.Contains(prompt, "now passes") {
		t.Fatalf("the turn-end green transition did not fold into the next prompt:\n%s", prompt)
	}
}

// A turn-end run can outlive its turn; its stale count must not blind the
// next turn's due check.
func TestAStaleTurnEndRunCannotBlindTheNextTurn(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	cmdWatch(m, "go test")
	ws := m.app.watchSt
	ws.beginTurn(nil, currentSessionID(m))

	// Round one of turn one: three files captured, run due.
	ticket, _, ok := ws.due(3)
	if !ok {
		t.Fatal("three fresh captures were not due")
	}

	// The next turn begins before the run reports back.
	ws.beginTurn(nil, currentSessionID(m))
	ws.mu.Lock()
	if ws.ticketCurrentLocked(ticket) {
		ws.ranLocked(ticket)
	}
	ws.mu.Unlock()

	// One capture into the new turn must be news.
	if _, _, ok := ws.due(1); !ok {
		t.Fatal("a stale ran() blinded the new turn")
	}
}

func TestWatchRejectsResultsOlderThanACommittedInvocation(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	cmdWatch(m, "go test")
	ws := m.app.watchSt
	ws.beginTurn(context.Background(), currentSessionID(m))

	old, _, ok := ws.due(1)
	if !ok {
		t.Fatal("old watch run was not due")
	}
	// A cancellation can legitimately leave a gap in the invocation sequence.
	// Once its FIFO link is released, the next valid observation may commit.
	ws.finish(old)

	ws.beginTurn(context.Background(), currentSessionID(m))
	newer, _, ok := ws.due(1)
	if !ok {
		t.Fatal("new watch run was not due")
	}
	if _, committed := ws.commit(newer, watch.Observation{}); !committed {
		t.Fatal("newer watch observation did not commit")
	}
	if _, committed := ws.commit(old, watch.Observation{}); committed {
		t.Fatal("an older observation regressed the committed watch baseline")
	}

	if !ws.reportCurrent(newer) {
		t.Fatal("newer committed report was not current")
	}
	if ws.reportCurrent(old) {
		t.Fatal("an older report rendered after a newer invocation")
	}
}

func TestWatchInvalidationReleasesQueuedInvocations(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	cmdWatch(m, "go test")
	ws := m.app.watchSt
	ws.beginTurn(context.Background(), currentSessionID(m))

	blocking, _, ok := ws.due(1)
	if !ok {
		t.Fatal("blocking watch run was not due")
	}
	queued, ctx, ok := ws.due(2)
	if !ok {
		t.Fatal("queued watch run was not due")
	}
	done := make(chan bool, 1)
	go func() {
		defer ws.finish(queued)
		_, current := ws.execute(queued, ctx)
		done <- current
	}()

	ws.disarm()
	select {
	case current := <-done:
		if current {
			t.Fatal("invalidated queued watch invocation executed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("/watch invalidation did not release a queued invocation")
	}
	ws.finish(blocking)
}

func TestSuggestVerifierReadsTheWorkspaceNotTheImagination(t *testing.T) {
	write := func(dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	empty := t.TempDir()
	if got := suggestVerifier(empty); got != "" {
		t.Errorf("an empty workspace implied %q", got)
	}

	goDir := t.TempDir()
	write(goDir, "go.mod", "module example.com/x\n")
	if got := suggestVerifier(goDir); got != "go test ./..." {
		t.Errorf("a Go module implied %q", got)
	}

	// The Makefile's own test target outranks the language manifest: it is
	// the project's declaration, not an implication.
	write(goDir, "Makefile", "build:\n\tgo build\n\ntest:\n\tgo test ./...\n")
	if got := suggestVerifier(goDir); got != "make test" {
		t.Errorf("a declared test target lost to the manifest: %q", got)
	}

	npmDir := t.TempDir()
	write(npmDir, "package.json", `{"scripts":{"test":"vitest run"}}`)
	if got := suggestVerifier(npmDir); got != "npm test" {
		t.Errorf("a test script implied %q", got)
	}

	// The npm placeholder fails on purpose; suggesting it would arm a
	// verifier that is red by construction.
	placeholder := t.TempDir()
	write(placeholder, "package.json", `{"scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`)
	if got := suggestVerifier(placeholder); got != "" {
		t.Errorf("the npm placeholder was suggested: %q", got)
	}
}

func TestBareWatchOffersTheWorkspaceVerifier(t *testing.T) {
	m := testModel(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.app.workspace = dir

	cmdWatch(m, "")
	hint := m.tr.entries[len(m.tr.entries)-1].text
	if !strings.Contains(hint, "go test ./...") {
		t.Fatalf("the hint does not offer the implied verifier: %q", hint)
	}
}

// A fold is a report to the conversation that made the edits. A swap that
// replaces the conversation drops it; a compaction, which continues the
// same conversation in summary, carries it.
func TestSessionSwapDropsTheFoldUnlessAsked(t *testing.T) {
	m := testModel(t)
	m.app.watchSt.addFold("[watch] verdict for the old conversation")
	fresh, err := m.app.store.Create(m.app.workspace, m.app.tier.Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	m.onSessionSwap(sessionSwapMsg{sess: fresh, tier: m.app.tier, client: m.app.loop.Provider, fresh: true})
	if got := m.watchContext("hello"); got != "hello" {
		t.Errorf("a cleared session inherited the old conversation's verdict:\n%s", got)
	}

	m.app.watchSt.addFold("[watch] verdict that predates the compaction")
	compacted, err := m.app.store.Create(m.app.workspace, m.app.tier.Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	m.onSessionSwap(sessionSwapMsg{sess: compacted, tier: m.app.tier, client: m.app.loop.Provider, keepFold: true})
	if got := m.watchContext("continue"); !strings.Contains(got, "predates the compaction") {
		t.Errorf("a compaction lost the pending verdict:\n%s", got)
	}
}

func TestLateWatchReportCannotCrossAnOrdinarySessionSwap(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	cmdWatch(m, "go test")
	report := bindWatchReport(t, m, watchReportMsg{command: "go test", turnEnd: true, rep: watch.Report{
		ExitCode:   1,
		New:        []route.Failure{{Signature: "old", Line: "--- FAIL: OldSession"}},
		Signatures: []string{"old"},
	}})
	oldSessionID := report.ticket.sourceSessionID

	fresh, err := m.app.store.Create(m.app.workspace, m.app.tier.Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	m.onSessionSwap(sessionSwapMsg{sess: fresh, tier: m.app.tier, client: m.app.loop.Provider, fresh: true})
	if currentSessionID(m) == oldSessionID {
		t.Fatal("test did not adopt a distinct session")
	}

	before := len(m.tr.entries)
	m.onWatchReport(report)
	if len(m.tr.entries) != before || m.watchFails != 0 {
		t.Fatal("the old session's late watch report changed the new session")
	}
	if got := m.watchContext("new work"); got != "new work" {
		t.Fatalf("the old session's late verdict folded into the new prompt:\n%s", got)
	}
}

func TestCompactionDeliberatelyCarriesAnInFlightWatchReport(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	cmdWatch(m, "go test")
	report := bindWatchReport(t, m, watchReportMsg{command: "go test", turnEnd: true, rep: watch.Report{
		ExitCode:   1,
		New:        []route.Failure{{Signature: "carried", Line: "--- FAIL: BeforeCompaction"}},
		Signatures: []string{"carried"},
	}})

	compacted, err := m.app.store.Create(m.app.workspace, m.app.tier.Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	m.onSessionSwap(sessionSwapMsg{sess: compacted, tier: m.app.tier, client: m.app.loop.Provider, keepFold: true})
	m.onWatchReport(report)
	if got := m.watchContext("continue"); !strings.Contains(got, "BeforeCompaction") {
		t.Fatalf("the explicit same-conversation carry lost its in-flight verdict:\n%s", got)
	}
}

func TestRearmingWatchInvalidatesThePriorInvocation(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	cmdWatch(m, "go test old")
	report := bindWatchReport(t, m, watchReportMsg{command: "go test old", turnEnd: true, rep: watch.Report{
		ExitCode:   1,
		New:        []route.Failure{{Signature: "old", Line: "--- FAIL: OldDeclaration"}},
		Signatures: []string{"old"},
	}})

	cmdWatch(m, "go test new")
	before := len(m.tr.entries)
	m.onWatchReport(report)
	if len(m.tr.entries) != before {
		t.Fatal("a report from the replaced /watch declaration rendered")
	}
	if got := m.watchContext("work"); got != "work" {
		t.Fatalf("a report from the replaced declaration folded into a prompt: %s", got)
	}
}

func TestOrdinaryBoundaryCancelsDetachedWatchButCompactionCarriesIt(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	cmdWatch(m, "go test")
	m.app.watchSt.beginTurn(context.Background(), currentSessionID(m))
	ticket, _, ok := m.app.watchSt.due(1)
	if !ok {
		t.Fatal("watch run was not due")
	}
	ctx, ok := m.app.watchSt.backgroundContext(ticket)
	if !ok {
		t.Fatal("turn-end watch did not get a detached context")
	}

	m.app.watchSt.sessionBoundary("compacted-session", true)
	select {
	case <-ctx.Done():
		t.Fatal("same-conversation compaction cancelled the carried watch run")
	default:
	}
	if !m.app.watchSt.reportCurrent(ticket) {
		t.Fatal("same-conversation compaction invalidated the carried ticket")
	}

	m.app.watchSt.sessionBoundary("ordinary-replacement", false)
	select {
	case <-ctx.Done():
	default:
		t.Fatal("ordinary session adoption did not cancel the detached watch run")
	}
	if m.app.watchSt.reportCurrent(ticket) {
		t.Fatal("ordinary session adoption left the old ticket current")
	}
}
