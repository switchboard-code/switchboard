package delegate

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/tools"
)

const (
	// DefaultMaxParallel bounds active delegated loops. Calls beyond the limit
	// wait without probing a provider or creating a session.
	DefaultMaxParallel = 4

	// taskHistoryLimit keeps /tasks useful over a long-running process without
	// turning completed errands into an unbounded in-memory log. Their durable
	// sub-sessions remain available for accounting and blame.
	taskHistoryLimit = 100
)

type TaskStatus string

const (
	TaskQueued    TaskStatus = "queued"
	TaskRunning   TaskStatus = "running"
	TaskCanceling TaskStatus = "canceling"
	TaskSucceeded TaskStatus = "succeeded"
	TaskFailed    TaskStatus = "failed"
	TaskCanceled  TaskStatus = "canceled"
)

func (s TaskStatus) terminal() bool {
	return s == TaskSucceeded || s == TaskFailed || s == TaskCanceled
}

// TaskRef is allocated at Plan time, in provider call order. Registration
// waits until Run so a denied delegate permission does not leave a phantom
// queued task behind.
type TaskRef struct {
	ID              string
	Name            string
	Tier            string
	ParentSessionID string
	sequence        uint64
}

func (r TaskRef) Label() string {
	if r.Name == "" {
		return r.ID
	}
	return r.ID + " " + r.Name
}

// TaskSnapshot is the race-free public view used by status surfaces.
type TaskSnapshot struct {
	ID                string
	Name              string
	Status            TaskStatus
	Tier              string
	CostMicroUSD      int64
	Calls             int
	ParentSessionID   string
	DelegateSessionID string
	CreatedAt         time.Time
	StartedAt         time.Time
	FinishedAt        time.Time
	Error             string

	// Activity is a bounded tail of what the subagent has been doing, so a
	// caller can answer "what is it up to" without opening the sub-session.
	Activity []string

	// SteersSent counts guidance accepted for this task and SteersApplied
	// counts what the loop has taken up. They differ while a round is in
	// flight, which is the honest answer to "did it get my message yet".
	SteersSent    int
	SteersApplied int
}

type taskState struct {
	TaskSnapshot
	cancel        context.CancelFunc
	sequence      uint64
	reportedError string

	// pendingSteers is guidance queued for the next round boundary. It is
	// drained by the running loop rather than delivered, because a message
	// cannot be handed to a model mid-call.
	pendingSteers []string
}

// TaskManager owns bounded delegate concurrency, task-local cancellation, and
// the one-at-a-time approval lane shared by otherwise independent subagents.
// It starts no goroutines itself: the agent loop owns workers and joins them at
// its deterministic tool-result barrier.
type TaskManager struct {
	mu       sync.Mutex
	next     uint64
	max      int
	slots    chan struct{}
	approval chan struct{}
	tasks    map[string]*taskState
	order    []string

	// beforeFinish is a deterministic test seam at the only boundary where a
	// completed worker and task-local cancellation can race. Tests install it
	// before starting any task; production leaves it nil.
	beforeFinish func(TaskRef)
}

func NewTaskManager(maxParallel int) *TaskManager {
	if maxParallel <= 0 {
		maxParallel = DefaultMaxParallel
	}
	return &TaskManager{
		max:      maxParallel,
		slots:    make(chan struct{}, maxParallel),
		approval: make(chan struct{}, 1),
		tasks:    make(map[string]*taskState),
	}
}

func (m *TaskManager) MaxParallel() int {
	if m == nil {
		return 0
	}
	return m.max
}

// Reserve allocates identity only. IDs are deterministic for the process
// because the primary loop plans provider tool calls in response order.
func (m *TaskManager) Reserve(name, task, tier, parentSessionID string) TaskRef {
	if m == nil {
		return TaskRef{}
	}
	m.mu.Lock()
	m.next++
	sequence := m.next
	id := fmt.Sprintf("task-%03d", sequence)
	m.mu.Unlock()
	if strings.TrimSpace(name) == "" {
		name = task
	}
	return TaskRef{
		ID:              id,
		Name:            normalizeTaskName(name),
		Tier:            tier,
		ParentSessionID: parentSessionID,
		sequence:        sequence,
	}
}

func normalizeTaskName(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	const maxRunes = 48
	runes := []rune(value)
	if len(runes) > maxRunes {
		value = string(runes[:maxRunes-1]) + "…"
	}
	return value
}

