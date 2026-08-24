package main

import (
	"errors"
	"fmt"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
)

// operationRun owns every asynchronous command stage launched by one
// startOperation generation. Bubble Tea deliberately does not join Cmd
// goroutines at shutdown, so exclusive UI state alone is not process-lifetime
// ownership: a completed result can also be dropped before Update sees it.
type operationRun struct {
	mu sync.Mutex

	generation uint64
	sourceID   string
	cancel     func()
	stopping   bool
	tasks      map[*operationTask]struct{}
	cleanupErr error
}

type operationTask struct {
	owner *operationRun
	done  chan struct{}

	started   bool
	finished  bool
	hasResult bool
	result    tea.Msg
	abandon   func() error
}

// operationTaskResultMsg keeps the task capability beside its result. Update
// claims it before dispatching the inner message; if the program stops first,
// operationRun still owns and disposes the result's sessions and barriers.
type operationTaskResultMsg struct {
	task *operationTask
	msg  tea.Msg
}

func newOperationRun(generation uint64, sourceID string, cancel func()) *operationRun {
	return &operationRun{
		generation: generation,
		sourceID:   sourceID,
		cancel:     cancel,
		tasks:      make(map[*operationTask]struct{}),
	}
}

func (r *operationRun) command(cmd tea.Cmd) tea.Cmd {
	return r.commandWithAbandon(cmd, nil)
}

// commandWithAbandon gives the owner the inputs captured by a queued command
// until Bubble Tea actually starts it. Once the command starts, ownership
// moves into the command and then its result. If shutdown or generation
// retirement wins first, abandon releases those inputs without running work.
func (r *operationRun) commandWithAbandon(cmd tea.Cmd, abandon func() error) tea.Cmd {
	if r == nil || cmd == nil {
		return cmd
	}
	task := &operationTask{owner: r, done: make(chan struct{}), abandon: abandon}
	r.mu.Lock()
	if r.stopping {
		task.finished = true
		abandon = task.abandon
		task.abandon = nil
		close(task.done)
		r.mu.Unlock()
		if abandon != nil {
			r.recordCleanup(abandon())
		}
		return func() tea.Msg { return nil }
	}
	r.tasks[task] = struct{}{}
	r.mu.Unlock()

	return func() tea.Msg {
		r.mu.Lock()
		if r.stopping || task.finished {
			if !task.finished {
				task.finished = true
				close(task.done)
			}
			delete(r.tasks, task)
			r.mu.Unlock()
			return nil
		}
		task.started = true
		// The command now owns every captured input. Its returned message is the
		// next ownership capsule, disposed below if shutdown wins the race.
		task.abandon = nil
		r.mu.Unlock()

		msg := cmd()

		r.mu.Lock()
		if r.stopping {
			r.mu.Unlock()
			cleanupErr := cleanupDroppedOperationResult(msg)
			r.mu.Lock()
			r.cleanupErr = errors.Join(r.cleanupErr, cleanupErr)
			task.finished = true
			close(task.done)
			delete(r.tasks, task)
			r.mu.Unlock()
			return nil
		}
		task.result = msg
		task.hasResult = true
		task.finished = true
		close(task.done)
		r.mu.Unlock()
		return operationTaskResultMsg{task: task, msg: msg}
	}
}

func (r *operationRun) recordCleanup(err error) {
	if r == nil || err == nil {
		return
	}
	r.mu.Lock()
	r.cleanupErr = errors.Join(r.cleanupErr, err)
	r.mu.Unlock()
}

func (r *operationRun) claim(task *operationTask) bool {
	if r == nil || task == nil || task.owner != r {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopping {
		return false
	}
	if _, ok := r.tasks[task]; !ok || !task.finished || !task.hasResult {
		return false
	}
	task.result = nil
	task.hasResult = false
	delete(r.tasks, task)
	return true
}

func (r *operationRun) discard(task *operationTask) error {
	if r == nil || task == nil || task.owner != r {
		return nil
	}
	r.mu.Lock()
	if _, ok := r.tasks[task]; !ok || !task.hasResult {
		r.mu.Unlock()
		return nil
	}
	msg := task.result
	task.result = nil
	task.hasResult = false
	delete(r.tasks, task)
	r.mu.Unlock()
	return cleanupDroppedOperationResult(msg)
}

// retire ends a normally handled generation without blocking the event loop.
// There should be no concurrent task after a claimed completion, but marking
// the owner stopping makes any accidental late stage dispose its own result.
func (r *operationRun) retire() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	r.stopping = true
	if r.cancel != nil {
		r.cancel()
	}
	var pending []tea.Msg
	var abandons []func() error
	for task := range r.tasks {
		if !task.started && !task.finished {
			task.finished = true
			close(task.done)
			if task.abandon != nil {
				abandons = append(abandons, task.abandon)
				task.abandon = nil
			}
		}
		if task.hasResult {
			pending = append(pending, task.result)
			task.result = nil
			task.hasResult = false
			delete(r.tasks, task)
		}
	}
	err := r.cleanupErr
	r.mu.Unlock()
	for _, abandon := range abandons {
		err = errors.Join(err, abandon())
	}
	for _, msg := range pending {
		err = errors.Join(err, cleanupDroppedOperationResult(msg))
	}
	return err
}

