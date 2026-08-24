package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/terminaltext"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// renderer writes a turn to a terminal. It tracks what kind of output it last
// wrote so that model text, tool lines, and notices stay visually separate
// without the loop having to know about any of it.
type renderer struct {
	w     *bufio.Writer
	color bool
	mu    sync.Mutex

	lastKind  string
	atLineTop bool
}

func newRenderer(f *os.File) *renderer {
	return &renderer{
		w:         bufio.NewWriter(f),
		color:     useColor(f),
		atLineTop: true,
	}
}

func useColor(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

const (
	reset = "\x1b[0m"
	dim   = "\x1b[2m"
	bold  = "\x1b[1m"
	red   = "\x1b[31m"
)

func (r *renderer) style(code, s string) string {
	s = terminaltext.Escape(s)
	return r.styleRendered(code, s)
}

// styleRendered is reserved for text already passed through terminaltext
// while retaining intentional layout, such as streamed model prose.
func (r *renderer) styleRendered(code, s string) string {
	if !r.color {
		return s
	}
	return code + s + reset
}

// The *Locked helpers are the only code that touches the buffered writer or
// its layout state. Callers that need one indivisible terminal transaction —
// notably approval prompts — hold mu and use these directly. The small public
// helpers take the lock for legacy REPL call sites that write outside a turn.
func (r *renderer) flushLocked() { _ = r.w.Flush() }

func (r *renderer) flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushLocked()
}

// section separates a new kind of output from whatever came before it.
func (r *renderer) sectionLocked(kind string) {
	if r.lastKind == kind {
		return
	}
	if !r.atLineTop {
		r.w.WriteByte('\n')
		r.atLineTop = true
	}
	if r.lastKind != "" {
		r.w.WriteByte('\n')
	}
	r.lastKind = kind
}

func (r *renderer) writeLocked(s string) {
	if s == "" {
		return
	}
	r.w.WriteString(s)
	r.atLineTop = strings.HasSuffix(s, "\n")
}

func (r *renderer) lineLocked(s string) {
	if !r.atLineTop {
		r.w.WriteByte('\n')
	}
	r.w.WriteString(s)
	r.w.WriteByte('\n')
	r.atLineTop = true
}

func (r *renderer) line(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lineLocked(s)
}

func (r *renderer) ThinkingDelta(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sectionLocked("thinking")
	r.writeLocked(r.styleRendered(dim, terminaltext.Display(text)))
	r.flushLocked()
}

func (r *renderer) TextDelta(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sectionLocked("text")
	r.writeLocked(terminaltext.Display(text))
	r.flushLocked()
}

func (r *renderer) ToolStart(call provider.ToolUse, req permission.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sectionLocked("tool")
	r.lineLocked(r.style(bold, call.Name) + " " + r.style(dim, describeRequest(req)))
	r.flushLocked()
}

func (r *renderer) ToolEnd(call provider.ToolUse, req permission.Request, res tools.Result, took time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	status := "ok in " + formatDuration(took)
	if res.IsError {
		status = "failed in " + formatDuration(took)
	}

	detail := firstLine(terminaltext.Display(res.Content))
	if detail != "" {
		status += ": " + detail
	}
	if task := delegateCompletionLabel(call, req); task != "" {
		status = task + " · " + status
	}
	if res.IsError {
		r.lineLocked("  " + r.style(red, status))
	} else {
		r.lineLocked("  " + r.style(dim, status))
	}
	r.flushLocked()
}

// delegateCompletionLabel correlates an end rail with the attributed start
// without repeating a model-controlled path or command. Forwarded delegate
// IDs have a process-owned task prefix; ordinary top-level calls are unchanged.
func delegateCompletionLabel(call provider.ToolUse, req permission.Request) string {
	taskID, _, delegated := strings.Cut(call.ID, "/")
	if !delegated || !strings.HasPrefix(taskID, "task-") {
		return ""
	}
	detail := strings.TrimSpace(req.Detail)
	if strings.HasPrefix(detail, "[") {
		if end := strings.IndexByte(detail, ']'); end >= 0 {
			return boundedApprovalText(terminaltext.Escape(detail[:end+1]), 80)
		}
	}
	return boundedApprovalText(terminaltext.Escape("["+taskID+"]"), 80)
}

