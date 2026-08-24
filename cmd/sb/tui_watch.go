package main

// /watch: the user's declared verifier, run at the seams of a turn, with
// only the delta reported (internal/watch). The wiring here follows the
// advisor's: evidence reaches the model through the loop's round-boundary
// injection seam, never by rewriting anything already sent, and the same
// evidence feeds the escalation policy through the watcher — §8.4 calls a
// task-specific verifier stronger evidence than the harness's own
// completion signal, and this is where the declaration gets made.
//
// What decides a run is due is the checkpoint recorder's own capture count:
// the loop's evidence that this turn changed files, which is exactly what
// /undo restores from. Mutations the recorder cannot see — a command's side
// effects, a script writing files — do not trip the watch, and that is the
// §8.3 posture, not an oversight: evidence the loop does not keep is absent,
// not guessed.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/watch"
)

// watchInjectLines caps how many new failing lines ride to the model. The
// point of the message is "look at the verifier", not the verifier's whole
// output; the model can run the command itself when three lines are not
// enough.
const watchInjectLines = 3

type watchReportMsg struct {
	command string
	rep     watch.Report
	turnEnd bool
	ticket  watchRunTicket
}

// turnEndWatchDoneMsg is the exclusive-operation completion fence around a
// final verifier run. report is populated by direct/test programs; the real
// Bubble Tea program receives the report through Program.Send before this
// completion, which preserves fold-before-continuation ordering.
type turnEndWatchDoneMsg struct {
	operation           uint64
	sourceID            string
	suppressAutoCompact bool
	report              tea.Msg
}

// watchRunTicket binds one verifier execution to the conversation and exact
// /watch declaration that launched it. The session epoch is carried only by
// swaps that explicitly continue the same conversation (compaction and a kept
// race arm); every other committed adoption invalidates it.
type watchRunTicket struct {
	sourceSessionID string
	sessionEpoch    uint64
	turnGeneration  int
	invocation      uint64
	pending         int
	watch           *watch.Watch
	sequence        *watchRunSequence
}

// watchRunSequence is one link in a declaration-local FIFO. A turn-end run is
// asynchronous, so the next turn can otherwise finish a newer verifier run
// first and let the old observation regress Watch's delta baseline. The link
// is carried by value with the ticket; the once keeps every cancellation and
// stale-result path safe to finish.
type watchRunSequence struct {
	previous    <-chan struct{}
	invalidated <-chan struct{}
	done        chan struct{}
	once        sync.Once
}

func (s *watchRunSequence) wait(ctx context.Context) bool {
	if s == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return false
	}
	if s.invalidated != nil {
		select {
		case <-s.invalidated:
			return false
		default:
		}
	}
	if s.previous != nil {
		select {
		case <-s.previous:
		case <-ctx.Done():
			return false
		case <-s.invalidated:
			return false
		}
	}
	if ctx.Err() != nil {
		return false
	}
	if s.invalidated != nil {
		select {
		case <-s.invalidated:
			return false
		default:
		}
	}
	return true
}

func (s *watchRunSequence) finish() {
	if s != nil {
		s.once.Do(func() { close(s.done) })
	}
}

// watchState is the mutable half of the feature, guarded because the loop's
// goroutine consults it at round boundaries while the UI goroutine arms,
// disarms, and folds.
type watchState struct {
	mu      sync.Mutex
	w       *watch.Watch
	turnCtx context.Context

	// lastPending is the recorder's capture count when the verifier last
	// ran, reset each turn with the recorder's own scope. A run is due when
	// the count has grown past it. gen counts turns, because a turn-end run
	// finishes on its own goroutine and may land after the next turn has
	// begun — its stale count must not overwrite the fresh turn's zero, or
	// the new turn's first edits would never look new.
	lastPending int
	gen         int

	// epoch and sessionSources are the conversation ownership stamp for a run.
	// invocation distinguishes individual executions, while watch identifies
	// the exact declaration so /watch off or a re-arm invalidates old work.
	epoch          uint64
	nextInvocation uint64
	lastCommitted  uint64
	lastDelivered  uint64
	turnSessionID  string
	currentSession string
	sessionSources map[string]struct{}
	pendingCancels map[uint64]context.CancelFunc
	runTail        <-chan struct{}
	runInvalidated chan struct{}

	// fold holds turn-end reports for the next prompt, the seam advice and
	// ! output already use: one user message per turn.
	fold []string
}

