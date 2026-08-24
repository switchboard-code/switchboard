package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/tools"
)

type blockingAnswerReader struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	answer  []byte
}

func (r *blockingAnswerReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.entered) })
	<-r.release
	if len(r.answer) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.answer)
	r.answer = r.answer[n:]
	return n, nil
}

type overlapDetectWriter struct {
	active  atomic.Int32
	overlap atomic.Bool
	mu      sync.Mutex
	buf     bytes.Buffer
}

func (w *overlapDetectWriter) Write(p []byte) (int, error) {
	if w.active.Add(1) != 1 {
		w.overlap.Store(true)
	}
	defer w.active.Add(-1)
	// Widen the underlying-write window so a missing renderer lock is a
	// deterministic failure instead of depending on the race detector alone.
	time.Sleep(200 * time.Microsecond)
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *overlapDetectWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestPermissionRememberHintMatchesRememberScope(t *testing.T) {
	execHint := permissionRememberHint(permission.Request{Tool: "exec", Effect: permission.EffectExecute})
	if !strings.Contains(execHint, "exact command") {
		t.Fatalf("exec remember hint = %q", execHint)
	}
	mcpHint := permissionRememberHint(permission.Request{Tool: "mcp__github__delete_issue", Effect: permission.EffectExternal})
	if strings.Contains(mcpHint, "exact") || !strings.Contains(mcpHint, "this tool") {
		t.Fatalf("MCP remember hint misstates per-tool scope: %q", mcpHint)
	}
	hostHint := permissionRememberHint(permission.Request{Tool: "webfetch", Effect: permission.EffectExternal, Path: "example.com"})
	if !strings.Contains(hostHint, "example.com") || strings.Contains(hostHint, "exact") {
		t.Fatalf("host-scoped remember hint = %q", hostHint)
	}
}

func TestDescribeRequestEscapesExternalControlSequences(t *testing.T) {
	got := describeRequest(permission.Request{Effect: permission.EffectExternal, Detail: "\x1b[2JAPPROVED\x07"})
	if strings.ContainsAny(got, "\x1b\x07") || !strings.Contains(got, `\x1b`) {
		t.Fatalf("unsafe external request description %q", got)
	}
}

func TestExecuteDescriptionKeepsTaskAttribution(t *testing.T) {
	got := describeRequest(permission.Request{
		Effect: permission.EffectExecute,
		Argv:   []string{"go", "test", "./..."},
		Detail: "[task-007 verify linux]",
	})
	if got != "[task-007 verify linux] · go test ./..." {
		t.Fatalf("description = %q", got)
	}
}

func TestREPLToolResultCannotWriteTerminalControls(t *testing.T) {
	var buf bytes.Buffer
	r := &renderer{w: bufio.NewWriter(&buf), atLineTop: true}
	r.ToolEnd(provider.ToolUse{Name: "exec"}, permission.Request{}, tools.Result{
		Content: "ok\x1b[2J\x1b]52;c;Y2xpcGJvYXJk\x07\rSPOOF",
	}, time.Millisecond)
	got := buf.String()
	if strings.ContainsAny(got, "\x1b\x07\r") || !strings.Contains(got, `\x1b`) {
		t.Fatalf("REPL rendered unsafe tool output: %q", got)
	}
}

func TestREPLModelAndNoticeTextCannotWriteTerminalControls(t *testing.T) {
	var buf bytes.Buffer
	r := &renderer{w: bufio.NewWriter(&buf), atLineTop: true}
	r.TextDelta("answer\x1b[2J")
	r.ThinkingDelta("thought\x1b]52;c;Y2xpcGJvYXJk\x07")
	r.Notice("error", "provider\rSPOOF")
	got := buf.String()
	if strings.ContainsAny(got, "\x1b\x07\r") || !strings.Contains(got, `\x1b`) {
		t.Fatalf("REPL rendered unsafe model/notice text: %q", got)
	}
}

func TestTerminalQuestionEscapesControlsWithoutChangingPickedLabel(t *testing.T) {
	question := "choose\n> forged prompt\x1b]2;forged title\a\u202e"
	label := "original\n  [9] forged\x1b[2J"
	detail := "detail\rrewritten\u2066"
	var buf bytes.Buffer
	r := &renderer{w: bufio.NewWriter(&buf), atLineTop: true}
	questioner := terminalQuestioner{
		in:  bufio.NewReader(strings.NewReader("1\n")),
		out: r,
	}
	answer, err := questioner.AskUser(context.Background(), tools.Question{
		Question: question,
		Options:  []tools.QuestionOption{{Label: label, Detail: detail}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(answer.Picked) != 1 || answer.Picked[0] != label {
		t.Fatalf("picked label was changed by display escaping: %+v", answer)
	}

	got := buf.String()
	for _, unsafe := range []string{"\x1b", "\a", "\r", "\u202e", "\u2066"} {
		if strings.Contains(got, unsafe) {
			t.Fatalf("question retained terminal control %q: %q", unsafe, got)
		}
	}
	if strings.Contains(got, "choose\n> forged prompt") || strings.Contains(got, "original\n  [9] forged") {
		t.Fatalf("model-authored newline created terminal structure: %q", got)
	}
	for _, escaped := range []string{`choose\x0a> forged prompt`, `original\x0a  [9] forged`, `\x1b`, `\u202e`} {
		if !strings.Contains(got, escaped) {
			t.Fatalf("question did not visibly escape %q: %q", escaped, got)
		}
	}
}

func TestConcurrentObserverWritesUseOneRendererTransaction(t *testing.T) {
	w := &overlapDetectWriter{}
	r := &renderer{w: bufio.NewWriter(w), atLineTop: true}

	const count = 24
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			marker := fmt.Sprintf("observer-%02d", i)
			switch i % 4 {
			case 0:
				r.TextDelta(marker + "\n")
			case 1:
				r.ThinkingDelta(marker + "\n")
			case 2:
				r.Notice("warn", marker)
			default:
				r.ToolStart(provider.ToolUse{Name: marker}, permission.Request{Detail: marker})
			}
		}()
	}
	wg.Wait()
	r.flush()

	if w.overlap.Load() {
		t.Fatal("renderer allowed concurrent writes to its buffered output")
	}
	got := w.String()
	for i := 0; i < count; i++ {
		marker := fmt.Sprintf("observer-%02d", i)
		if strings.Count(got, marker) == 0 {
			t.Fatalf("concurrent output lost %q:\n%s", marker, got)
		}
	}
}

func TestTerminalApprovalOwnsRendererUntilAnswer(t *testing.T) {
	input := &blockingAnswerReader{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		answer:  []byte("y\n"),
	}
	var buf bytes.Buffer
	r := &renderer{w: bufio.NewWriter(&buf), atLineTop: true}
	asker := terminalAsker{in: bufio.NewReader(input), out: r}

	type askResult struct {
		response permission.Response
		err      error
	}
	askDone := make(chan askResult, 1)
	go func() {
		response, err := asker.Ask(context.Background(), permission.Request{
			Tool: "exec", Effect: permission.EffectExecute,
			Argv: []string{"go", "test", "./..."}, Detail: "[task-004 verify]",
		}, permission.Outcome{Decision: permission.Ask, Reason: "runs a command"})
		askDone <- askResult{response: response, err: err}
	}()
	<-input.entered // the prompt is flushed and the asker is waiting on stdin
	if r.mu.TryLock() {
		r.mu.Unlock()
		t.Fatal("approval released the renderer while still waiting for its answer")
	}

	noticeStarted := make(chan struct{})
	noticeDone := make(chan struct{})
	go func() {
		close(noticeStarted)
		r.Notice("warn", "task-005 sibling finished")
		close(noticeDone)
	}()
	<-noticeStarted
	select {
	case <-noticeDone:
		t.Fatal("sibling output painted inside an active approval prompt")
	case <-time.After(20 * time.Millisecond):
	}
	close(input.release)
	result := <-askDone
	if result.err != nil || !result.response.Approved {
		t.Fatalf("approval result = %+v, %v", result.response, result.err)
	}
	<-noticeDone

	got := buf.String()
	promptAt := strings.Index(got, "[task-004 verify]")
	noticeAt := strings.Index(got, "task-005 sibling finished")
	if promptAt < 0 || noticeAt < 0 || noticeAt < promptAt {
		t.Fatalf("renderer did not preserve prompt-before-sibling order: %q", got)
	}
}

func TestInterleavedDelegateEndRailsKeepTaskAttribution(t *testing.T) {
	var buf bytes.Buffer
	r := &renderer{w: bufio.NewWriter(&buf), atLineTop: true}
	firstCall := provider.ToolUse{ID: "task-011/read-a", Name: "read"}
	firstReq := permission.Request{Detail: "[task-011 inspect api] /workspace/a.go"}
	secondCall := provider.ToolUse{ID: "task-012/read-b", Name: "read"}
	secondReq := permission.Request{Detail: "[task-012 inspect ui] /workspace/b.go"}

	// Starts overlap and completions arrive in the opposite order.
	r.ToolStart(firstCall, firstReq)
	r.ToolStart(secondCall, secondReq)
	r.ToolEnd(secondCall, secondReq, tools.Result{Content: "ui result"}, 2*time.Millisecond)
	r.ToolEnd(firstCall, firstReq, tools.Result{Content: "api result"}, 3*time.Millisecond)

	got := buf.String()
	for _, want := range []string{
		"[task-012 inspect ui] · ok in 2ms: ui result",
		"[task-011 inspect api] · ok in 3ms: api result",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("interleaved end rails lost %q:\n%s", want, got)
		}
	}
}

func TestApprovalViewsMakeFullNetworkRequestExplicit(t *testing.T) {
	req := permission.Request{Tool: "exec", Effect: permission.EffectExecute, Argv: []string{"go", "test", "./..."}, Network: true}
	outcome := permission.Outcome{Decision: permission.Ask, Reason: "runs a command"}

	var buf bytes.Buffer
	r := &renderer{w: bufio.NewWriter(&buf), atLineTop: true}
	asker := terminalAsker{in: bufio.NewReader(strings.NewReader("n\n")), out: r}
	if _, err := asker.Ask(context.Background(), req, outcome); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); !strings.Contains(got, "FULL NETWORK ACCESS REQUESTED") {
		t.Fatalf("REPL network prompt hid egress reach: %q", got)
	}

	m := testModel(t)
	dialog := newPermissionDialog(req, outcome, make(chan permission.Response, 1))
	if got := stripANSI(dialog.view(80, m.th)); !strings.Contains(got, "FULL NETWORK ACCESS REQUESTED") {
		t.Fatalf("TUI network prompt hid egress reach: %q", got)
	}
}

func TestTUIApprovalBoundsHugeArgvWithoutHidingExecutableOrChoices(t *testing.T) {
	req := permission.Request{
		Tool: "exec", Effect: permission.EffectExecute,
		Argv: []string{"dangerous-command", strings.Repeat("padding ", 600), "--target", "/outside/workspace"},
	}
	outcome := permission.Outcome{Decision: permission.Ask, Reason: strings.Repeat("review detail ", 100), SandboxAbsent: true}
	m := testModel(t)
	dialog := newPermissionDialog(req, outcome, make(chan permission.Response, 1))
	plain := stripANSI(dialog.view(80, m.th))
	if lines := strings.Count(plain, "\n") + 1; lines > 24 {
		t.Fatalf("approval modal is %d lines and can scroll its identity off a 24-line terminal:\n%s", lines, plain)
	}
	for _, visible := range []string{"dangerous-command", "chars omitted", "/outside/workspace", "FULL HOST ACCESS", "yes", "no"} {
		if !strings.Contains(plain, visible) {
			t.Fatalf("bounded modal hid %q:\n%s", visible, plain)
		}
	}
}
