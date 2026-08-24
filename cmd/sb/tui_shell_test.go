package main

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestShellContextRedactsBeforeTheByteCap(t *testing.T) {
	token := "sk-ant-api03-" + "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefghijklmnop"
	raw := strings.Repeat("x", shellOutputCap-len(token)/2-1) + " " + token
	var capture shellOutput
	if n, err := capture.Write([]byte(raw + strings.Repeat("z", shellCaptureCap))); err != nil || n == 0 {
		t.Fatalf("bounded capture write = %d, %v", n, err)
	}
	captured := capture.String()
	if len(captured) != shellCaptureCap {
		t.Fatalf("bounded capture retained %d bytes, want %d", len(captured), shellCaptureCap)
	}
	display := capShellOutput(captured)
	if !strings.Contains(display, "sk-ant-api03-") {
		t.Fatal("test precondition: the user-visible cap did not retain the boundary fragment")
	}
	contextOutput := capShellOutput(redactCredentialText(captured))
	if strings.Contains(contextOutput, "sk-ant-api03-") || !strings.Contains(contextOutput, "[redacted") {
		t.Fatalf("redacted shell projection retained a credential fragment: %q", contextOutput)
	}

	m := testModel(t)
	m.onShellDone(shellDoneMsg{
		command:        "printf " + token,
		output:         display,
		contextCommand: redactCredentialText("printf " + token),
		contextOutput:  contextOutput,
		result:         shellResult{kind: shellSucceeded},
	})
	prompt := m.shellContext("continue")
	if strings.Contains(prompt, "sk-ant-api03-") || strings.Count(prompt, "[redacted") < 2 {
		t.Fatalf("shell command or output reached provider context as a credential fragment:\n%s", prompt)
	}
}

func TestShellOutputWriterIsBoundedAndAcknowledgesConcurrentWrites(t *testing.T) {
	var out shellOutput
	payload := []byte(strings.Repeat("output", shellCaptureCap))
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if n, err := out.Write(payload); err != nil || n != len(payload) {
				t.Errorf("Write = %d, %v; want %d, nil", n, err, len(payload))
			}
		}()
	}
	wg.Wait()
	retained, discarded := out.snapshot()
	if got := len(retained); got != shellCaptureCap {
		t.Fatalf("concurrent capture retained %d bytes, want exactly %d", got, shellCaptureCap)
	}
	if !discarded {
		t.Fatal("bounded writer did not record that output was discarded")
	}
}

func TestShellScanOverlapCoversEveryCredentialShapeAtTheCap(t *testing.T) {
	secrets := []string{
		"sk-ant-" + strings.Repeat("A", 40),
		"sk-proj-" + strings.Repeat("A", 40),
		"ghp_" + strings.Repeat("A", 36),
		"github_pat_" + strings.Repeat("A", 22),
		"glpat-" + strings.Repeat("A", 20),
		"xoxb-" + strings.Repeat("A", 10),
		"AKIAABCDEFGHIJKLMNOP",
		"AIza" + strings.Repeat("A", 35),
		"sk_live_" + strings.Repeat("A", 20),
		"npm_" + strings.Repeat("A", 36),
		"hf_" + strings.Repeat("A", 30),
		"-----BEGIN PRIVATE KEY-----\nabcdefghijklmnopqrstuvwxyz\n-----END PRIVATE KEY-----",
	}
	for _, secret := range secrets {
		var out shellOutput
		raw := strings.Repeat("x", shellOutputCap-5) + " " + secret + "\n" + strings.Repeat("z", shellCaptureCap)
		if _, err := out.Write([]byte(raw)); err != nil {
			t.Fatal(err)
		}
		redacted := redactCredentialText(out.String())
		if !strings.Contains(redacted, "[redacted:") {
			t.Fatalf("overlap did not retain enough of credential shape %q for scanning", secret[:min(12, len(secret))])
		}
		bounded := capShellOutput(redacted)
		if strings.Contains(bounded, secret[:min(12, len(secret))]) {
			t.Fatalf("credential prefix survived the bounded projection for %q", secret[:min(12, len(secret))])
		}
	}
}

func TestShellByteCapPreservesUTF8AndWholeGraphemes(t *testing.T) {
	for _, cluster := range []string{"e\u0301", "👩‍💻"} {
		raw := strings.Repeat("x", shellOutputCap-1) + cluster + "tail"
		got := capShellOutput(raw)
		body, _, _ := strings.Cut(got, "\n[truncated")
		if !utf8.ValidString(got) {
			t.Fatalf("cap split UTF-8 for %q: %q", cluster, got[len(got)-32:])
		}
		if strings.Contains(body, cluster) || body != strings.Repeat("x", shellOutputCap-1) {
			t.Fatalf("cap retained only part of grapheme %q: suffix=%q", cluster, body[len(body)-8:])
		}
	}
}

