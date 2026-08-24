package main

// The ! prefix runs a shell command as the user, immediately, with no model
// in the loop. It exists because "let me just check something" should not
// cost a turn, a permission dialog, or a context switch to another terminal.
// The output lands in the transcript right away and is carried into the next
// turn's prompt, so the model sees what the user saw — one user message per
// turn, which keeps every adapter's view of the conversation well-formed.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/execution"
)

const (
	shellTimeout               = 60 * time.Second
	shellOutputCap             = 16 << 10
	shellCredentialScanOverlap = 256
	shellCaptureCap            = shellOutputCap + shellCredentialScanOverlap
)

type shellDoneMsg struct {
	command        string
	output         string
	contextCommand string
	contextOutput  string
	err            error
	took           time.Duration
	result         shellResult
	operation      uint64
	sourceID       string
}

type shellResultKind string

const (
	shellSucceeded shellResultKind = "success"
	shellExited    shellResultKind = "exit"
	shellSignaled  shellResultKind = "signal"
	shellTimedOut  shellResultKind = "timeout"
	shellCancelled shellResultKind = "cancelled"
	shellFailed    shellResultKind = "error"
)

// shellResult is kept separate from command output so the human transcript
// cannot lose the verdict in a long stderr tail and the next model turn gets
// the same structured fact. Output remains raw until a rendering boundary,
// where terminaltext.Display makes control bytes visible and inert.
type shellResult struct {
	kind     shellResultKind
	exitCode int
	signal   string
	detail   string
}

// shellOutput remains safe to snapshot if cancellation reaches the bounded
// cleanup return before an unkillable or deliberately detached descendant
// releases os/exec's copy pipe.
type shellOutput struct {
	mu        sync.Mutex
	buf       []byte
	truncated bool
}

func (o *shellOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	written := len(p)
	remaining := shellCaptureCap - len(o.buf)
	if remaining > 0 {
		keep := min(remaining, len(p))
		o.buf = append(o.buf, p[:keep]...)
	}
	if len(p) > remaining {
		o.truncated = true
	}
	// os/exec treats a short write as a pipe failure. Bytes beyond the bounded
	// head are deliberately discarded, so acknowledge the caller's full chunk.
	return written, nil
}

func (o *shellOutput) String() string {
	text, _ := o.snapshot()
	return text
}

func (o *shellOutput) snapshot() (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return string(append([]byte(nil), o.buf...)), o.truncated
}

func classifyShellResult(err, contextErr error) shellResult {
	if err == nil {
		return shellResult{kind: shellSucceeded, exitCode: 0}
	}
	if errors.Is(contextErr, context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return shellResult{kind: shellTimedOut, detail: shellCancellationLimit()}
	}
	if errors.Is(contextErr, context.Canceled) || errors.Is(err, context.Canceled) {
		return shellResult{kind: shellCancelled, detail: shellCancellationLimit()}
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			sig := status.Signal()
			return shellResult{kind: shellSignaled, signal: fmt.Sprintf("%d (%s)", sig, sig)}
		}
		if code := exitErr.ExitCode(); code >= 0 {
			return shellResult{kind: shellExited, exitCode: code}
		}
	}
	return shellResult{kind: shellFailed, detail: err.Error()}
}

func shellCancellationLimit() string {
	if runtime.GOOS == "windows" {
		return "only the direct shell was stopped; descendant processes may still be running"
	}
	return ""
}

func (r shellResult) failed() bool {
	return r.kind != shellSucceeded
}

func (r shellResult) summary() string {
	withDetail := func(summary string) string {
		if r.detail == "" {
			return summary
		}
		return summary + "; " + r.detail
	}
	switch r.kind {
	case shellSucceeded:
		return "exit 0 (success)"
	case shellExited:
		return fmt.Sprintf("exit %d (failure)", r.exitCode)
	case shellSignaled:
		return "terminated by signal " + r.signal
	case shellTimedOut:
		return withDetail("timed out after " + shellTimeout.String())
	case shellCancelled:
		return withDetail("cancelled by user")
	default:
		if r.detail != "" {
			return "could not run: " + r.detail
		}
		return "could not run"
	}
}

func (r shellResult) contextRecord() string {
	cleanup := ""
	if r.detail != "" && (r.kind == shellTimedOut || r.kind == shellCancelled) {
		cleanup = "; cleanup=direct-shell-only; descendants_may_survive=true"
	}
	switch r.kind {
	case shellSucceeded:
		return "[shell result: success; exit_code=0]"
	case shellExited:
		return fmt.Sprintf("[shell result: failure; exit_code=%d]", r.exitCode)
	case shellSignaled:
		return "[shell result: signal; signal=" + r.signal + "]"
	case shellTimedOut:
		return "[shell result: timeout; limit=" + shellTimeout.String() + cleanup + "]"
	case shellCancelled:
		return "[shell result: cancelled" + cleanup + "]"
	default:
		return "[shell result: error; detail=" + r.detail + "]"
	}
}

