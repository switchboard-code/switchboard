package main

import (
	"context"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
)

// updateTaskOwner gives executable replacement a process-lifetime owner.
// Bubble Tea does not join commands when its terminal disappears, but an
// updater must finish any namespace transition it has entered before main can
// return. One task is admitted at a time so the startup check and /update can
// never race over the same executable and Windows backup name.
type updateTaskOwner struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc

	stopping bool
	active   *updateTask
	stopped  chan struct{}
}

type updateTask struct {
	started  bool
	finished bool
}

func newUpdateTaskOwner() *updateTaskOwner {
	ctx, cancel := context.WithCancel(context.Background())
	return &updateTaskOwner{
		ctx:     ctx,
		cancel:  cancel,
		stopped: make(chan struct{}),
	}
}

// command registers before returning the Tea command. That closes the gap in
// which Run can stop after Init/Update returns a command but before Bubble Tea
// starts it: stop marks an unstarted task finished, and a later invocation is
// a no-op. The bool is false while another update is already registered.
func (o *updateTaskOwner) command(work func(context.Context) tea.Msg) (tea.Cmd, bool) {
	if o == nil || work == nil {
		return nil, false
	}
	o.mu.Lock()
	if o.stopping || o.active != nil {
		o.mu.Unlock()
		return nil, false
	}
	task := &updateTask{}
	o.active = task
	o.mu.Unlock()

	return func() tea.Msg {
		o.mu.Lock()
		if o.stopping || task.started || task.finished || o.active != task {
			o.finishLocked(task)
			o.mu.Unlock()
			return nil
		}
		task.started = true
		ctx := o.ctx
		o.mu.Unlock()

		defer o.finish(task)
		return work(ctx)
	}, true
}

func (o *updateTaskOwner) finish(task *updateTask) {
	if o == nil || task == nil {
		return
	}
	o.mu.Lock()
	o.finishLocked(task)
	o.mu.Unlock()
}

func (o *updateTaskOwner) finishLocked(task *updateTask) {
	if task == nil || task.finished {
		return
	}
	task.finished = true
	if o.active == task {
		o.active = nil
	}
	if o.stopping && o.active == nil {
		select {
		case <-o.stopped:
		default:
			close(o.stopped)
		}
	}
}

// stop cancels network work immediately without waiting for a publication
// already in progress. runTUIProgram uses this first, alongside the other
// lifetime cancellations, then joins after all owners have been signalled.
func (o *updateTaskOwner) stop() {
	if o == nil {
		return
	}
	o.mu.Lock()
	if !o.stopping {
		o.stopping = true
		o.cancel()
	}
	if o.active != nil && !o.active.started {
		o.finishLocked(o.active)
	}
	if o.active == nil {
		select {
		case <-o.stopped:
		default:
			close(o.stopped)
		}
	}
	o.mu.Unlock()
}

func (o *updateTaskOwner) stopAndWait() {
	if o == nil {
		return
	}
	o.stop()
	<-o.stopped
}

func (m *tuiModel) ownUpdateCmd(work func(context.Context) tea.Msg) tea.Cmd {
	if m == nil || work == nil {
		return nil
	}
	if m.updateOwner == nil {
		m.updateOwner = newUpdateTaskOwner()
	}
	cmd, accepted := m.updateOwner.command(work)
	if !accepted {
		return noticeCmd("warn", "an update is already in progress")
	}
	return cmd
}