func (ws *watchState) arm(w *watch.Watch) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.invalidateRunsLocked()
	ws.w = w
	ws.lastPending = 0
	ws.fold = nil
}

func (ws *watchState) disarm() *watch.Watch {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	w := ws.w
	ws.invalidateRunsLocked()
	ws.w = nil
	ws.fold = nil
	return w
}

func (ws *watchState) armed() *watch.Watch {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.w
}

// beginTurn resets the per-turn counter alongside the recorder's new scope
// and remembers the turn's context, so an interrupted turn interrupts a
// mid-turn verifier run with it.
func (ws *watchState) beginTurn(ctx context.Context, sessionID string) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.epoch == 0 {
		ws.epoch = 1
	}
	if ws.sessionSources == nil {
		ws.sessionSources = make(map[string]struct{})
	}
	// A direct binding change that did not pass the normal adoption seam still
	// fails closed. Production swaps call sessionBoundary first; this guard
	// keeps tests and future call sites from silently carrying a verdict.
	if ws.currentSession != "" && sessionID != ws.currentSession {
		if _, carried := ws.sessionSources[sessionID]; !carried {
			ws.invalidateRunsLocked()
			if ws.w != nil {
				ws.w.ResetBaseline()
			}
			ws.sessionSources = make(map[string]struct{})
		}
	}
	if sessionID != "" {
		ws.sessionSources[sessionID] = struct{}{}
	}
	ws.currentSession = sessionID
	ws.turnSessionID = sessionID
	ws.lastPending = 0
	ws.gen++
	ws.turnCtx = ctx
}

// due reports whether the verifier should run now: armed, and the turn has
// captured files it has not seen. Its ticket is immutable ownership evidence
// for both committing the delta baseline and later rendering the report.
func (ws *watchState) due(pending int) (watchRunTicket, context.Context, bool) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.w == nil || pending <= ws.lastPending {
		return watchRunTicket{}, nil, false
	}
	ctx := ws.turnCtx
	if ctx == nil {
		ctx = context.Background()
	}
	ws.nextInvocation++
	if ws.nextInvocation == 0 {
		ws.nextInvocation++
	}
	if ws.runInvalidated == nil {
		ws.runInvalidated = make(chan struct{})
	}
	sequence := &watchRunSequence{
		previous:    ws.runTail,
		invalidated: ws.runInvalidated,
		done:        make(chan struct{}),
	}
	ws.runTail = sequence.done
	return watchRunTicket{
		sourceSessionID: ws.turnSessionID,
		sessionEpoch:    ws.epoch,
		turnGeneration:  ws.gen,
		invocation:      ws.nextInvocation,
		pending:         pending,
		watch:           ws.w,
		sequence:        sequence,
	}, ctx, true
}

// backgroundContext makes a turn-end execution independent of the completed
// turn's cancellation while still cancellable by an ordinary session swap,
// /watch off, or re-arming the declaration.
func (ws *watchState) backgroundContext(ticket watchRunTicket) (context.Context, bool) {
	return ws.backgroundContextFrom(ticket, context.Background())
}

// backgroundContextFrom gives the TUI's operation owner a cancellation seam
// without weakening declaration/session invalidation. The ordinary helper
// above preserves the detached turn-end contract for focused callers.
func (ws *watchState) backgroundContextFrom(ticket watchRunTicket, owner context.Context) (context.Context, bool) {
	ws.mu.Lock()
	if !ws.ticketCurrentLocked(ticket) {
		ws.mu.Unlock()
		ticket.sequence.finish()
		return nil, false
	}
	if owner == nil {
		owner = context.Background()
	}
	ctx, cancel := context.WithCancel(owner)
	if ws.pendingCancels == nil {
		ws.pendingCancels = make(map[uint64]context.CancelFunc)
	}
	ws.pendingCancels[ticket.invocation] = cancel
	ws.mu.Unlock()
	return ctx, true
}

