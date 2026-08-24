package main

// /bisect: binary-search this session's recorded turns for the one that
// turned the verifier red. The states probed are the checkpoint
// recorder's own pre-images — the same evidence /undo restores from — and
// the verifier is declared, never inferred: the /watch command if one is
// armed, or the command given to /bisect itself. The workspace is
// mutated in place, one reconstruction at a time, and put back on every
// exit path; while it runs the session is busy, so no turn can edit the
// tree mid-probe and prompts queue the way they do behind a turn.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/bisect"
	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/execution"
	route "github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/watch"
)

type bisectRun struct {
	command   string
	labels    []string
	cancel    context.CancelFunc
	cancelled bool
	// done closes only after Runner.Run has returned, including its mandatory
	// deferred workspace restore. Abnormal terminal teardown waits for it before
	// allowing the process to exit; cancellation without that join can strand the
	// checkout at the last probed historical state.
	done      chan struct{}
	resultErr error
	rail      *entry
}

type bisectProbeMsg struct {
	state  int // turn boundary being reconstructed; len(labels) means the current state
	probes int
}

type bisectDoneMsg struct {
	res bisect.Result
	err error
}

func cmdBisect(m *tuiModel, args string) tea.Cmd {
	if m.bisect != nil {
		return noticeCmd("warn", "a bisect is already running; esc cancels it")
	}
	command := strings.TrimSpace(args)
	if command == "" {
		if w := m.app.watchSt.armed(); w != nil {
			command = w.Command()
		}
	}
	if command == "" {
		return noticeCmd("error", "/bisect takes the verifier to run, or /watch <cmd> declares one it can use")
	}

	turns := m.app.undo.Turns()
	if len(turns) == 0 {
		return noticeCmd("", "no turn has changed files; there is nothing to bisect over")
	}
	for i, t := range turns {
		if t.Partial {
			return noticeCmd("error", fmt.Sprintf("turn %d (%q) passed the snapshot cap; a bisect over it would restore half a turn",
				i+1, redactCredentialText(t.Label)))
		}
	}

	states := make([]map[string]checkpoint.FileState, len(turns))
	labels := make([]string, len(turns))
	for i := range turns {
		states[i] = m.app.undo.StateBefore(i)
		labels[i] = turns[i].Label
	}
	workspace := m.app.workspace
	if m.app.store == nil {
		return noticeCmd("error", "bisect cleanup storage is unavailable")
	}
	journalDir, err := m.app.store.WorkspaceDir(workspace)
	if err != nil {
		return noticeCmd("error", redactCredentialText("opening bisect cleanup storage failed: "+err.Error()))
	}

	ctx, cancel := context.WithCancel(context.Background())
	run := &bisectRun{command: command, labels: labels, cancel: cancel, done: make(chan struct{})}
	run.rail = m.tr.add(&entry{kind: kindInfo, text: bisectRailLine(command, len(labels), -1, 0)})
	m.tr.scrollToBottom()
	m.bisect = run
	m.busy = true
	m.started = time.Now()
	// The spinner shares the turn's status line; a stale token rate from
	// the last turn must not tick along under a bisect that streams none.
	m.samples, m.tokChars, m.tokAt = nil, 0, time.Time{}

	send := func(msg tea.Msg) {
		if m.app.p != nil {
			m.app.p.Send(msg)
		}
	}
	runner := &bisect.Runner{
		Workspace:  workspace,
		JournalDir: journalDir,
		States:     states,
		Verify:     func(ctx context.Context) bisect.Verdict { return runVerifier(ctx, command, workspace) },
		OnProbe: func(state, probes int) {
			send(bisectProbeMsg{state: state, probes: probes})
		},
	}
	go func() {
		defer close(run.done)
		res, err := runner.Run(ctx)
		run.resultErr = err
		send(bisectDoneMsg{res: res, err: err})
	}()
	return m.spin.Tick
}

// finishBisectForExit is the process-exit half of the bisect contract. A
// Bubble Tea program may stop without accepting the final message, but the
// checkout must not be allowed to exit in a reconstructed historical state.
func (m *tuiModel) finishBisectForExit(run *bisectRun) {
	if run == nil {
		return
	}
	run.cancelled = true
	if run.cancel != nil {
		run.cancel()
	}
	if run.done != nil {
		<-run.done
	}
	if m.bisect == run {
		m.bisect = nil
		m.busy = false
	}
	var restoreFail *bisect.RestoreError
	if errors.As(run.resultErr, &restoreFail) {
		m.shutdownErr = errors.Join(m.shutdownErr,
			fmt.Errorf("restoring workspace while the TUI stopped: %w", restoreFail))
	}
}

// runVerifier is one probe: the declared command, unconfined through the
// user's shell in the workspace, the same authority /watch runs with.
func runVerifier(ctx context.Context, command, workspace string) bisect.Verdict {
	res, err := execution.Run(ctx, execution.Command{
		Argv:      []string{command},
		Shell:     true,
		Dir:       workspace,
		Timeout:   watch.DefaultTimeout,
		MaxOutput: 32 << 10,
	})
	if err != nil {
		return bisect.Verdict{Err: err}
	}
	if res.ExitCode == 0 && !res.TimedOut {
		return bisect.Verdict{Passed: true}
	}
	if res.Truncated {
		// The capture retained only fragments around an omitted middle. A
		// failure token or credential can cross that boundary, so do not parse
		// or render either fragment as a verifier diagnostic.
		return bisect.Verdict{FirstFail: "verifier output exceeded its bounded capture and was withheld"}
	}
	fail := ""
	if failures := route.ExtractFailures(res.Output); len(failures) > 0 {
		fail = failures[0].Line
	}
	if fail == "" {
		for _, line := range strings.Split(res.Output, "\n") {
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				fail = trimmed
				break
			}
		}
	}
	if res.TimedOut {
		fail = fmt.Sprintf("timed out after %s", watch.DefaultTimeout)
	}
	if fail == "" {
		fail = fmt.Sprintf("exited %d with no output", res.ExitCode)
	}
	return bisect.Verdict{FirstFail: fail}
}

