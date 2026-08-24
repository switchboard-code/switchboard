package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/switchboard-code/switchboard/internal/continuity"
	"github.com/switchboard-code/switchboard/internal/permission"
)

// TodoStatus is the lifecycle of one task. Three states, no more: a richer
// vocabulary reads as project management, and the model maintaining the list
// is mid-task with a fixed round budget.
type TodoStatus string

const (
	TodoPending TodoStatus = "pending"
	TodoActive  TodoStatus = "active"
	TodoDone    TodoStatus = "done"
)

type TodoItem struct {
	Text   string     `json:"text"`
	Status TodoStatus `json:"status"`
}

// todoState is the live, session-scoped mirror used by tools and the UI. The
// agent persists a bounded advisory copy only after the matching tool-result
// batch is durable, and explicitly hydrates or clears this mirror when it
// binds another session.
type todoState struct {
	mu    sync.Mutex
	items []TodoItem

	// working is what the model last said about the job itself. It is held
	// here beside the list because the two travel together into the capsule,
	// and a surface that read one without the other would carry checkboxes
	// with no reason attached.
	working continuity.Working
}

// restore replaces the durable task and working snapshots together. Unlike a
// model-authored todo update, restoration must not merge omitted fields: an
// empty value belongs to the session being restored and clears whatever the
// previously bound session left in memory.
func (s *todoState) restore(items []TodoItem, working continuity.Working) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append([]TodoItem(nil), items...)
	s.working = working
}

// setWorking replaces the list and folds in whatever the model said about the
// job. Objective and stop condition persist when the call omits them, because
// the list changes far more often than the reason for it; next action does not,
// because a stale one names a step the model has already left behind.
func (s *todoState) setWorking(items []TodoItem, working continuity.Working) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append([]TodoItem(nil), items...)
	s.working.NextAction = working.NextAction
	if working.Objective != "" {
		s.working.Objective = working.Objective
	}
	if working.StopCondition != "" {
		s.working.StopCondition = working.StopCondition
	}
}

// Working returns what the model last said about the job, for the agent to
// fold into the capsule beside the task list.
func (r *Registry) Working() continuity.Working {
	r.todos.mu.Lock()
	defer r.todos.mu.Unlock()
	return r.todos.working
}

// Todos returns a snapshot of the current task list. It is safe to call from
// outside the loop's goroutine, which is how a UI reads it: the ToolEnd event
// says the list changed, this says what it now is.
func (r *Registry) Todos() []TodoItem {
	r.todos.mu.Lock()
	defer r.todos.mu.Unlock()
	return append([]TodoItem(nil), r.todos.items...)
}

// RestoreContinuity hydrates the registry from a validated durable continuity
// capsule. The task list and working context are replaced under one lock after
// validation succeeds, so a failed restore changes neither. Passing nil tasks
// and a zero Working explicitly clears the old session's in-memory state.
func (r *Registry) RestoreContinuity(items []TodoItem, working continuity.Working) error {
	prepared, err := prepareTodoItems(items)
	if err != nil {
		return err
	}
	r.todos.restore(prepared, working)
	return nil
}

// RestoreTodos is the task-only compatibility form. A caller with no durable
// working fields must clear them rather than inherit them from another
// session; callers restoring a full capsule should use RestoreContinuity.
func (r *Registry) RestoreTodos(items []TodoItem) error {
	return r.RestoreContinuity(items, continuity.Working{})
}

const maxTodoItems = 50

type todoTool struct{ r *Registry }

func (t *todoTool) Name() string { return "todo" }

func (t *todoTool) Description() string {
	return "Maintain the task list for the current job. Send the whole list each time; it " +
		"replaces the previous one. Statuses are pending, active, and done, with at most " +
		"one item active. Use it for work with three or more distinct steps: write the " +
		"list before starting, mark each step active when you begin it and done when it " +
		"is finished. Skip it for single-step tasks. " +
		"objective, next_action, and stop_condition are what survives a context boundary " +
		"with the list: why this work is being done, what you would do next, and what " +
		"would mean it is finished. Set objective and stop_condition once when the job " +
		"starts; they are kept until you change them. A list that crosses a boundary " +
		"without them arrives as checkboxes whose point is gone."
}