// execute waits for every earlier invocation from this declaration before it
// observes the workspace. The state mutex remains free while the command runs,
// so /watch off, session adoption, and cancellation stay responsive.
func (ws *watchState) execute(ticket watchRunTicket, ctx context.Context) (watch.Report, bool) {
	if !ticket.sequence.wait(ctx) {
		return watch.Report{}, false
	}
	ws.mu.Lock()
	current := ws.ticketCurrentLocked(ticket) && ticket.invocation > ws.lastCommitted
	ws.mu.Unlock()
	if !current {
		return watch.Report{}, false
	}
	observation := ticket.watch.Observe(ctx)
	// The turn owns a round-boundary verifier. If that turn is cancelled while
	// the command is running, the partial execution is not a verifier verdict and
	// must not consume the recorder count. onTurnDone can then run the same pending
	// captures with its detached context. Ordinary launch failures still commit a
	// harness-error report because their owner context remains live.
	if ctx.Err() != nil {
		return watch.Report{}, false
	}
	return ws.commitObservation(ticket, observation)
}

// commit is the testable staged-observation path. Production executions use
// execute so observations themselves are serialized, not merely the baseline
// mutation. Direct callers still release their FIFO link on every outcome.
func (ws *watchState) commit(ticket watchRunTicket, observation watch.Observation) (watch.Report, bool) {
	defer ws.finish(ticket)
	return ws.commitObservation(ticket, observation)
}

// commitObservation is the only TUI path that may advance a Watch baseline.
// It holds the session ticket lock while committing, so a swap either
// invalidates first and this observation is discarded, or resets the
// just-committed baseline before the new conversation can use it.
func (ws *watchState) commitObservation(ticket watchRunTicket, observation watch.Observation) (watch.Report, bool) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if !ws.ticketCurrentLocked(ticket) || ticket.invocation <= ws.lastCommitted {
		return watch.Report{}, false
	}
	rep := ticket.watch.Commit(observation)
	ws.lastCommitted = ticket.invocation
	ws.ranLocked(ticket)
	return rep, true
}

// finish releases the next invocation and retires a detached turn-end
// context. It is deliberately separate from commitObservation: callers keep
// their FIFO ownership until their report has been handed to Bubble Tea, so
// reports cannot overtake the baseline order they describe.
func (ws *watchState) finish(ticket watchRunTicket) {
	ws.mu.Lock()
	if cancel := ws.pendingCancels[ticket.invocation]; cancel != nil {
		delete(ws.pendingCancels, ticket.invocation)
		cancel()
	}
	ws.mu.Unlock()
	ticket.sequence.finish()
}

func (ws *watchState) ranLocked(ticket watchRunTicket) {
	if ticket.turnGeneration == ws.gen {
		ws.lastPending = ticket.pending
	}
}

// reportCurrent revalidates a committed observation when its Bubble Tea
// message reaches the UI. A swap can commit after commit() and before this
// message is handled; that report must still not render or fold into the new
// conversation.
func (ws *watchState) reportCurrent(ticket watchRunTicket) bool {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if !ws.ticketCurrentLocked(ticket) || ticket.invocation <= ws.lastDelivered {
		return false
	}
	ws.lastDelivered = ticket.invocation
	return true
}

func (ws *watchState) ticketCurrentLocked(ticket watchRunTicket) bool {
	if ticket.invocation == 0 || ticket.sessionEpoch == 0 ||
		ticket.sessionEpoch != ws.epoch || ticket.watch == nil || ticket.watch != ws.w {
		return false
	}
	_, sourceCurrent := ws.sessionSources[ticket.sourceSessionID]
	return ticket.sourceSessionID != "" && sourceCurrent
}