func runShellForTest(t *testing.T, command string) (*tuiModel, shellDoneMsg) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the TUI shell runner uses the user's platform shell")
	}
	m := testModel(t)
	m.app.workspace = t.TempDir()
	t.Setenv("SHELL", "/bin/sh")
	cmd := m.runShell(command)
	if cmd == nil || !m.busy || !m.operationActive {
		t.Fatalf("shell did not claim asynchronous operation ownership: cmd=%v busy=%v operation=%v", cmd != nil, m.busy, m.operationActive)
	}
	msg, ok := cmd().(shellDoneMsg)
	if !ok {
		t.Fatalf("shell command returned an unexpected message")
	}
	return m, msg
}

func shellEntry(t *testing.T, m *tuiModel) *entry {
	t.Helper()
	for i := len(m.tr.entries) - 1; i >= 0; i-- {
		if m.tr.entries[i].kind == kindTool && m.tr.entries[i].tool.name == "shell" {
			return m.tr.entries[i]
		}
	}
	t.Fatal("no shell transcript entry")
	return nil
}

func TestShellSuccessShowsExitZeroAndKeepsOutputExpandable(t *testing.T) {
	m, msg := runShellForTest(t, `printf "$PWD"`)
	if msg.err != nil {
		t.Fatalf("shell command failed: %v", msg.err)
	}
	m.onShellDone(msg)
	if m.busy || m.operationActive {
		t.Fatal("completed shell command did not release operation ownership")
	}

	e := shellEntry(t, m)
	if e.tool.failed || !strings.Contains(e.tool.detail, "exit 0 (success)") {
		t.Fatalf("successful command has no explicit verdict: %+v", e.tool)
	}
	collapsed := stripANSI(strings.Join(m.tr.flat, "\n"))
	if !strings.Contains(collapsed, "exit 0 (success)") {
		t.Fatalf("success is not visible while collapsed:\n%s", collapsed)
	}
	if strings.Contains(collapsed, m.app.workspace) {
		t.Fatalf("stdout should stay behind expansion on success:\n%s", collapsed)
	}
	e.expanded = true
	e.cache = nil
	m.tr.invalidate(m.tr.indexOf(e))
	expanded := stripANSI(strings.Join(m.tr.flat, "\n"))
	// Transcript wrapping may split a long temporary path across physical
	// rows; compare its semantic cells rather than requiring one terminal row.
	if !strings.Contains(strings.Join(strings.Fields(expanded), ""), strings.Join(strings.Fields(m.app.workspace), "")) {
		t.Fatalf("expansion did not reveal stdout:\n%s", expanded)
	}

	contextPrompt := m.shellContext("continue")
	if !strings.Contains(contextPrompt, "[shell result: success; exit_code=0]") {
		t.Fatalf("next-prompt context lost the structured outcome:\n%s", contextPrompt)
	}
}

func TestShellCompletionInvalidatesEveryOpenWorkspaceSurface(t *testing.T) {
	for _, test := range []struct {
		name  string
		open  func(*tuiModel) fullscreen
		stale func(fullscreen) bool
	}{
		{
			name: "workspace source",
			open: func(m *tuiModel) fullscreen {
				return &workspaceView{runtime: m.workspaceRuntime, generation: m.workspaceGeneration}
			},
			stale: func(view fullscreen) bool {
				v := view.(*workspaceView)
				return v.stale && v.previewStale
			},
		},
		{
			name:  "diff",
			open:  func(*tuiModel) fullscreen { return &diffView{} },
			stale: func(view fullscreen) bool { return view.(*diffView).stale },
		},
		{
			name:  "lsp",
			open:  func(*tuiModel) fullscreen { return &lspView{} },
			stale: func(view fullscreen) bool { return view.(*lspView).stale },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := testModel(t)
			before := m.workspaceGeneration
			view := test.open(m)
			m.full = view

			m.onShellDone(shellDoneMsg{command: "true", result: shellResult{kind: shellSucceeded}})

			if m.workspaceGeneration != before+1 {
				t.Fatalf("workspace generation = %d, want %d", m.workspaceGeneration, before+1)
			}
			if !test.stale(view) {
				t.Fatalf("%s remained current after a host-authority shell completion", test.name)
			}
		})
	}
}

func TestShellFailureShowsExactExitCodeBesideOutput(t *testing.T) {
	m, msg := runShellForTest(t, `printf "problem on stderr\n" >&2; exit 7`)
	m.onShellDone(msg)

	e := shellEntry(t, m)
	if !e.tool.failed {
		t.Fatal("nonzero command rendered as successful")
	}
	visible := stripANSI(strings.Join(m.tr.flat, "\n"))
	for _, want := range []string{"problem on stderr", "exit 7 (failure)"} {
		if !strings.Contains(visible, want) {
			t.Fatalf("collapsed failure is missing %q:\n%s", want, visible)
		}
	}
	contextPrompt := m.shellContext("diagnose it")
	if !strings.Contains(contextPrompt, "[shell result: failure; exit_code=7]") {
		t.Fatalf("next-prompt context lost the exit code:\n%s", contextPrompt)
	}
}

