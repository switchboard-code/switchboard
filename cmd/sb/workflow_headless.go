package main

// Running a workflow without a terminal.
//
// The stages were decided when the file was written, so a workflow asks
// nothing of a person while it runs — which makes it the one thing on this
// surface that is genuinely scriptable. It is also the only way to exercise
// the real path against a real ladder without a TUI, which is the reason it
// exists at all: a feature whose only entrance is an interactive one is a
// feature nobody can test.

import (
	"context"
	"fmt"
	"strings"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/delegate"
)

type headlessWorkflowPlan struct {
	workflow delegate.Workflow
	runner   *delegate.Runner
}

// prepareHeadlessWorkflow resolves everything that can refuse a workflow
// before the startup session is published. A typo or unavailable delegate
// runner must not make an otherwise empty staged session become --continue's
// newest history.
func prepareHeadlessWorkflow(args string, allowSecrets bool) (headlessWorkflowPlan, error) {
	name, arguments, _ := strings.Cut(strings.TrimSpace(args), " ")
	arguments = strings.TrimSpace(arguments)
	shownName := headlessWorkflowText(redactWorkflowInput(name))

	workflow, ok := findWorkflow(name)
	if !ok {
		if diagnostic := requestedWorkflowDiagnostic(name); diagnostic != "" {
			return headlessWorkflowPlan{}, fmt.Errorf("workflow %q is unavailable: %s", shownName, headlessWorkflowText(diagnostic))
		}
		var names []string
		for _, w := range subagentWorkflows {
			names = append(names, headlessWorkflowText(w.Name))
		}
		if len(names) == 0 {
			return headlessWorkflowPlan{}, fmt.Errorf("no workflows are defined; write one to .switchboard/workflows/<name>.toml")
		}
		return headlessWorkflowPlan{}, fmt.Errorf("no workflow %q; this workspace has %s", shownName, strings.Join(names, ", "))
	}
	runner := subagentRunner.get()
	if runner == nil {
		return headlessWorkflowPlan{}, fmt.Errorf("the subagent runner is not assembled; delegation is unavailable")
	}
	workflowName := workflow.Name
	workflow, err := delegate.ExpandWorkflowArguments(workflow, arguments)
	if err != nil {
		return headlessWorkflowPlan{}, fmt.Errorf("workflow %s cannot start: %w",
			headlessWorkflowText(workflowName), terminalWorkflowError{err})
	}
	if leaks := workflowCredentialLeaks(workflow); len(leaks) > 0 && !allowSecrets {
		return headlessWorkflowPlan{}, workflowCredentialRefusal(leaks)
	}
	if err := runner.PreflightExpandedWorkflow(workflow); err != nil {
		return headlessWorkflowPlan{}, fmt.Errorf("workflow %s cannot start: %w",
			headlessWorkflowText(workflow.Name), terminalWorkflowError{err})
	}
	return headlessWorkflowPlan{workflow: workflow, runner: runner}, nil
}

func redactWorkflowInput(text string) string {
	if leaks := credential.ScanPrompt(text); len(leaks) > 0 {
		return credential.Redact(text, leaks)
	}
	return text
}

// requestedWorkflowDiagnostic makes a rejected definition visible on the
// unattended surface that asked for it. Discovery diagnostics are already
// sanitized and bounded; match only the requested basename so an unrelated
// malformed workflow does not drown the actionable error.
func requestedWorkflowDiagnostic(name string) string {
	marker := "/" + name + ".toml:"
	for _, note := range subagentWorkflowNotes {
		normalized := strings.ReplaceAll(note, `\`, "/")
		if strings.Contains(normalized, marker) {
			return note
		}
	}
	return ""
}

func runHeadlessWorkflow(ctx context.Context, out *renderer, args string, allowSecrets bool) error {
	// The renderer buffers, and this path returns straight to main rather than
	// through the REPL's own teardown, so every exit has to flush or the run
	// produces its answers and prints none of them.
	defer out.flush()

	plan, err := prepareHeadlessWorkflow(args, allowSecrets)
	if err != nil {
		return err
	}

	out.line(out.style(dim, fmt.Sprintf("workflow %s · %d stage(s)", headlessWorkflowText(plan.workflow.Name), len(plan.workflow.Stages))))
	result := plan.runner.RunExpandedWorkflow(ctx, plan.workflow, func(text string) {
		out.line(out.style(dim, "  "+headlessWorkflowText(text)))
	})

	renderHeadlessWorkflowResult(out, result)
	switch {
	case result.Canceled:
		return fmt.Errorf("workflow %s was cancelled after %d stage(s)", headlessWorkflowText(plan.workflow.Name), len(result.Stages))
	case result.Err != nil:
		return fmt.Errorf("workflow %s stopped: %w", headlessWorkflowText(plan.workflow.Name), terminalWorkflowError{result.Err})
	}
	return nil
}

// headlessWorkflowText is the only path for repository-, provider-, or
// error-authored bytes into the plain workflow renderer. Escape rather than
// Display keeps an embedded newline visible without allowing it to mint a
// stage, answer, status, or prompt line of its own.
func headlessWorkflowText(text string) string { return cliText(text) }

type terminalWorkflowError struct{ err error }

func (e terminalWorkflowError) Error() string { return headlessWorkflowText(e.err.Error()) }
func (e terminalWorkflowError) Unwrap() error { return e.err }

func renderHeadlessWorkflowResult(out *renderer, result delegate.WorkflowResult) {
	for _, stage := range result.Stages {
		out.line("")
		out.line(out.style(bold, headlessWorkflowText(stage.Stage)) +
			out.style(dim, fmt.Sprintf("  %d answered, %d failed", len(stage.Answers), len(stage.Failed))))
		for _, answer := range stage.Answers {
			out.line(headlessWorkflowText(answer))
		}
		for _, failed := range stage.Failed {
			// firstLine also applies a display cap. Redact the complete failure
			// component first so the cap cannot cut a credential-shaped value
			// below the scanner's length floor and expose its prefix.
			out.line(out.style(dim, "  failed: "+headlessWorkflowText(firstLine(redactWorkflowInput(failed)))))
		}
	}
}