// sessionBoundary invalidates every ordinary session's staged and committed
// report. carry is the explicit same-conversation exception used by
// compaction and a kept race arm: their source IDs join the same epoch, and a
// pending turn-end observation remains eligible to commit and render.
func (ws *watchState) sessionBoundary(nextSessionID string, carry bool) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.epoch == 0 {
		ws.epoch = 1
	}
	if carry {
		if ws.sessionSources == nil {
			ws.sessionSources = make(map[string]struct{})
		}
		if ws.currentSession != "" {
			ws.sessionSources[ws.currentSession] = struct{}{}
		}
		if nextSessionID != "" {
			ws.sessionSources[nextSessionID] = struct{}{}
		}
		ws.currentSession = nextSessionID
		return
	}

	ws.invalidateRunsLocked()
	ws.lastPending = 0
	ws.turnCtx = nil
	ws.turnSessionID = ""
	ws.fold = nil
	ws.sessionSources = make(map[string]struct{})
	if nextSessionID != "" {
		ws.sessionSources[nextSessionID] = struct{}{}
	}
	ws.currentSession = nextSessionID
	if ws.w != nil {
		ws.w.ResetBaseline()
	}
}

func (ws *watchState) invalidateRunsLocked() {
	ws.epoch++
	if ws.epoch == 0 {
		ws.epoch++
	}
	for invocation, cancel := range ws.pendingCancels {
		cancel()
		delete(ws.pendingCancels, invocation)
	}
	// A different declaration or conversation owns an independent FIFO. Old
	// links finish through their canceled/stale paths, but cannot hold new work.
	if ws.runInvalidated != nil {
		close(ws.runInvalidated)
		ws.runInvalidated = nil
	}
	ws.runTail = nil
}

func (ws *watchState) addFold(text string) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.fold = append(ws.fold, text)
}

func (ws *watchState) takeFold() []string {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	out := ws.fold
	ws.fold = nil
	return out
}

// inject is the loop's round-boundary seam, composed once at assembly: the
// user's steers first, then whatever the advisor queued, then the watch's
// delta. Each part
// contributes nothing when off, so the loop never needs its Inject swapped.
// Every message leaves marked Injected, because a log reader — /retry above
// all — must be able to tell a turn's opening from what rode in mid-turn.
func (a *tuiApp) inject() []provider.Message {
	var out []provider.Message
	out = append(out, a.steerRound()...)
	if adv := a.currentAdvisor(); adv != nil {
		out = append(out, adv.Drain()...)
	}
	out = append(out, a.watchRound()...)
	out = append(out, a.driftRound()...)
	out = append(out, a.pressureRound()...)
	out = append(out, a.toolImageRound()...)
	out = append(out, a.ruleRound()...)
	for i := range out {
		out[i].Injected = true
	}
	return out
}

// watchRound runs the verifier at a round boundary when this turn has edits
// it has not checked. It runs on the loop's goroutine deliberately: the
// model is about to build its next request, and "the tests just broke" is
// worth waiting for — a human pair would not keep typing through it either.
func (a *tuiApp) watchRound() []provider.Message {
	if a.undo == nil {
		return nil
	}
	ticket, ctx, ok := a.watchSt.due(a.undo.PendingFiles())
	if !ok {
		return nil
	}
	defer a.watchSt.finish(ticket)
	rep, current := a.watchSt.execute(ticket, ctx)
	if !current {
		return nil
	}
	command := redactedWatchCommand(ticket.watch.Command())
	if a.p != nil {
		a.p.Send(watchReportMsg{command: command, rep: rep, ticket: ticket})
	}
	if a.watcher != nil && len(rep.Signatures) > 0 {
		a.watcher.VerifierFailures(ctx, rep.Signatures)
	}
	if text := watchInjectText(command, rep); text != "" {
		return []provider.Message{provider.UserText(text)}
	}
	return nil
}

// redactedWatchCommand is the only spelling of a verifier command allowed to
// leave watch.Watch for a non-execution use. The Watch retains the exact user
// input because the shell must execute it byte-for-byte; every display, note,
// report, and model-facing copy uses this unattended redaction posture instead.
func redactedWatchCommand(command string) string {
	return redactCredentialText(command)
}

