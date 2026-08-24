package main

// /workflow: a multi-agent script, written to a file and run by name.
//
// It is a slash command rather than a tool on purpose. A model that could
// invoke a workflow would need its stages, rungs, and fan-out carried in the
// frozen zone, paid for on every cold cache of every session, to describe work
// the user already decided when they wrote the file. As a command it costs the
// cached prefix nothing at all.
//
// It runs on the exclusive operation lane, the same one /compact and /learn
// use, so the model is not executing while a workflow does. That is what keeps
// the loop's tool-result barrier untouched: no workflow task is ever a tool
// call waiting to return.

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/delegate"
)

func cmdWorkflow(m *tuiModel, args string) tea.Cmd {
	verb, rest, _ := strings.Cut(strings.TrimSpace(args), " ")
	rest = strings.TrimSpace(rest)

	switch verb {
	case "", "list":
		return workflowList(m)
	case "show":
		return workflowShow(m, rest)
	case "run":
		return workflowRun(m, rest)
	}
	return noticeCmd("warn", "usage: /workflow [list] · /workflow show <name> · /workflow run <name> [arguments]")
}

func workflowList(m *tuiModel) tea.Cmd {
	if len(subagentWorkflows) == 0 {
		m.addInfo("no workflows defined\n" +
			"  a workflow is stages of subagent tasks in a file, run in order:\n" +
			"  .switchboard/workflows/<name>.toml, or ~/.switchboard/workflows/<name>.toml\n\n" +
			"    [[stage]]\n" +
			"    name = \"survey\"\n" +
			"    [[stage.task]]\n" +
			"    task = \"List every call site of X with file:line.\"\n\n" +
			"    [[stage]]\n" +
			"    name = \"propose\"\n" +
			"    carry = true\n" +
			"    [[stage.task]]\n" +
			"    tier = \"t2\"\n" +
			"    task = \"Given the survey, propose the minimal edit.\"\n\n" +
			"  at most " + fmt.Sprint(delegate.MaxWorkflowStages) + " stages, " +
			fmt.Sprint(delegate.MaxTasksPerStage) + " tasks per stage, " +
			fmt.Sprint(delegate.MaxTasksPerWorkflow) + " tasks in all")
		return nil
	}
	var b strings.Builder
	b.WriteString("workflows\n")
	for _, wf := range subagentWorkflows {
		where := "project"
		if wf.FromHome {
			where = "user"
		}
		fmt.Fprintf(&b, "\n  %-16s %s\n", tuiWorkflowText(wf.Name), tuiWorkflowText(wf.Description))
		fmt.Fprintf(&b, "    %d stage(s) · %s · %s\n", len(wf.Stages), where, tuiWorkflowText(m.app.displayPath(wf.Path)))
	}
	b.WriteString("\n  /workflow show <name> reads one · /workflow run <name> runs it")
	// Redact the complete composed view before terminal escaping. A definition
	// may split a credential-shaped value across adjacent name/description/path
	// components; per-field redaction alone would miss the joined rendering.
	m.addInfo(redactWorkflowInput(strings.TrimRight(b.String(), "\n")))
	return nil
}

