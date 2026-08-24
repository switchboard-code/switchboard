package main

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/delegate"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

func workflowBoundaryRunner(t *testing.T, tier config.Tier) (*delegate.Runner, *learnCaptureProvider, *[]string) {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	capture := &learnCaptureProvider{}
	paths := &[]string{}
	runner := delegate.NewRunner(delegate.Config{
		Tiers: []config.Tier{tier}, Tasks: delegate.NewTaskManager(1),
		Probe: func(context.Context, string) (config.Tier, provider.Provider, string, error) {
			return tier, capture, "", nil
		},
		NewSession: func(target provider.RouteTargetID) (*session.Session, error) {
			sess, err := store.Create(workspace, target, "workflow-boundary-test")
			if sess != nil {
				*paths = append(*paths, sess.Path())
			}
			return sess, err
		},
		NewLoop: func(tier config.Tier, client provider.Provider, sess *session.Session, observer agent.Observer, _ *delegate.Agent, _ delegate.TaskRef) (*agent.Loop, error) {
			registry, err := tools.NewRegistry(workspace, execution.Capability{})
			if err != nil {
				return nil, err
			}
			return &agent.Loop{
				Provider: client, Target: tier.Target, Tools: registry,
				Perms:   permission.NewEngine(permission.ModeDefault, execution.Capability{}),
				Session: sess, System: []provider.Block{provider.Text{Text: delegate.Preamble}},
				Observer: observer, MaxToolRounds: delegate.MaxRounds,
			}, nil
		},
	})
	return runner, capture, paths
}

func workflowRequestText(request provider.Request) string {
	var out strings.Builder
	for _, message := range request.Messages {
		out.WriteString(message.Text())
		out.WriteByte('\n')
	}
	return out.String()
}

func workflowSessionText(t *testing.T, paths []string) string {
	t.Helper()
	var out strings.Builder
	for _, path := range paths {
		state, err := session.ReadState(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range state.Messages {
			out.WriteString(message.Text())
			out.WriteByte('\n')
		}
	}
	return out.String()
}

func TestHeadlessWorkflowNeverAttachesInteractiveRelays(t *testing.T) {
	for _, stdinTerminal := range []bool{false, true} {
		name := "non-terminal stdin"
		if stdinTerminal {
			name = "terminal stdin"
		}
		t.Run(name, func(t *testing.T) {
			opts := options{workflow: "survey internal/agent"}
			if plainSurfaceCanAsk(opts, stdinTerminal) {
				t.Fatal("headless workflow was classified as an attended surface")
			}
			primary := &agent.Loop{}
			questions := &questionRelay{}
			// Nil I/O is deliberate: an unattended branch must return before it
			// constructs either relay, so this also fails fast if it ever widens.
			attachPlainSurfaceRelays(opts, stdinTerminal, primary, questions, nil, nil)
			if primary.Asker != nil {
				t.Fatal("headless workflow installed a permission asker")
			}
			questions.mu.Lock()
			questioner := questions.to
			questions.mu.Unlock()
			if questioner != nil {
				t.Fatal("headless workflow installed a question relay")
			}
			child := &agent.Loop{Asker: primary.Asker}
			if child.Asker != nil {
				t.Fatal("a delegate inherited an asker from an unattended workflow")
			}
			manager := delegate.NewTaskManager(1)
			ref := manager.Reserve("workflow task", "inspect", "t1", "parent")
			_, err := manager.AttributedAsker(ref, child.Asker).Ask(context.Background(), permission.Request{
				Tool: "exec", Effect: permission.EffectExecute,
			}, permission.Outcome{Decision: permission.Ask})
			if err == nil || !strings.Contains(err.Error(), "no permission asker") {
				t.Fatalf("unattended delegate approval = %v, want immediate fail-closed refusal", err)
			}
		})
	}
}

func TestHeadlessWorkflowPreflightResolvesEveryTaskBeforePublication(t *testing.T) {
	tests := []struct {
		name    string
		command string
		task    delegate.WorkflowTask
		note    string
		want    string
	}{
		{name: "unknown tier", command: "check", task: delegate.WorkflowTask{Task: "inspect", Tier: "t9"}, want: `no tier "t9"`},
		{name: "unknown agent", command: "check", task: delegate.WorkflowTask{Task: "inspect", Agent: "ghost"}, want: `no agent "ghost"`},
		{name: "empty expansion", command: "check", task: delegate.WorkflowTask{Task: "$ARGUMENTS"}, want: "task is required"},
		{name: "amplified expansion", command: "check " + strings.Repeat("x", delegate.MaxExpandedWorkflowTaskBytes/2+1),
			task: delegate.WorkflowTask{Task: "$ARGUMENTS$ARGUMENTS"}, want: "1048576-byte limit"},
		{name: "credential after expansion", command: "check aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", task: delegate.WorkflowTask{Task: "ghp_$ARGUMENTS"}, want: "-allow-secrets"},
		{name: "rejected definition", command: "broken", note: "workflow /repo/.switchboard/workflows/broken.toml: unrecognized settings: stage.naem", want: "unrecognized settings"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			savedWorkflows := subagentWorkflows
			savedNotes := subagentWorkflowNotes
			savedRunner := subagentRunner.get()
			t.Cleanup(func() {
				subagentWorkflows = savedWorkflows
				subagentWorkflowNotes = savedNotes
				subagentRunner.set(savedRunner)
			})

			tier := config.Tier{ID: "t1", Target: provider.RouteTarget{
				Provider: "ollama", Surface: "local", ModelID: "test",
			}}
			subagentRunner.set(delegate.NewRunner(delegate.Config{
				Tiers: []config.Tier{tier}, Agents: []delegate.Agent{{Name: "known"}},
			}))
			if test.note == "" {
				subagentWorkflows = []delegate.Workflow{{
					Name: "check", Stages: []delegate.Stage{{Name: "stage", Tasks: []delegate.WorkflowTask{test.task}}},
				}}
				subagentWorkflowNotes = nil
			} else {
				subagentWorkflows = nil
				subagentWorkflowNotes = []string{test.note}
			}

			store, err := session.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			workspace := t.TempDir()
			prior, err := store.Create(workspace, tier.Target.ID(), "rev")
			if err != nil {
				t.Fatal(err)
			}
			priorID := prior.ID()
			if err := prior.Close(); err != nil {
				t.Fatal(err)
			}
			fresh, err := store.CreateStaged(workspace, tier.Target.ID(), "rev")
			if err != nil {
				t.Fatal(err)
			}
			defer fresh.CloseDiscardingStaged()

			err = publishSessionAfterStartupPreflight(fresh, "", false, test.command)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("preflight error = %v, want %q", err, test.want)
			}
			if !fresh.PublicationPending() {
				t.Fatal("invalid workflow published its empty primary session")
			}
			latest, err := store.Latest(workspace)
			if err != nil {
				t.Fatal(err)
			}
			defer latest.Close()
			if latest.ID() != priorID {
				t.Fatalf("Latest after rejected workflow = %s, want prior %s", latest.ID(), priorID)
			}
		})
	}
}