// watchInjectText is what the model reads. Only a change speaks: a repeat
// verdict injects nothing, because a verifier that repeats itself every
// round teaches its reader to stop reading it.
func watchInjectText(command string, rep watch.Report) string {
	if !rep.Changed() {
		return ""
	}
	command = redactCredentialText(command)
	var b strings.Builder
	if rep.WentGreen {
		fmt.Fprintf(&b, "[watch] The user's verifier `%s` now passes.", command)
	} else {
		fmt.Fprintf(&b, "[watch] The user's verifier `%s` ran after your edits and reports new failures (exit %d):\n", command, rep.ExitCode)
		for i, f := range rep.New {
			if i == watchInjectLines {
				fmt.Fprintf(&b, "…and %d more new failures\n", len(rep.New)-watchInjectLines)
				break
			}
			b.WriteString(redactCredentialTextBeforeTruncate(f.Line, 200) + "\n")
		}
		if rep.Persisting > 0 {
			fmt.Fprintf(&b, "%d earlier failure(s) persist.\n", rep.Persisting)
		}
	}
	text := strings.TrimRight(b.String(), "\n")
	// Verifier output is exactly the surface an env dump leaks a key
	// through, and a round boundary has no one to ask, so this redacts
	// unconditionally — the race record's posture.
	return redactCredentialText(text)
}

// watchTurnEnd covers the edits of a turn's final round, which no later
// round boundary will see. It runs off the UI goroutine and its report
// waits for the next prompt: the turn is over, so there is no request to
// inject into and no escalation left to feed.
func (m *tuiModel) prepareWatchTurnEnd() (watchRunTicket, bool) {
	if m.app.undo == nil {
		return watchRunTicket{}, false
	}
	ticket, _, ok := m.app.watchSt.due(m.app.undo.PendingFiles())
	if !ok {
		return watchRunTicket{}, false
	}
	return ticket, true
}

func (m *tuiModel) watchTurnEnd() tea.Cmd {
	ticket, ok := m.prepareWatchTurnEnd()
	if !ok {
		return nil
	}
	return m.watchTurnEndTicket(ticket, context.Background())
}

func (m *tuiModel) watchTurnEndTicket(ticket watchRunTicket, owner context.Context) tea.Cmd {
	ctx, ok := m.app.watchSt.backgroundContextFrom(ticket, owner)
	if !ok {
		return nil
	}
	return func() tea.Msg {
		defer m.app.watchSt.finish(ticket)
		// Deliberately not the completed turn's context: this run reports what
		// that turn left behind. The short-lived watch operation now owns it, so
		// a later esc or TUI exit can still cancel without reviving the old turn.
		rep, current := m.app.watchSt.execute(ticket, ctx)
		if !current {
			return nil
		}
		msg := watchReportMsg{command: redactedWatchCommand(ticket.watch.Command()), rep: rep, turnEnd: true, ticket: ticket}
		// Program.Send hands the report to the event loop before the FIFO link is
		// released, preserving report order as well as baseline order. Tests that
		// execute the command without a Program still receive the message directly.
		if m.app.p != nil {
			m.app.p.Send(msg)
			return nil
		}
		return msg
	}
}

// startTurnEndWatch claims the ordinary exclusive-operation gate before the
// verifier starts. The event loop stays responsive, while turns, shell work,
// compaction, and deferred startup remain behind the completed tree snapshot.
func (m *tuiModel) startTurnEndWatch(suppressAutoCompact bool) tea.Cmd {
	return m.startTurnEndWatchWithHook(suppressAutoCompact, nil)
}