func (r *renderer) ToolBatchEnd(context.Context) {}

func (r *renderer) Notice(level, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sectionLocked("notice")
	prefix := "  note: "
	if level == "warn" || level == "error" {
		prefix = "  " + level + ": "
	}
	r.lineLocked(r.styleRendered(dim, prefix+terminaltext.Display(text)))
	r.flushLocked()
}

func (r *renderer) TurnUsage(session.Usage) {}

// endTurn closes out whatever the turn was writing so the next prompt starts
// on a clean line.
func (r *renderer) endTurn() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.atLineTop {
		r.w.WriteByte('\n')
		r.atLineTop = true
	}
	r.lastKind = ""
	r.flushLocked()
}

func describeRequest(req permission.Request) string {
	if req.Effect == permission.EffectExecute {
		command := tools.Describe(req.Argv, req.Shell)
		if req.Detail != "" {
			return terminaltext.Escape(req.Detail) + " · " + command
		}
		return command
	}
	if req.Detail != "" {
		return terminaltext.Escape(req.Detail)
	}
	return terminaltext.Escape(req.Path)
}

func approvalDescription(req permission.Request) string {
	return boundedApprovalText(describeRequest(req), 160)
}

func approvalReason(reason string) string {
	return boundedApprovalText(terminaltext.Escape(reason), 120)
}

// boundedApprovalText keeps the executable, reach warnings, and decision
// controls in the visible modal even when model-controlled argv is enormous.
// The prefix identifies the executable and early flags; the tail retains the
// target/scope. The marker makes it impossible to mistake this for the full
// byte-for-byte command recorded in the durable audit.
func boundedApprovalText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	const markerRoom = 36
	content := limit - markerRoom
	if content < 16 {
		content = 16
	}
	head := content * 2 / 3
	tail := content - head
	omitted := len(runes) - head - tail
	return string(runes[:head]) + fmt.Sprintf(" … [%d chars omitted] … ", omitted) + string(runes[len(runes)-tail:])
}

// formatDuration keeps a fast tool from reporting "0s", which reads as a
// failure to measure rather than as speed.
func formatDuration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return d.Round(10 * time.Microsecond).String()
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	default:
		return d.Round(10 * time.Millisecond).String()
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	line, _, _ := strings.Cut(s, "\n")
	const width = 100
	runes := []rune(line)
	if len(runes) > width {
		return string(runes[:width]) + "..."
	}
	return line
}

// terminalAsker resolves a permission Ask against the same stdin the REPL
// reads. The loop is not reading input while a tool is pending, so there is no
// contention.
type terminalAsker struct {
	in  *bufio.Reader
	out *renderer
}

func (a *terminalAsker) Ask(_ context.Context, req permission.Request, out permission.Outcome) (permission.Response, error) {
	r := a.out
	// Hold the renderer for the complete prompt/read transaction. A sibling
	// delegate may finish a tool or emit a fallback notice while the user is
	// deciding; letting that output paint inside the prompt can make the visible
	// command disagree with the answer the input is about.
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sectionLocked("prompt")
	r.lineLocked(r.style(bold, "approve "+terminaltext.Escape(req.Tool)) + " " + approvalDescription(req))
	r.lineLocked(r.style(dim, "  "+approvalReason(out.Reason)))

	// Design principle 4: a prompt is not containment, and the moment the user
	// approves is the moment that has to be plain.
	if out.SandboxAbsent {
		r.lineLocked(r.style(dim, "  FULL HOST ACCESS: this command is not sandboxed; it can access files outside the workspace and the network"))
	}
	if req.Effect == permission.EffectExecute && req.Network {
		r.lineLocked(r.style(dim, "  FULL NETWORK ACCESS REQUESTED: this command can send workspace data off this machine"))
	}
	r.lineLocked("  [y] once   [a] " + permissionRememberHint(req) + "   [n] no")
	r.w.WriteString("  > ")
	r.atLineTop = false
	r.flushLocked()

	answer, err := a.in.ReadString('\n')
	if err != nil {
		if err == io.EOF {
			// No one is there to answer, so nothing is approved.
			r.lineLocked("")
			return permission.Response{}, nil
		}
		return permission.Response{}, err
	}

	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return permission.Response{Approved: true}, nil
	case "a", "always":
		return permission.Response{Approved: true, Remember: true}, nil
	default:
		return permission.Response{}, nil
	}
}

