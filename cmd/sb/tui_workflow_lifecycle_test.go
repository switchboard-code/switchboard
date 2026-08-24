package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/delegate"
)

func TestWorkflowCompletionDrainsOneQueuedPromptForEveryOutcome(t *testing.T) {
	tests := []struct {
		name   string
		result delegate.WorkflowResult
	}{
		{
			name: "success",
			result: delegate.WorkflowResult{Stages: []delegate.StageResult{{
				Stage: "collect", Answers: []string{"done"},
			}}},
		},
		{
			name:   "canceled",
			result: delegate.WorkflowResult{Canceled: true, Err: context.Canceled},
		},
		{
			name:   "error",
			result: delegate.WorkflowResult{Err: errors.New("injected workflow failure")},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := testModel(t)
			_, generation, sourceID, err := m.startOperation("workflow review")
			if err != nil {
				t.Fatal(err)
			}
			if cmd := m.enqueue("queued after workflow", ""); cmd != nil || len(m.queue) != 1 {
				t.Fatalf("prompt did not queue behind workflow: cmd=%v queue=%#v", cmd != nil, m.queue)
			}
			msg := workflowDoneMsg{
				generation: generation, sourceID: sourceID, name: "review", result: test.result,
			}
			next := m.onWorkflowDone(msg)
			if next == nil || len(m.queue) != 0 || m.operationActive || !m.turnPlanning || !m.busy {
				t.Fatalf("workflow completion did not launch exactly one queued turn: next=%v queue=%#v operation=%v planning=%v busy=%v",
					next != nil, m.queue, m.operationActive, m.turnPlanning, m.busy)
			}
			planned, ok := next().(turnPlanMsg)
			if !ok || !strings.Contains(planned.prompt, "queued after workflow") {
				t.Fatalf("queued launch = %#v", planned)
			}
			if duplicate := m.onWorkflowDone(msg); duplicate != nil {
				t.Fatal("duplicate workflow completion launched queued work twice")
			}
			m.finishPlanning()
		})
	}
}

func TestStaleWorkflowCompletionCannotDrainQueuedPrompt(t *testing.T) {
	m := testModel(t)
	_, generation, sourceID, err := m.startOperation("workflow review")
	if err != nil {
		t.Fatal(err)
	}
	if cmd := m.enqueue("queued behind current workflow", ""); cmd != nil {
		t.Fatal("prompt started while workflow owned the operation lane")
	}
	before := len(m.tr.entries)
	for _, stale := range []workflowDoneMsg{
		{generation: generation + 1, sourceID: sourceID, name: "stale generation"},
		{generation: generation, sourceID: sourceID + "-stale", name: "stale source"},
	} {
		if next := m.onWorkflowDone(stale); next != nil {
			t.Fatalf("stale workflow result launched work: %+v", stale)
		}
		if len(m.queue) != 1 || !m.operationActive || !m.busy || m.turnPlanning || len(m.tr.entries) != before {
			t.Fatalf("stale workflow result mutated ownership: queue=%#v operation=%v busy=%v planning=%v transcript=%d/%d",
				m.queue, m.operationActive, m.busy, m.turnPlanning, len(m.tr.entries), before)
		}
	}

	next := m.onWorkflowDone(workflowDoneMsg{generation: generation, sourceID: sourceID, name: "current"})
	if next == nil || len(m.queue) != 0 {
		t.Fatalf("current completion did not drain the retained prompt: next=%v queue=%#v", next != nil, m.queue)
	}
	m.finishPlanning()
}