func TestUnknownWorkflowNameCannotRenderASecret(t *testing.T) {
	savedWorkflows := subagentWorkflows
	savedNotes := subagentWorkflowNotes
	subagentWorkflows = []delegate.Workflow{{Name: "known"}}
	subagentWorkflowNotes = nil
	t.Cleanup(func() {
		subagentWorkflows = savedWorkflows
		subagentWorkflowNotes = savedNotes
	})

	const token = "ghp_012345678901234567890123456789012345"
	_, err := prepareHeadlessWorkflow(token, false)
	if err == nil {
		t.Fatal("unknown workflow unexpectedly resolved")
	}
	if strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "[redacted:") {
		t.Fatalf("unknown-workflow diagnostic leaked input: %v", err)
	}
}

func TestWorkflowArgumentsAreScannedAfterTemplateExpansion(t *testing.T) {
	savedWorkflows := subagentWorkflows
	savedRunner := subagentRunner.get()
	t.Cleanup(func() {
		subagentWorkflows = savedWorkflows
		subagentRunner.set(savedRunner)
	})

	tier := config.Tier{ID: "t1", Target: provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "test"}}
	subagentRunner.set(delegate.NewRunner(delegate.Config{Tiers: []config.Tier{tier}}))
	subagentWorkflows = []delegate.Workflow{{
		Name: "check", Stages: []delegate.Stage{{Name: "one", Tasks: []delegate.WorkflowTask{{Task: "use ghp_$ARGUMENTS"}}}},
	}}
	const suffix = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const token = "ghp_" + suffix

	_, err := prepareHeadlessWorkflow("check "+suffix, false)
	if err == nil || !strings.Contains(err.Error(), "-allow-secrets") {
		t.Fatalf("composed credential was not refused: %v", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("workflow refusal rendered the composed credential: %v", err)
	}
	if _, err := prepareHeadlessWorkflow("check "+suffix, true); err != nil {
		t.Fatalf("explicit scripted widening did not pass preflight: %v", err)
	}

	m := testModel(t)
	if cmd := workflowRun(m, "check "+suffix); cmd != nil {
		t.Fatal("credential-bearing workflow should wait behind a dialog")
	}
	if m.dlg == nil || m.operationActive {
		t.Fatalf("workflow gate state: dialog=%T active=%v", m.dlg, m.operationActive)
	}
	if view := m.dlg.view(100, m.th); strings.Contains(view, token) {
		t.Fatalf("workflow gate rendered the credential: %q", view)
	}
}