func permissionRememberHint(req permission.Request) string {
	if req.Effect != permission.EffectExternal {
		return "always, this exact command"
	}
	if req.Path != "" {
		return "allow " + terminaltext.Escape(req.Path) + " for the rest of the session"
	}
	return "allow this tool for the rest of the session"
}

// terminalQuestioner resolves the ask tool against the same stdin the REPL
// reads, the terminalAsker's arrangement. Anything that is not a number is
// the user's own answer, because the natural response to a question whose
// options do not fit is to just say so.
type terminalQuestioner struct {
	in  *bufio.Reader
	out *renderer
}

func (a *terminalQuestioner) AskUser(_ context.Context, q tools.Question) (tools.Answer, error) {
	r := a.out
	// Questions own stdin just like permission prompts, so keep their full
	// render/read cycle exclusive too.
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sectionLocked("question")
	r.lineLocked(r.style(bold, terminaltext.Escape(q.Question)))
	for i, opt := range q.Options {
		line := "  [" + strconv.Itoa(i+1) + "] " + terminaltext.Escape(opt.Label)
		if opt.Detail != "" {
			line += "  " + r.style(dim, terminaltext.Escape(opt.Detail))
		}
		r.lineLocked(line)
	}
	hint := "  a number chooses; anything else is your own answer; enter alone declines"
	if q.Multi {
		hint = "  numbers choose, space-separated; anything else is your own answer; enter alone declines"
	}
	r.lineLocked(r.style(dim, hint))
	r.w.WriteString("  > ")
	r.atLineTop = false
	r.flushLocked()

	answer, err := a.in.ReadString('\n')
	if err != nil {
		if err == io.EOF {
			// No one is there to answer, which is a decline, not a crash:
			// the model hears it and continues on its own judgment.
			r.lineLocked("")
			return tools.Answer{Declined: true}, nil
		}
		return tools.Answer{}, err
	}
	r.atLineTop = true
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return tools.Answer{Declined: true}, nil
	}
	if picked, ok := parseQuestionPicks(answer, q); ok {
		return tools.Answer{Picked: picked}, nil
	}
	return tools.Answer{Text: answer}, nil
}

// parseQuestionPicks reads an answer as option numbers. Every token must be
// a number in range — one on a single-select question — or the whole answer
// is the user's own words instead; a half-numeric guess must not silently
// become a pick. Picks come back in offered order, the shape the model asked
// the question in.
func parseQuestionPicks(answer string, q tools.Question) ([]string, bool) {
	fields := strings.FieldsFunc(answer, func(r rune) bool { return r == ' ' || r == ',' })
	if len(fields) == 0 || (!q.Multi && len(fields) > 1) {
		return nil, false
	}
	marked := make([]bool, len(q.Options))
	for _, f := range fields {
		if len(f) != 1 || f[0] < '1' || f[0] > '9' {
			return nil, false
		}
		i := int(f[0] - '1')
		if i >= len(q.Options) {
			return nil, false
		}
		marked[i] = true
	}
	var out []string
	for i, opt := range q.Options {
		if marked[i] {
			out = append(out, opt.Label)
		}
	}
	return out, true
}