func TestShellSignalNamesTheCause(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signal behavior")
	}
	m, msg := runShellForTest(t, `kill -TERM $$`)
	m.onShellDone(msg)
	visible := stripANSI(strings.Join(m.tr.flat, "\n"))
	if !strings.Contains(visible, "terminated by signal") || !strings.Contains(visible, "terminated") {
		t.Fatalf("signal termination was not named:\n%s", visible)
	}
}

func TestShellTimeoutAndCancellationNameTheirCause(t *testing.T) {
	timedOut := classifyShellResult(errors.New("process killed"), context.DeadlineExceeded)
	wantTimeout := "timed out after " + shellTimeout.String()
	wantCancelled := "cancelled by user"
	if runtime.GOOS == "windows" {
		limit := "; only the direct shell was stopped; descendant processes may still be running"
		wantTimeout += limit
		wantCancelled += limit
	}
	if got := timedOut.summary(); got != wantTimeout {
		t.Fatalf("timeout summary = %q", got)
	}
	cancelled := classifyShellResult(errors.New("process killed"), context.Canceled)
	if got := cancelled.summary(); got != wantCancelled {
		t.Fatalf("cancellation summary = %q", got)
	}
	if record := cancelled.contextRecord(); (runtime.GOOS == "windows") != strings.Contains(record, "descendants_may_survive=true") {
		t.Fatalf("cancellation context did not match platform cleanup semantics: %q", record)
	}

	if runtime.GOOS == "windows" {
		return
	}
	m := testModel(t)
	m.app.workspace = t.TempDir()
	t.Setenv("SHELL", "/bin/sh")
	cmd := m.runShell(`printf "must not run"`)
	m.operationCancelling = true
	m.turnCancel()
	msg := cmd().(shellDoneMsg)
	m.onShellDone(msg)
	visible := stripANSI(strings.Join(m.tr.flat, "\n"))
	if !strings.Contains(visible, "cancelled by user") || m.busy || m.operationActive {
		t.Fatalf("cancelled operation was not visibly and completely settled: busy=%v operation=%v\n%s", m.busy, m.operationActive, visible)
	}
}

func TestShellOutputControlsStayInertWhenExpanded(t *testing.T) {
	m := testModel(t)
	m.tr.add(&entry{kind: kindTool, tool: toolEntry{name: "shell", desc: "unsafe output"}, rank: -1})
	m.onShellDone(shellDoneMsg{
		command: "unsafe output",
		output:  "ok\x1b[2J\x1b]52;c;Y2xpcGJvYXJk\x07\rSPOOF\nnext\tcolumn",
		result:  shellResult{kind: shellSucceeded},
		took:    time.Millisecond,
	})
	e := shellEntry(t, m)
	e.expanded = true
	e.cache = nil
	m.tr.invalidate(m.tr.indexOf(e))
	rendered := strings.Join(m.tr.flat, "\n")
	if strings.ContainsAny(rendered, "\x07\r") || strings.Contains(rendered, "\x1b[2J") || strings.Contains(rendered, "\x1b]52;") {
		t.Fatalf("shell output wrote terminal controls: %q", rendered)
	}
	plain := stripANSI(rendered)
	if !strings.Contains(plain, `\x1b`) || !strings.Contains(plain, `\x0d`) || !strings.Contains(plain, "next") {
		t.Fatalf("safe rendering lost visible escapes or output: %q", plain)
	}
}

func TestStaleShellCompletionCannotMutateTheCurrentSession(t *testing.T) {
	m, msg := runShellForTest(t, `exit 9`)
	e := shellEntry(t, m)
	m.operationSourceID = "a-new-session-owns-the-prompt"
	m.onShellDone(msg)
	if e.tool.done || len(m.pendingShell) != 0 {
		t.Fatalf("stale completion mutated live state: done=%v pending=%d", e.tool.done, len(m.pendingShell))
	}

	// Restore the synthetic guard change so test cleanup does not leave an
	// owned context behind.
	m.operationSourceID = msg.sourceID
	m.finishOperation(msg.operation, false)
}

func TestShellCompletionInvalidatesLiteralAndSemanticWorkspaceSnapshots(t *testing.T) {
	for _, test := range []struct {
		name   string
		result shellResult
	}{
		{name: "success", result: shellResult{kind: shellSucceeded}},
		// A failed shell may already have changed files before its non-zero exit.
		{name: "failure after partial mutation", result: shellResult{kind: shellExited, exitCode: 9}},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := testModel(t)
			m.workspaceRuntime = newWorkspaceRuntime(m.app.workspace)
			semantic := &lspView{}
			m.full = semantic
			m.tr.add(&entry{kind: kindTool, tool: toolEntry{name: "shell", desc: test.name}, rank: -1})

			before := m.workspaceRuntime.epoch.Load()
			m.onShellDone(shellDoneMsg{command: test.name, result: test.result})

			if after := m.workspaceRuntime.epoch.Load(); after != before+1 {
				t.Fatalf("workspace epoch = %d, want %d", after, before+1)
			}
			if !semantic.stale {
				t.Fatal("shell completion left the open semantic result looking current")
			}
		})
	}
}