// stopAndWait is the abnormal-exit barrier. It cancels the operation, prevents
// a queued-but-unstarted Cmd from beginning, joins every command Bubble Tea did
// launch, then disposes results that never reached Update.
func (r *operationRun) stopAndWait() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	r.stopping = true
	if r.cancel != nil {
		r.cancel()
	}
	waits := make([]<-chan struct{}, 0, len(r.tasks))
	var abandons []func() error
	for task := range r.tasks {
		if !task.started && !task.finished {
			task.finished = true
			close(task.done)
			if task.abandon != nil {
				abandons = append(abandons, task.abandon)
				task.abandon = nil
			}
		}
		waits = append(waits, task.done)
	}
	r.mu.Unlock()
	var abandonErr error
	for _, abandon := range abandons {
		abandonErr = errors.Join(abandonErr, abandon())
	}
	for _, done := range waits {
		<-done
	}

	r.mu.Lock()
	var pending []tea.Msg
	for task := range r.tasks {
		if task.hasResult {
			pending = append(pending, task.result)
			task.result = nil
			task.hasResult = false
		}
		delete(r.tasks, task)
	}
	err := errors.Join(r.cleanupErr, abandonErr)
	r.mu.Unlock()
	for _, msg := range pending {
		err = errors.Join(err, cleanupDroppedOperationResult(msg))
	}
	return err
}

func cleanupDroppedOperationResult(msg tea.Msg) error {
	switch msg := msg.(type) {
	case nil:
		return nil
	case sessionSwapMsg:
		var err error
		if msg.retry != nil {
			err = errors.Join(err, msg.retry.abortPrepared())
		}
		if msg.sess != nil && (msg.retry == nil || msg.sess != msg.retry.source) {
			err = errors.Join(err, msg.sess.CloseDiscardingStaged())
		}
		if msg.publishAfter != nil && msg.publishAfter != msg.sess {
			err = errors.Join(err, msg.publishAfter.CloseDiscardingStaged())
		}
		if msg.release != nil {
			msg.release()
		}
		return err
	case raceSetupMsg:
		var err error
		for _, arm := range msg.arms {
			if arm != nil && arm.sess != nil {
				err = errors.Join(err, arm.sess.CloseDiscardingStaged())
			}
		}
		if msg.release != nil {
			msg.release()
		}
		return err
	case advisorReadyMsg:
		// advisor off pauses the live advisor before returning its result. A
		// dropped result must release that ordinary transition barrier; TUI exit
		// will permanently stop it immediately afterward.
		if msg.action == "off" && msg.adv != nil {
			msg.adv.Resume()
		}
		return nil
	default:
		return nil
	}
}

func (m *tuiModel) ownOperationCmd(generation uint64, cmd tea.Cmd) tea.Cmd {
	return m.ownOperationCmdWithAbandon(generation, cmd, nil)
}

func (m *tuiModel) ownOperationCmdWithAbandon(generation uint64, cmd tea.Cmd, abandon func() error) tea.Cmd {
	if cmd == nil || !m.trackOperationTasks {
		return cmd
	}
	owner := m.operationOwner
	if owner == nil || owner.generation != generation {
		sourceID := m.operationSourceID
		cleanupErr := error(nil)
		if abandon != nil {
			cleanupErr = abandon()
		}
		return func() tea.Msg {
			err := errors.New("an asynchronous operation lost its generation before launch")
			if cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("cleaning its captured inputs: %w", cleanupErr))
			}
			return noticeMsg{
				level: "error", text: err.Error(),
				operation: generation, sourceID: sourceID,
			}
		}
	}
	return owner.commandWithAbandon(cmd, abandon)
}

func (m *tuiModel) claimOperationResult(msg operationTaskResultMsg) (tea.Msg, bool) {
	owner := msg.task.owner
	if owner == m.operationOwner && owner.claim(msg.task) {
		return msg.msg, true
	}
	if err := owner.discard(msg.task); err != nil {
		m.shutdownErr = errors.Join(m.shutdownErr,
			fmt.Errorf("cleaning a stale asynchronous operation result: %w", err))
	}
	return nil, false
}