// ParallelSafe is false because each call replaces the whole list, and two
// concurrent replacements would leave whichever finished last.
func (t *todoTool) ParallelSafe() bool { return false }

func (t *todoTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "items": {
      "type": "array",
      "description": "The complete task list, replacing the previous one. An empty list clears it.",
      "items": {
        "type": "object",
        "properties": {
          "text": {"type": "string", "description": "The task, imperative and short."},
          "status": {"type": "string", "enum": ["pending", "active", "done"]}
        },
        "required": ["text", "status"]
      }
    },
    "objective": {"type": "string", "description": "What this work is for, in a sentence. Kept until changed."},
    "next_action": {"type": "string", "description": "The single thing you would do next. Cleared on every call that does not set it, because a stale one is worse than none."},
    "stop_condition": {"type": "string", "description": "What would mean this job is finished. Kept until changed."}
  },
  "required": ["items"]
}`)
}

type todoInput struct {
	Items         []TodoItem `json:"items"`
	Objective     string     `json:"objective"`
	NextAction    string     `json:"next_action"`
	StopCondition string     `json:"stop_condition"`
}

func (t *todoTool) Plan(input json.RawMessage) (Plan, error) {
	var in todoInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Plan{}, fmt.Errorf("todo: %w", err)
	}
	prepared, err := prepareTodoItems(in.Items)
	if err != nil {
		return Plan{}, err
	}
	in.Items = prepared

	// The list is session state, not an effect on the world, so it carries the
	// read effect: allowed in every mode, plan mode included, because planning
	// is exactly when a task list earns its place.
	return Plan{
		Request: permission.Request{Tool: t.Name(), Effect: permission.EffectRead},
		Run: func(context.Context) (Result, error) {
			working := continuity.Working{
				Objective:     strings.TrimSpace(in.Objective),
				NextAction:    strings.TrimSpace(in.NextAction),
				StopCondition: strings.TrimSpace(in.StopCondition),
			}
			t.r.todos.setWorking(in.Items, working)
			return Result{Content: renderTodos(in.Items)}, nil
		},
	}, nil
}

func prepareTodoItems(items []TodoItem) ([]TodoItem, error) {
	if len(items) > maxTodoItems {
		return nil, fmt.Errorf("todo: %d items; a list this long is a plan document, not a task list. Keep it under %d", len(items), maxTodoItems)
	}
	active := 0
	tasks := make([]continuity.Task, len(items))
	for i, item := range items {
		if strings.TrimSpace(item.Text) == "" {
			return nil, fmt.Errorf("todo: item %d has no text", i+1)
		}
		switch item.Status {
		case TodoPending, TodoDone:
		case TodoActive:
			active++
		default:
			return nil, fmt.Errorf("todo: item %d has status %q; use pending, active, or done", i+1, item.Status)
		}
		tasks[i] = continuity.Task{Text: item.Text, Status: continuity.TaskStatus(item.Status)}
	}
	if active > 1 {
		return nil, fmt.Errorf("todo: %d items are active; work on one thing at a time", active)
	}
	prepared, err := continuity.PrepareTasks(tasks)
	if err != nil {
		return nil, fmt.Errorf("todo: %w", err)
	}
	out := make([]TodoItem, len(prepared))
	for i, item := range prepared {
		out[i] = TodoItem{Text: item.Text, Status: TodoStatus(item.Status)}
	}
	return out, nil
}

// renderTodos is the model-facing rendering. Plain markers, one line per
// item: the model pastes from its own previous output when it updates the
// list, so the format has to survive a round trip through its context.
func renderTodos(items []TodoItem) string {
	if len(items) == 0 {
		return "task list cleared"
	}
	done := 0
	var b strings.Builder
	for _, item := range items {
		mark := "[ ]"
		switch item.Status {
		case TodoActive:
			mark = "[>]"
		case TodoDone:
			mark = "[x]"
			done++
		}
		fmt.Fprintf(&b, "%s %s\n", mark, item.Text)
	}
	fmt.Fprintf(&b, "%d of %d done", done, len(items))
	return b.String()
}