// startTurnEndWatchWithHook keeps a deterministic invalidation seam between
// ticket preparation and background-context binding. Production passes nil;
// tests use it to prove that session/declaration invalidation cannot strand
// the completed turn's continuation or the verifier FIFO.
func (m *tuiModel) startTurnEndWatchWithHook(suppressAutoCompact bool, beforeBind func(watchRunTicket)) tea.Cmd {
	ticket, ok := m.prepareWatchTurnEnd()
	if !ok {
		return nil
	}
	owner, operation, sourceID, err := m.startOperation("watch verifier")
	if err != nil {
		m.app.watchSt.finish(ticket)
		m.addNotice("warn", "watch could not claim the completed turn: "+err.Error())
		return nil
	}
	if beforeBind != nil {
		beforeBind(ticket)
	}
	run := m.watchTurnEndTicket(ticket, owner)
	if run == nil {
		m.finishOperation(operation, false)
		if next := m.continueAfterTurnEnd(suppressAutoCompact); next != nil {
			return next
		}
		// onTurnDone treats nil as "no watch was due" and would call the
		// continuation a second time. A no-op command records that this path
		// already consumed the completed-turn handoff.
		return func() tea.Msg { return nil }
	}
	return m.ownOperationCmdWithAbandon(operation, func() tea.Msg {
		return turnEndWatchDoneMsg{
			operation: operation, sourceID: sourceID,
			suppressAutoCompact: suppressAutoCompact,
			report:              run(),
		}
	}, func() error {
		m.app.watchSt.finish(ticket)
		return nil
	})
}

func (m *tuiModel) onTurnEndWatchDone(msg turnEndWatchDoneMsg) tea.Cmd {
	if !m.operationMatches(msg.operation, msg.sourceID) {
		return nil
	}
	if report, ok := msg.report.(watchReportMsg); ok {
		m.onWatchReport(report)
	}
	cancelled := m.operationCancelling
	m.finishOperation(msg.operation, false)
	if cancelled {
		m.addNotice("", "watch cancelled; continuing from the completed turn")
	}
	return m.continueAfterTurnEnd(msg.suppressAutoCompact)
}

// onWatchReport renders a run's outcome. The transcript speaks only on a
// change; the status chip always tells the current color.
func (m *tuiModel) onWatchReport(msg watchReportMsg) {
	if !m.app.watchSt.reportCurrent(msg.ticket) {
		return
	}
	rep := msg.rep
	if msg.turnEnd && rep.Changed() {
		if text := watchInjectText(msg.command, rep); text != "" {
			m.app.watchSt.addFold(text)
		}
	}
	if rep.Err != nil {
		m.watchFails = -1
		m.addNotice("warn", redactCredentialText(fmt.Sprintf("watch: %s could not run: %v", msg.command, rep.Err)))
		return
	}
	if rep.Passed {
		m.watchFails = 0
		if rep.WentGreen || rep.FirstRun {
			m.addNotice("watch", redactCredentialText(fmt.Sprintf("watch: %s is green", msg.command)))
		}
		return
	}
	m.watchFails = len(rep.Signatures)
	if len(rep.New) > 0 {
		text := fmt.Sprintf("watch: %s — new failure: %s",
			redactCredentialText(msg.command), redactCredentialTextBeforeTruncate(rep.New[0].Line, 120))
		if extra := len(rep.New) - 1 + rep.Persisting; extra > 0 {
			text += fmt.Sprintf(" (+%d more)", extra)
		}
		m.addNotice("warn", redactCredentialText(text))
		// The moment a verifier turns red at a turn's end is the moment
		// "which turn broke it" becomes askable, so /bisect is named here
		// once — with turns to search, and never again this session,
		// because a lesson repeated is noise.
		if msg.turnEnd && !m.bisectHinted && m.app.undo != nil && len(m.app.undo.Turns()) > 1 {
			m.bisectHinted = true
			m.addNotice("", "/bisect can name the turn that broke it")
		}
	}
}

// watchContext folds a turn-end verdict into the next prompt, the same seam
// advice and ! output use and for the same reason: one user message per
// turn. The typed prompt leads and the report follows, so an opening never
// leads with the injection label — which is what lets /retry's shape check
// for unmarked logs stay honest.
func (m *tuiModel) watchContext(prompt string) string {
	folds := m.app.watchSt.takeFold()
	if len(folds) == 0 {
		return prompt
	}
	var b strings.Builder
	b.WriteString(prompt)
	for _, f := range folds {
		b.WriteString("\n\n" + f)
	}
	return b.String()
}