func (r shellResult) transcriptDetail(output string) string {
	output = strings.TrimRight(output, "\r\n")
	if output == "" {
		return r.summary()
	}
	if r.failed() {
		// Failed tool entries expose their tail while collapsed. Put the
		// verdict last so even a capped, noisy stderr cannot push it away.
		return output + "\n" + r.summary()
	}
	// Successful entries expose their first detail line while collapsed;
	// expansion reveals stdout and stderr beneath the explicit verdict.
	return r.summary() + "\n" + output
}

func shellToolID(operation uint64) string {
	return fmt.Sprintf("shell:%d", operation)
}

func capShellOutput(text string) string {
	return capShellOutputWithDiscard(text, false)
}

func capShellOutputWithDiscard(text string, discarded bool) string {
	bounded, truncated := truncateBytesAtGrapheme(text, shellOutputCap)
	if !truncated && !discarded {
		return bounded
	}
	return bounded + fmt.Sprintf("\n[truncated at %d-byte limit]", shellOutputCap)
}

// runShell executes the command through the user's shell in the workspace.
// This is deliberately not the sandboxed tool path: the user typed it, which
// is the same authority as typing it into the terminal next door. The agent's
// own commands still go through permissions; this never becomes its escape
// hatch because the loop cannot reach it.
func (m *tuiModel) runShell(command string) tea.Cmd {
	if command == "" {
		return noticeCmd("error", "nothing to run after !")
	}
	if m.busy {
		return noticeCmd("warn", "a turn is running; ! commands wait for the prompt")
	}
	opCtx, generation, sourceID, err := m.startOperation("shell")
	if err != nil {
		return noticeCmd("warn", err.Error())
	}

	// rank -1: the user ran this, not a rung, so the rail stays neutral.
	m.tr.add(&entry{kind: kindTool, tool: toolEntry{id: shellToolID(generation), name: "shell", desc: command}, rank: -1})
	m.tr.scrollToBottom()

	workspace := m.app.workspace
	return m.ownOperationCmd(generation, func() tea.Msg {
		ctx, cancel := context.WithTimeout(opCtx, shellTimeout)
		defer cancel()

		start := time.Now()
		var out shellOutput
		cmd := newUserShellCommand(command)
		cmd.Dir = workspace
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := execution.RunProcess(ctx, cmd)
		took := time.Since(start)

		output, discarded := out.snapshot()
		return shellDoneMsg{
			command: command, output: capShellOutputWithDiscard(output, discarded),
			// Provider/session context gets the unattended-output posture. Scan
			// the complete values before the byte cap so a boundary-straddling
			// credential cannot become an undetectable partial prefix.
			contextCommand: redactCredentialText(command),
			contextOutput:  capShellOutputWithDiscard(redactCredentialText(output), discarded),
			err:            err, took: took,
			result: classifyShellResult(err, ctx.Err()), operation: generation, sourceID: sourceID,
		}
	})
}

func (m *tuiModel) onShellDone(msg shellDoneMsg) tea.Cmd {
	// Production shell runs always carry an operation. The zero case keeps
	// direct synthetic calls useful in focused prompt-construction tests.
	if msg.operation != 0 && !m.operationMatches(msg.operation, msg.sourceID) {
		return nil
	}
	result := msg.result
	if result.kind == "" {
		result = classifyShellResult(msg.err, nil)
	}

	idx := -1
	for i := len(m.tr.entries) - 1; i >= 0; i-- {
		e := m.tr.entries[i]
		if e.kind != kindTool || e.tool.done || e.tool.name != "shell" {
			continue
		}
		if msg.operation == 0 || e.tool.id == shellToolID(msg.operation) {
			idx = i
			break
		}
	}
	if idx >= 0 {
		e := m.tr.entries[idx]
		e.tool.done = true
		e.tool.failed = result.failed()
		e.tool.took = msg.took
		e.tool.detail = result.transcriptDetail(msg.output)
		e.cache = nil
		m.tr.invalidate(idx)
		m.tr.scrollToBottom()
	}

	contextCommand := msg.contextCommand
	if contextCommand == "" {
		contextCommand = redactCredentialText(msg.command)
	}
	contextOutput := msg.contextOutput
	if contextOutput == "" && msg.output != "" {
		contextOutput = redactCredentialText(msg.output)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "$ %s\n%s", contextCommand, strings.TrimRight(contextOutput, "\r\n"))
	fmt.Fprintf(&b, "\n%s", redactCredentialText(result.contextRecord()))
	m.pendingShell = append(m.pendingShell, b.String())
	// A shell command has full user authority and can mutate the tree even when
	// it exits non-zero or is cancelled halfway through. Its completion is the
	// first sound point to retire every source surface and reject loads that
	// began against the old tree.
	invalidateRestoredWorkspace(m)
	if msg.operation == 0 {
		return nil
	}
	m.finishOperation(msg.operation, false)
	return m.nextQueuedTurn()
}

// shellContext drains what ! commands produced into the next prompt. The
// session records the augmented prompt, so a replayed or resumed session
// carries the same context the model actually saw.
func (m *tuiModel) shellContext(prompt string) string {
	if len(m.pendingShell) == 0 {
		return prompt
	}
	var b strings.Builder
	b.WriteString("I ran these shell commands in the workspace just now:\n\n")
	for _, s := range m.pendingShell {
		b.WriteString(s + "\n\n")
	}
	b.WriteString(prompt)
	m.pendingShell = nil
	return b.String()
}