// bisectInjectText is the verdict shaped for the model. The failure line
// came out of verifier output, exactly the surface an env dump leaks a
// key through, and a fold has no one to ask — so it redacts
// unconditionally, the watch injection's posture.
func bisectInjectText(command, label string, res bisect.Result) string {
	text := fmt.Sprintf(
		"[bisect] The user bisected this session's checkpoints against `%s`. Turn %d (%q) is where it turned red — green just before it, red ever since. First failure:\n%s",
		redactCredentialText(command), res.Culprit+1, redactCredentialText(label),
		redactCredentialTextBeforeTruncate(res.Fail.FirstFail, 200))
	return redactCredentialText(text)
}

func bisectRailLine(command string, span, state, probes int) string {
	command = redactCredentialText(command)
	where := "the current state"
	if state >= 0 && state < span {
		where = fmt.Sprintf("before turn %d of %d", state+1, span)
	}
	if probes == 0 && state < 0 {
		return fmt.Sprintf("bisect — %s over %d recorded turns", command, span)
	}
	return fmt.Sprintf("bisect — %s · probe %d, %s", command, probes+1, where)
}

func (m *tuiModel) onBisectProbe(msg bisectProbeMsg) {
	if m.bisect == nil {
		return
	}
	state := msg.state
	if state >= len(m.bisect.labels) {
		state = -1
	}
	m.bisect.rail.text = bisectRailLine(m.bisect.command, len(m.bisect.labels), state, msg.probes)
	if idx := m.tr.indexOf(m.bisect.rail); idx >= 0 {
		m.tr.invalidate(idx)
	}
}

func (m *tuiModel) onBisectDone(msg bisectDoneMsg) tea.Cmd {
	run := m.bisect
	if run == nil {
		return nil
	}
	m.bisect = nil
	m.busy = false
	run.cancel()

	summary := ""
	var restoreFail *bisect.RestoreError
	switch {
	case errors.As(msg.err, &restoreFail):
		// The one message that must never soften: whatever else ended the
		// run — cancellation included — the tree may sit at a past state,
		// and saying "restored" here would be the lie the contract exists
		// to prevent.
		summary = redactCredentialText("bisect: " + restoreFail.Error() + " — /diff shows what stands")
		m.addNotice("error", summary)
	case run.cancelled || errors.Is(msg.err, context.Canceled):
		summary = "bisect cancelled; the workspace is restored"
		m.addNotice("", summary)
	case msg.err != nil:
		summary = redactCredentialText("bisect: " + msg.err.Error())
		m.addNotice("error", summary)
	case msg.res.Outcome == bisect.AlreadyGreen:
		summary = fmt.Sprintf("bisect: %s passes as things stand; there is nothing to find", redactCredentialText(run.command))
		m.addNotice("", summary)
	case msg.res.Outcome == bisect.RedBeforeRecord:
		summary = fmt.Sprintf("bisect: red before the oldest recorded turn — the break predates what this session's checkpoints hold (%s)",
			redactCredentialText(msg.res.Fail.FirstFail))
		m.addNotice("warn", summary)
	default:
		label := run.labels[msg.res.Culprit]
		summary = fmt.Sprintf("bisect: turn %d of %d (%q) turned %s red — green just before it, red ever since",
			msg.res.Culprit+1, len(run.labels), redactCredentialText(label), redactCredentialText(run.command))
		m.addInfo(summary + "\n" +
			"  first failure: " + redactCredentialText(msg.res.Fail.FirstFail) + "\n" +
			fmt.Sprintf("  %d probes; reconstruction covers what write and edit captured — shell-made and hand-made changes rode along at today's state", msg.res.Probes))
		// The verdict folds behind the next typed prompt, the watch
		// posture: the user who now types "fix it" should not have to
		// restate what the machine just measured.
		m.app.watchSt.addFold(bisectInjectText(run.command, label, msg.res))
	}
	run.rail.text = summary
	if idx := m.tr.indexOf(run.rail); idx >= 0 {
		m.tr.invalidate(idx)
	}
	m.app.loop.Session.AppendNote("info", summary)

	// A queued prompt was written assuming the tree it queued against.
	// When the restore failed, that premise failed with it: draining now
	// would fire a turn at a past state in the same breath as the warning,
	// so the queue drops with its count instead of running.
	if restoreFail != nil {
		if dropped := len(m.queue); dropped > 0 {
			m.queue = nil
			word := "prompts"
			if dropped == 1 {
				word = "prompt"
			}
			m.addNotice("warn", fmt.Sprintf("%d queued %s dropped — the workspace needs your eyes before anything runs", dropped, word))
		}
		return nil
	}

	if len(m.queue) > 0 && !m.busy {
		next := m.queue[0]
		m.queue = m.queue[1:]
		return m.startTurn(next, "")
	}
	return nil
}