// Execute registers a reserved task, waits for a bounded worker slot, and
// records its terminal state. Canceling this task cancels only the child
// context passed to run; canceling the parent turn still cancels every child.
func (m *TaskManager) Execute(
	ctx context.Context,
	ref TaskRef,
	run func(context.Context, *TaskHandle) (tools.Result, error),
) (res tools.Result, err error) {
	if m == nil {
		return tools.Result{}, errors.New("delegate task manager is unavailable")
	}
	if ref.ID == "" {
		return tools.Result{}, errors.New("delegate task has no identity")
	}
	runCtx, cancel := context.WithCancel(ctx)
	state := &taskState{TaskSnapshot: TaskSnapshot{
		ID: ref.ID, Name: ref.Name, Status: TaskQueued, Tier: ref.Tier,
		ParentSessionID: ref.ParentSessionID, CreatedAt: time.Now(),
	}, cancel: cancel, sequence: ref.sequence}
	if err := m.register(state); err != nil {
		cancel()
		return tools.Result{}, err
	}
	defer func() {
		if m.beforeFinish != nil {
			m.beforeFinish(ref)
		}
		canceled, ctxErr := m.finish(ref.ID, res, err, runCtx)
		cancel()
		if canceled {
			// A successful result and a canceled task row must never describe the
			// same execution. Workflow stages consume this return value directly;
			// suppressing it here also prevents canceled evidence from being
			// carried into a later stage.
			res = tools.Result{}
			if ctxErr != nil {
				err = ctxErr
			} else if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				err = context.Canceled
			}
		}
	}()

	select {
	case m.slots <- struct{}{}:
		defer func() { <-m.slots }()
	case <-runCtx.Done():
		return tools.Result{}, runCtx.Err()
	}
	if err := runCtx.Err(); err != nil {
		return tools.Result{}, err
	}
	if err := m.setRunning(ref.ID); err != nil {
		return tools.Result{}, err
	}
	return run(runCtx, &TaskHandle{manager: m, id: ref.ID})
}

func (m *TaskManager) register(state *taskState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.tasks[state.ID]; exists {
		return fmt.Errorf("delegate task %s is already registered", state.ID)
	}
	if !m.pruneLocked() {
		return fmt.Errorf("delegate task queue is full (%d nonterminal tasks)", taskHistoryLimit)
	}
	m.tasks[state.ID] = state
	m.order = append(m.order, state.ID)
	sort.SliceStable(m.order, func(i, j int) bool {
		left, right := m.tasks[m.order[i]], m.tasks[m.order[j]]
		if left.sequence == right.sequence {
			return left.ID < right.ID
		}
		return left.sequence < right.sequence
	})
	return nil
}

func (m *TaskManager) pruneLocked() bool {
	for len(m.order) >= taskHistoryLimit {
		removed := false
		for i, id := range m.order {
			state := m.tasks[id]
			if state == nil || state.Status.terminal() {
				delete(m.tasks, id)
				m.order = append(m.order[:i], m.order[i+1:]...)
				removed = true
				break
			}
		}
		if !removed {
			return false
		}
	}
	return true
}

func (m *TaskManager) setRunning(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state := m.tasks[id]; state != nil {
		if state.Status == TaskCanceling {
			return context.Canceled
		}
		state.Status = TaskRunning
		state.StartedAt = time.Now()
		return nil
	}
	return errors.New("delegate task disappeared before it started")
}

func (m *TaskManager) finish(id string, res tools.Result, runErr error, runCtx context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.tasks[id]
	if state == nil {
		return false, nil
	}
	ctxErr := runCtx.Err()
	canceled := ctxErr != nil || errors.Is(runErr, context.Canceled) || state.Status == TaskCanceling
	state.FinishedAt = time.Now()
	switch {
	case canceled:
		state.Status = TaskCanceled
		switch {
		case ctxErr != nil:
			state.Error = ctxErr.Error()
		case runErr != nil:
			state.Error = runErr.Error()
		default:
			state.Error = context.Canceled.Error()
		}
	case runErr != nil:
		state.Status = TaskFailed
		state.Error = runErr.Error()
	case res.IsError:
		state.Status = TaskFailed
		state.Error = firstTaskLine(res.Content)
	case state.reportedError != "":
		// A delegate may return useful partial text to the parent as a normal
		// tool result even though its own loop stopped early. Keep that content
		// usable without misreporting the task as a clean success in /tasks.
		state.Status = TaskFailed
		state.Error = firstTaskLine(state.reportedError)
	default:
		state.Status = TaskSucceeded
	}
	state.cancel = nil
	return canceled, ctxErr
}