// watchChip is the status bar's readout: green when the last run passed,
// the failure count when it did not, a question mark when the verifier
// itself could not run.
func (m *tuiModel) watchChip() string {
	if m.app.watchSt.armed() == nil {
		return ""
	}
	th := m.th
	switch {
	case m.watchFails < 0:
		return th.onBar(th.warn).Render("watch ?")
	case m.watchFails == 0:
		return th.onBar(th.ok).Render("watch ✓")
	default:
		return th.onBar(th.err).Render(fmt.Sprintf("watch ✗%d", m.watchFails))
	}
}

// suggestVerifier names the verifier this workspace's own files imply, for
// the bare /watch hint. It suggests and never arms: the constraint is that
// a verifier is declared, not inferred, so the user's typing stays the only
// way one starts running. A Makefile's test target outranks the language
// manifests because it is the project's own declaration rather than an
// implication.
func suggestVerifier(workspace string) string {
	return suggestVerifierWithHook(workspace, nil)
}

func suggestVerifierWithHook(workspace string, beforeOpen func(string)) string {
	const maxSuggestionManifestBytes = int64(1 << 20)
	read := func(name string) string {
		path := filepath.Join(workspace, name)
		var hook func()
		if beforeOpen != nil {
			hook = func() { beforeOpen(name) }
		}
		data, err := readWorkspaceFileBounded(workspace, path, maxSuggestionManifestBytes, hook)
		if err != nil {
			return ""
		}
		return string(data)
	}
	if makeTestTarget.MatchString(read("Makefile")) {
		return "make test"
	}
	if read("go.mod") != "" {
		return "go test ./..."
	}
	if read("Cargo.toml") != "" {
		return "cargo test"
	}
	if read("pytest.ini") != "" || strings.Contains(read("pyproject.toml"), "[tool.pytest") {
		return "pytest"
	}
	if pkg := read("package.json"); pkg != "" {
		var manifest struct {
			Scripts map[string]string `json:"scripts"`
		}
		// The npm placeholder script fails on purpose; suggesting it would
		// arm a verifier that is red by construction.
		if err := json.Unmarshal([]byte(pkg), &manifest); err == nil {
			if s := manifest.Scripts["test"]; s != "" && !strings.Contains(s, "no test specified") {
				return "npm test"
			}
		}
	}
	return ""
}

var makeTestTarget = regexp.MustCompile(`(?m)^test:`)

func cmdWatch(m *tuiModel, args string) tea.Cmd {
	args = strings.TrimSpace(args)
	switch {
	case args == "":
		w := m.app.watchSt.armed()
		if w == nil {
			hint := "/watch <command> arms one"
			if s := suggestVerifier(m.app.workspace); s != "" {
				hint = fmt.Sprintf("this workspace implies `%s`; /watch %s arms it", s, s)
			}
			m.addInfo("  no watch set; " + hint + " — it runs after the model's edits, and only changes are reported")
			return nil
		}
		state := "green"
		if w.Red() {
			state = "failing"
		} else if m.watchFails < 0 {
			state = "could not run"
		}
		m.addInfo(fmt.Sprintf("  watching: %s  (%s; /watch off stops)", redactedWatchCommand(w.Command()), state))
		return nil

	case args == "off":
		if w := m.app.watchSt.disarm(); w != nil {
			m.watchFails = 0
			command := redactedWatchCommand(w.Command())
			m.app.loop.Session.AppendNote("info", "watch disarmed: "+command)
			return noticeCmd("", "watch off; "+command+" no longer runs")
		}
		return noticeCmd("", "no watch was set")

	default:
		if m.app.undo == nil {
			return noticeCmd("error", "watch needs the turn checkpoint recorder, which this session does not have")
		}
		m.app.watchSt.arm(watch.New(args, m.app.workspace))
		m.watchFails = 0
		command := redactedWatchCommand(args)
		m.app.loop.Session.AppendNote("info", "watch armed: "+command)
		m.addNotice("watch", fmt.Sprintf("watching with `%s`: it runs after the model's edits, unconfined, as you would run it; only changes are reported, and new failures count toward escalation", command))
		return nil
	}
}