func TestWorkflowSurfacesNeverExpandArgumentIntroducedTemplateSyntaxAgain(t *testing.T) {
	const half = "aaaaaaaaaaaaaaaaaa"
	const composed = "ghp_" + half + half
	const gated = "ghp_" + half + "$ARGUMENTS" + half
	tier := config.Tier{ID: "t1", Target: provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "test"}}

	savedWorkflows := subagentWorkflows
	savedRunner := subagentRunner.get()
	t.Cleanup(func() {
		subagentWorkflows = savedWorkflows
		subagentRunner.set(savedRunner)
	})
	subagentWorkflows = []delegate.Workflow{{
		Name: "check", Stages: []delegate.Stage{{Name: "one", Tasks: []delegate.WorkflowTask{{Task: "$ARGUMENTS"}}}},
	}}

	t.Run("headless", func(t *testing.T) {
		runner, capture, paths := workflowBoundaryRunner(t, tier)
		subagentRunner.set(runner)
		var output bytes.Buffer
		renderer := &renderer{w: bufio.NewWriter(&output), atLineTop: true}
		if err := runHeadlessWorkflow(context.Background(), renderer, "check "+gated, false); err != nil {
			t.Fatal(err)
		}
		request := workflowRequestText(capture.request)
		durable := workflowSessionText(t, *paths)
		if capture.calls != 1 || !strings.Contains(request, gated) || !strings.Contains(durable, gated) {
			t.Fatalf("headless exact boundary: calls=%d request=%q durable=%q", capture.calls, request, durable)
		}
		for where, text := range map[string]string{"provider": request, "session": durable, "progress": output.String()} {
			if strings.Contains(text, composed) {
				t.Fatalf("headless %s saw second-pass composed credential: %q", where, text)
			}
		}
	})

	t.Run("tui", func(t *testing.T) {
		runner, capture, paths := workflowBoundaryRunner(t, tier)
		subagentRunner.set(runner)
		m := testModel(t)
		cmd := workflowRun(m, "check "+gated)
		if cmd == nil || m.dlg != nil || !m.operationActive {
			t.Fatalf("TUI did not start literal-template workflow: cmd=%v dialog=%T active=%v", cmd != nil, m.dlg, m.operationActive)
		}
		message := cmd()
		if message == nil {
			t.Fatal("TUI workflow returned no completion")
		}
		m.Update(message)
		request := workflowRequestText(capture.request)
		durable := workflowSessionText(t, *paths)
		progress := strings.Join(m.tr.flat, "\n")
		if capture.calls != 1 || !strings.Contains(request, gated) || !strings.Contains(durable, gated) {
			t.Fatalf("TUI exact boundary: calls=%d request=%q durable=%q", capture.calls, request, durable)
		}
		for where, text := range map[string]string{"provider": request, "session": durable, "progress": progress} {
			if strings.Contains(text, composed) {
				t.Fatalf("TUI %s saw second-pass composed credential: %q", where, text)
			}
		}
	})
}

func TestWorkflowCredentialGateRedactsEveryExpandedTaskOrDrops(t *testing.T) {
	const token = "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	wf := delegate.Workflow{Name: "check", Stages: []delegate.Stage{{Tasks: []delegate.WorkflowTask{{Task: "one " + token}, {Task: "two " + token}}}}}
	leaks := workflowCredentialLeaks(wf)
	if len(leaks) == 0 {
		t.Fatal("fixture was not detected")
	}
	m := testModel(t)
	var ran delegate.Workflow
	openWorkflowCredentialGate(m, wf, leaks, func(safe delegate.Workflow) tea.Cmd { ran = safe; return nil })
	m.key(tea.KeyMsg{Type: tea.KeyUp})
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	for _, task := range ran.Stages[0].Tasks {
		if strings.Contains(task.Task, token) || !strings.Contains(task.Task, "[redacted: a GitHub token]") {
			t.Fatalf("redact choice left an unsafe task: %q", task.Task)
		}
	}
	if strings.Contains(wf.Stages[0].Tasks[0].Task, "[redacted:") {
		t.Fatal("workflow source was mutated")
	}

	ran = delegate.Workflow{}
	openWorkflowCredentialGate(m, wf, leaks, func(safe delegate.Workflow) tea.Cmd { ran = safe; return nil })
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	if len(ran.Stages) != 0 {
		t.Fatal("safe-default Enter ran the workflow")
	}
}