func firstTaskLine(value string) string {
	value = strings.TrimSpace(value)
	line, _, _ := strings.Cut(value, "\n")
	const max = 160
	if utf8.RuneCountInString(line) <= max {
		return line
	}
	runes := []rune(line)
	return string(runes[:max-1]) + "…"
}

// Cancel targets one queued or running delegate without touching its siblings.
func (m *TaskManager) Cancel(id string) error {
	if m == nil {
		return errors.New("delegate task manager is unavailable")
	}
	m.mu.Lock()
	state := m.tasks[id]
	if state == nil {
		m.mu.Unlock()
		return fmt.Errorf("no delegate task %s", id)
	}
	if state.Status.terminal() {
		status := state.Status
		m.mu.Unlock()
		return fmt.Errorf("delegate task %s already %s", id, status)
	}
	state.Status = TaskCanceling
	cancel := state.cancel
	if cancel != nil {
		// Cancel while the state lock is still held. Execute cannot observe the
		// canceling transition before its child context carries the same fact.
		cancel()
	}
	m.mu.Unlock()
	return nil
}

func (m *TaskManager) List() []TaskSnapshot {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TaskSnapshot, 0, len(m.order))
	for _, id := range m.order {
		if state := m.tasks[id]; state != nil {
			out = append(out, state.TaskSnapshot)
		}
	}
	return out
}

// TaskHandle lets assembly attach facts that become known only after a worker
// starts: the durable delegate session and its measured usage as each provider
// receipt becomes durable.
type TaskHandle struct {
	manager *TaskManager
	id      string
}

func (h *TaskHandle) AttachSession(id string) {
	if h == nil || h.manager == nil {
		return
	}
	h.manager.mu.Lock()
	defer h.manager.mu.Unlock()
	if state := h.manager.tasks[h.id]; state != nil {
		state.DelegateSessionID = id
	}
}

func (h *TaskHandle) RecordTier(tier string) {
	if h == nil || h.manager == nil || tier == "" {
		return
	}
	h.manager.mu.Lock()
	defer h.manager.mu.Unlock()
	if state := h.manager.tasks[h.id]; state != nil {
		state.Tier = tier
	}
}

func (h *TaskHandle) RecordUsage(calls int, costMicroUSD int64) {
	if h == nil || h.manager == nil {
		return
	}
	h.manager.mu.Lock()
	defer h.manager.mu.Unlock()
	if state := h.manager.tasks[h.id]; state != nil {
		state.Calls = calls
		state.CostMicroUSD = costMicroUSD
	}
}

// RecordFailure marks a task unsuccessful without forcing its returned tool
// result to be an error. Delegates use this when an interrupted/provider-failed
// loop still has a complete earlier answer worth returning to the parent.
func (h *TaskHandle) RecordFailure(message string) {
	if h == nil || h.manager == nil || strings.TrimSpace(message) == "" {
		return
	}
	h.manager.mu.Lock()
	defer h.manager.mu.Unlock()
	if state := h.manager.tasks[h.id]; state != nil {
		state.reportedError = message
	}
}

// AttributedAsker serializes human prompts and labels each one. The engine has
// already applied rules to the original request; Detail is display-only, so
// adding attribution cannot broaden a remembered grant or change execution.
func (m *TaskManager) AttributedAsker(ref TaskRef, parent permission.Asker) permission.Asker {
	return &taskAsker{manager: m, ref: ref, parent: parent}
}

type taskAsker struct {
	manager *TaskManager
	ref     TaskRef
	parent  permission.Asker
}

func (a *taskAsker) Ask(ctx context.Context, req permission.Request, out permission.Outcome) (permission.Response, error) {
	if a.parent == nil {
		return permission.Response{}, errors.New("delegate has no permission asker")
	}
	if a.manager == nil {
		return permission.Response{}, errors.New("delegate task manager is unavailable")
	}
	select {
	case a.manager.approval <- struct{}{}:
		defer func() { <-a.manager.approval }()
	case <-ctx.Done():
		return permission.Response{}, ctx.Err()
	}
	req = attributeRequest(a.ref, req)
	return a.parent.Ask(ctx, req, out)
}

func attributeRequest(ref TaskRef, req permission.Request) permission.Request {
	detail := req.Detail
	if strings.TrimSpace(detail) == "" && req.Effect != permission.EffectExecute {
		detail = req.Path
	}
	req.Detail = attributeDetail(ref, detail)
	return req
}

func attributeDetail(ref TaskRef, detail string) string {
	prefix := "[" + ref.Label() + "]"
	if strings.TrimSpace(detail) == "" {
		return prefix
	}
	return prefix + " " + detail
}