func workflowShow(m *tuiModel, name string) tea.Cmd {
	wf, ok := findWorkflow(name)
	if !ok {
		return noticeCmd("warn", "no workflow "+workspaceSanitize(redactWorkflowInput(name))+"; /workflow list shows what is defined")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s\n", tuiWorkflowText(wf.Name), tuiWorkflowText(wf.Description))
	for i, stage := range wf.Stages {
		carried := ""
		if stage.Carry {
			carried = " · carries the previous stage's answers"
		}
		fmt.Fprintf(&b, "\n  %d. %s%s\n", i+1, tuiWorkflowText(stage.Name), carried)
		for _, task := range stage.Tasks {
			where := task.Tier
			if task.Agent != "" {
				where = task.Agent
				if task.Tier != "" {
					where += " on " + task.Tier
				}
			}
			if where == "" {
				where = "the bottom rung"
			}
			// The task is one semantic component: redact it before firstLine's
			// display cap so a key straddling that cap cannot expose a prefix.
			fmt.Fprintf(&b, "     [%s] %s\n", tuiWorkflowText(where), tuiWorkflowText(firstLine(redactWorkflowInput(task.Task))))
		}
	}
	m.addInfo(redactWorkflowInput(strings.TrimRight(b.String(), "\n")))
	return nil
}

func findWorkflow(name string) (delegate.Workflow, bool) {
	for _, wf := range subagentWorkflows {
		if wf.Name == name {
			return wf, true
		}
	}
	return delegate.Workflow{}, false
}

// workflowDoneMsg carries a finished run back to the UI goroutine. It rides
// the operation lane's own completion path, so a run whose session was swapped
// underneath it is discarded rather than applied to the wrong transcript.
type workflowDoneMsg struct {
	generation uint64
	sourceID   string
	name       string
	result     delegate.WorkflowResult
}

func workflowRun(m *tuiModel, args string) tea.Cmd {
	name, arguments, _ := strings.Cut(args, " ")
	arguments = strings.TrimSpace(arguments)
	wf, ok := findWorkflow(name)
	if !ok {
		return noticeCmd("warn", "no workflow "+workspaceSanitize(redactWorkflowInput(name))+"; /workflow list shows what is defined")
	}
	runner := subagentRunner.get()
	if runner == nil {
		return noticeCmd("error", "the subagent runner is not assembled; delegation is unavailable in this session")
	}
	expanded, err := delegate.ExpandWorkflowArguments(wf, arguments)
	if err != nil {
		return noticeCmd("error", tuiWorkflowText("workflow "+wf.Name+" cannot start: "+err.Error()))
	}
	wf = expanded
	if err := runner.PreflightExpandedWorkflow(wf); err != nil {
		return noticeCmd("error", tuiWorkflowText("workflow "+wf.Name+" cannot start: "+err.Error()))
	}
	if leaks := workflowCredentialLeaks(wf); len(leaks) > 0 {
		return openWorkflowCredentialGate(m, wf, leaks, func(safe delegate.Workflow) tea.Cmd {
			return startWorkflowRun(m, runner, safe)
		})
	}
	return startWorkflowRun(m, runner, wf)
}

// A delegated worker's cross-agent contract never sends credentials raw, so
// this decision has only the two honest outcomes: redact every expanded task
// and run, or run nothing. In particular, a template prefix and argument
// suffix are scanned only after expansion has joined them.
func openWorkflowCredentialGate(m *tuiModel, wf delegate.Workflow, leaks []credential.Leak, proceed func(delegate.Workflow) tea.Cmd) tea.Cmd {
	found := make([]string, len(leaks))
	for i, leak := range leaks {
		found[i] = leak.String()
	}
	m.openDialog(&pickerDialog{
		title:          "the expanded workflow contains " + strings.Join(found, ", "),
		navigationOnly: true,
		items: []pickerItem{
			{id: "redact", label: "redact and run", desc: "each key becomes a placeholder in every affected task"},
			{id: "drop", label: "don't run", desc: "no delegate session starts and nothing leaves this machine"},
		},
		sel: 1,
		onPick: func(id string) tea.Cmd {
			if id == "redact" {
				return proceed(redactWorkflowCredentials(wf))
			}
			m.addNotice("", "workflow not run; its expanded tasks were dropped before anything left this machine")
			return nil
		},
	})
	return nil
}

func startWorkflowRun(m *tuiModel, runner *delegate.Runner, wf delegate.Workflow) tea.Cmd {
	safeName := redactWorkflowInput(wf.Name)
	ctx, generation, sourceID, err := m.startOperation("workflow " + safeName)
	if err != nil {
		return noticeCmd("warn", tuiWorkflowText(err.Error()))
	}
	m.addInfo(redactWorkflowInput("running workflow " + tuiWorkflowText(wf.Name) + "\n  " + fmt.Sprint(len(wf.Stages)) +
		" stage(s) · /tasks watches them · /tasks steer <id> corrects one · esc cancels the run"))

	program := m.app.p
	return m.ownOperationCmd(generation, func() tea.Msg {
		result := runner.RunExpandedWorkflow(ctx, wf, func(text string) {
			if program != nil {
				program.Send(noticeMsg{text: tuiWorkflowText("  " + text)})
			}
		})
		return workflowDoneMsg{generation: generation, sourceID: sourceID, name: wf.Name, result: result}
	})
}

// onWorkflowDone renders a finished run and releases the lane.
func (m *tuiModel) onWorkflowDone(msg workflowDoneMsg) tea.Cmd {
	if !m.operationMatches(msg.generation, msg.sourceID) || !m.finishOperation(msg.generation, false) {
		// The session was swapped or the operation was already released; the
		// answers belong to a transcript that is no longer here.
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "workflow %s\n", tuiWorkflowText(msg.name))
	for _, stage := range msg.result.Stages {
		fmt.Fprintf(&b, "\n  %s — %d answered, %d failed\n",
			tuiWorkflowText(stage.Stage), len(stage.Answers), len(stage.Failed))
		for _, answer := range stage.Answers {
			fmt.Fprintf(&b, "\n%s\n", tuiWorkflowText(answer))
		}
		for _, failed := range stage.Failed {
			fmt.Fprintf(&b, "\n  failed: %s\n", tuiWorkflowText(firstLine(redactWorkflowInput(failed))))
		}
	}
	switch {
	case msg.result.Canceled:
		// The finished stages are kept. The work was done and paid for, and
		// discarding it would make cancelling more expensive than waiting.
		b.WriteString("\n  cancelled; the stages above finished before it stopped")
	case msg.result.Err != nil:
		fmt.Fprintf(&b, "\n  stopped: %s", tuiWorkflowText(msg.result.Err.Error()))
	}
	m.addInfo(redactWorkflowInput(strings.TrimRight(b.String(), "\n")))
	m.refreshCost(m.app.loop.Session.State())
	return m.nextQueuedTurn()
}

func tuiWorkflowText(text string) string {
	return workspaceSanitize(redactWorkflowInput(text))
}