func TestHeadlessWorkflowEscapesRepositoryAndModelTerminalControls(t *testing.T) {
	token := "ghp_" + strings.Repeat("a", 36)
	var buf bytes.Buffer
	out := &renderer{w: bufio.NewWriter(&buf), atLineTop: true}
	renderHeadlessWorkflowResult(out, delegate.WorkflowResult{Stages: []delegate.StageResult{{
		Stage:   "stage\x1b[2J\nforged\u202e",
		Answers: []string{"answer\x1b]52;c;Y2xpcGJvYXJk\a\nworkflow forged"},
		Failed: []string{
			"failure\rrewritten\u2066\nignored tail",
			strings.Repeat("x", 90) + token,
		},
	}}})
	out.flush()

	got := buf.String()
	for _, unsafe := range []string{"\x1b", "\a", "\r", "\u202e", "\u2066"} {
		if strings.Contains(got, unsafe) {
			t.Fatalf("headless workflow retained terminal control %q: %q", unsafe, got)
		}
	}
	if strings.Contains(got, "stage\nforged") || strings.Contains(got, "answer\nworkflow forged") {
		t.Fatalf("repository/model newline created workflow structure: %q", got)
	}
	if strings.Contains(got, token) || strings.Contains(got, "ghp_") || !strings.Contains(got, "[redacted:") {
		t.Fatalf("workflow failure was capped before complete credential redaction: %q", got)
	}
	for _, escaped := range []string{`stage\x1b[2J\x0aforged\u202e`, `answer\x1b]52;c;Y2xpcGJvYXJk\x07\x0aworkflow forged`, `failure\x0drewritten\u2066`} {
		if !strings.Contains(got, escaped) {
			t.Fatalf("headless workflow did not visibly escape %q: %q", escaped, got)
		}
	}
	if name := headlessWorkflowText("flow\x1b]2;title\a\nforged\u202e"); strings.ContainsAny(name, "\x1b\a\n\u202e") || !strings.Contains(name, `\x0a`) {
		t.Fatalf("workflow header/progress boundary is unsafe: %q", name)
	}
}

func TestTUIWorkflowListAndShowRedactBeforeDisplayCaps(t *testing.T) {
	const token = "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	saved := subagentWorkflows
	t.Cleanup(func() { subagentWorkflows = saved })
	subagentWorkflows = []delegate.Workflow{{
		Name: "safe", Description: "description " + token + "\x1b]2;forged\a",
		Path: ".switchboard/workflows/safe.toml",
		Stages: []delegate.Stage{{Name: "stage\nforged", Tasks: []delegate.WorkflowTask{{
			Task: strings.Repeat("x", 90) + token,
		}}}},
	}}

	m := testModel(t)
	workflowList(m)
	workflowShow(m, "safe")
	got := stripANSI(strings.Join(m.tr.flat, "\n"))
	if strings.Contains(got, token) || strings.Contains(got, "ghp_") {
		t.Fatalf("TUI workflow inventory rendered a credential: %q", got)
	}
	if strings.ContainsAny(got, "\x1b\a") || strings.Contains(got, "stage\nforged") {
		t.Fatalf("TUI workflow inventory retained terminal structure: %q", got)
	}
	if !strings.Contains(got, "[redacted:") {
		t.Fatalf("TUI workflow inventory omitted the redaction marker: %q", got)
	}
	if safe := tuiWorkflowText("failure " + token + "\nforged"); strings.Contains(safe, token) || strings.Contains(safe, "\n") {
		t.Fatalf("TUI workflow result boundary is unsafe: %q", safe)
	}
}

func TestTUIWorkflowPreflightsEveryTaskBeforeClaimingTheOperationLane(t *testing.T) {
	savedWorkflows := subagentWorkflows
	savedRunner := subagentRunner.get()
	t.Cleanup(func() {
		subagentWorkflows = savedWorkflows
		subagentRunner.set(savedRunner)
	})

	tier := config.Tier{ID: "t1", Target: provider.RouteTarget{
		Provider: "ollama", Surface: "local", ModelID: "test",
	}}
	subagentRunner.set(delegate.NewRunner(delegate.Config{Tiers: []config.Tier{tier}}))
	subagentWorkflows = []delegate.Workflow{{Name: "check", Stages: []delegate.Stage{
		{Name: "valid", Tasks: []delegate.WorkflowTask{{Task: "inspect"}}},
		{Name: "invalid", Tasks: []delegate.WorkflowTask{{Task: "review", Tier: "t9"}}},
	}}}

	m := testModel(t)
	cmd := workflowRun(m, "check")
	if cmd == nil {
		t.Fatal("invalid workflow returned no refusal")
	}
	msg, ok := cmd().(noticeMsg)
	if !ok || !strings.Contains(msg.text, `no tier "t9"`) {
		t.Fatalf("invalid workflow refusal = %#v", msg)
	}
	if m.operationActive || m.busy {
		t.Fatal("invalid workflow claimed the operation lane before full preflight")
	}
}
