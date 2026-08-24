package main

import (
	"fmt"
	"strings"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/delegate"
)

func workflowCredentialLeaks(workflow delegate.Workflow) []credential.Leak {
	var out []credential.Leak
	seen := map[string]bool{}
	for _, stage := range workflow.Stages {
		for _, task := range stage.Tasks {
			for _, leak := range credential.ScanPrompt(task.Task) {
				// The rendering contains only kind and issuer stub. It is enough to
				// avoid repeating the same safe description in the decision UI.
				key := leak.String()
				if !seen[key] {
					seen[key] = true
					out = append(out, leak)
				}
			}
		}
	}
	return out
}

func redactWorkflowCredentials(workflow delegate.Workflow) delegate.Workflow {
	out := workflow
	out.Stages = make([]delegate.Stage, len(workflow.Stages))
	for i, stage := range workflow.Stages {
		out.Stages[i] = stage
		out.Stages[i].Tasks = make([]delegate.WorkflowTask, len(stage.Tasks))
		copy(out.Stages[i].Tasks, stage.Tasks)
		for j := range out.Stages[i].Tasks {
			text := out.Stages[i].Tasks[j].Task
			out.Stages[i].Tasks[j].Task = credential.Redact(text, credential.ScanPrompt(text))
		}
	}
	return out
}

func workflowCredentialRefusal(leaks []credential.Leak) error {
	found := make([]string, len(leaks))
	for i, leak := range leaks {
		found[i] = leak.String()
	}
	return fmt.Errorf("the expanded workflow contains %s; nothing was sent. Redact the arguments, or pass -allow-secrets to run it deliberately",
		strings.Join(found, ", "))
}
