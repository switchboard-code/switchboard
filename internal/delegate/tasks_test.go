package delegate

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/tools"
)

func TestTaskManagerOverlapsDelegatesWithinBound(t *testing.T) {
	manager := NewTaskManager(2)
	refs := []TaskRef{
		manager.Reserve("one", "", "t1", "parent"),
		manager.Reserve("two", "", "t1", "parent"),
		manager.Reserve("three", "", "t1", "parent"),
	}
	started := make(chan string, len(refs))
	release := make(chan struct{})
	var active atomic.Int32
	var peak atomic.Int32
	var wg sync.WaitGroup
	for _, ref := range refs {
		ref := ref
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := manager.Execute(context.Background(), ref, func(context.Context, *TaskHandle) (tools.Result, error) {
				now := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); now > old && !peak.CompareAndSwap(old, now); old = peak.Load() {
				}
				started <- ref.ID
				<-release
				return tools.Result{Content: ref.ID}, nil
			})
			if err != nil {
				t.Errorf("%s: %v", ref.ID, err)
			}
		}()
	}

	<-started
	<-started
	select {
	case id := <-started:
		t.Fatalf("third task %s crossed the two-worker bound", id)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	if got := peak.Load(); got != 2 {
		t.Fatalf("peak active delegates = %d, want 2", got)
	}
	tasks := manager.List()
	if len(tasks) != 3 {
		t.Fatalf("tasks = %+v", tasks)
	}
	for i, task := range tasks {
		wantID := fmt.Sprintf("task-%03d", i+1)
		if task.ID != wantID || task.Status != TaskSucceeded {
			t.Fatalf("task %d = %+v, want deterministic %s succeeded", i, task, wantID)
		}
	}
}

func TestCancelOneTaskDoesNotCancelSibling(t *testing.T) {
	manager := NewTaskManager(2)
	first := manager.Reserve("first", "", "t1", "parent")
	second := manager.Reserve("second", "", "t2", "parent")
	started := make(chan string, 2)
	releaseSecond := make(chan struct{})
	done := make(chan error, 2)
	run := func(ref TaskRef) {
		_, err := manager.Execute(context.Background(), ref, func(ctx context.Context, _ *TaskHandle) (tools.Result, error) {
			started <- ref.ID
			if ref.ID == first.ID {
				<-ctx.Done()
				return tools.Result{}, ctx.Err()
			}
			select {
			case <-releaseSecond:
				return tools.Result{Content: "done"}, nil
			case <-ctx.Done():
				return tools.Result{}, ctx.Err()
			}
		})
		done <- err
	}
	go run(first)
	go run(second)
	<-started
	<-started
	if err := manager.Cancel(first.ID); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled task error = %v", err)
	}
	byID := taskMap(manager.List())
	if byID[first.ID].Status != TaskCanceled {
		t.Fatalf("first status = %s", byID[first.ID].Status)
	}
	if byID[second.ID].Status != TaskRunning {
		t.Fatalf("sibling status = %s, want running", byID[second.ID].Status)
	}
	close(releaseSecond)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := taskMap(manager.List())[second.ID].Status; got != TaskSucceeded {
		t.Fatalf("sibling status = %s", got)
	}
}

func TestCancelWinningTerminalRaceSuppressesSuccessfulResult(t *testing.T) {
	manager := NewTaskManager(1)
	ref := manager.Reserve("cancel at completion", "", "t1", "parent")
	started := make(chan struct{})
	release := make(chan struct{})
	type outcome struct {
		result tools.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := manager.Execute(context.Background(), ref, func(context.Context, *TaskHandle) (tools.Result, error) {
			close(started)
			<-release
			// Model a provider that completed concurrently and did not observe
			// cancellation before handing its terminal answer to the owner.
			return tools.Result{Content: "must not escape"}, nil
		})
		done <- outcome{result: result, err: err}
	}()

	<-started
	if err := manager.Cancel(ref.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	got := <-done
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("cancel-wins Execute error = %v, want context.Canceled", got.err)
	}
	if got.result != (tools.Result{}) {
		t.Fatalf("cancel-wins Execute leaked result %+v", got.result)
	}
	task := taskMap(manager.List())[ref.ID]
	if task.Status != TaskCanceled || task.Error != context.Canceled.Error() {
		t.Fatalf("cancel-wins terminal state = %+v", task)
	}
}

func TestCancelQueuedTaskDoesNotWaitForWorkerSlot(t *testing.T) {
	manager := NewTaskManager(1)
	running := manager.Reserve("running", "", "t1", "parent")
	queued := manager.Reserve("queued", "", "t1", "parent")
	started := make(chan struct{})
	release := make(chan struct{})
	runningDone := make(chan error, 1)
	go func() {
		_, err := manager.Execute(context.Background(), running, func(context.Context, *TaskHandle) (tools.Result, error) {
			close(started)
			<-release
			return tools.Result{Content: "done"}, nil
		})
		runningDone <- err
	}()
	<-started
	queuedDone := make(chan error, 1)
	go func() {
		_, err := manager.Execute(context.Background(), queued, func(context.Context, *TaskHandle) (tools.Result, error) {
			return tools.Result{Content: "must not start"}, nil
		})
		queuedDone <- err
	}()
	deadline := time.Now().Add(time.Second)
	for taskMap(manager.List())[queued.ID].Status != TaskQueued && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := taskMap(manager.List())[queued.ID].Status; got != TaskQueued {
		t.Fatalf("queued task status = %s", got)
	}
	if err := manager.Cancel(queued.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-queuedDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued cancellation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued task waited for the occupied worker after cancellation")
	}
	if got := taskMap(manager.List())[running.ID].Status; got != TaskRunning {
		t.Fatalf("running sibling status = %s", got)
	}
	close(release)
	if err := <-runningDone; err != nil {
		t.Fatal(err)
	}
}

func TestCancelingTransitionCannotStartAsFailure(t *testing.T) {
	manager := NewTaskManager(1)
	// Occupy the worker lane directly so Execute registers and waits in the
	// exact queued state involved in the former Cancel unlock/cancel window.
	manager.slots <- struct{}{}
	ref := manager.Reserve("queued cancellation race", "", "t1", "parent")
	var ran atomic.Bool
	done := make(chan error, 1)
	go func() {
		_, err := manager.Execute(context.Background(), ref, func(context.Context, *TaskHandle) (tools.Result, error) {
			ran.Store(true)
			return tools.Result{Content: "must not run"}, nil
		})
		done <- err
	}()

	deadline := time.Now().Add(time.Second)
	for len(manager.List()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	manager.mu.Lock()
	state := manager.tasks[ref.ID]
	if state == nil {
		manager.mu.Unlock()
		t.Fatal("task did not register")
	}
	// Model the old observable gap exactly: status already says canceling but
	// the child context has not been canceled yet.
	state.Status = TaskCanceling
	manager.mu.Unlock()
	<-manager.slots

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued cancel transition returned %v, want context.Canceled", err)
	}
	if ran.Load() {
		t.Fatal("canceling queued task entered its run closure")
	}
	task := taskMap(manager.List())[ref.ID]
	if task.Status != TaskCanceled || task.Error != context.Canceled.Error() {
		t.Fatalf("canceling task terminal state = %+v", task)
	}
}

func TestApprovalLaneSerializesAndAttributesTasks(t *testing.T) {
	manager := NewTaskManager(2)
	first := manager.Reserve("review api", "", "t1", "parent")
	second := manager.Reserve("run tests", "", "t2", "parent")
	parent := &gatedAsker{started: make(chan permission.Request, 2), release: make(chan struct{}, 2)}
	done := make(chan error, 2)
	for _, ref := range []TaskRef{first, second} {
		ref := ref
		go func() {
			_, err := manager.AttributedAsker(ref, parent).Ask(context.Background(), permission.Request{
				Tool: "exec", Effect: permission.EffectExecute, Argv: []string{"go", "test"},
			}, permission.Outcome{Decision: permission.Ask})
			done <- err
		}()
	}
	req1 := <-parent.started
	select {
	case req := <-parent.started:
		t.Fatalf("approval prompts overlapped: %+v then %+v", req1, req)
	case <-time.After(30 * time.Millisecond):
	}
	parent.release <- struct{}{}
	req2 := <-parent.started
	parent.release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got := req1.Detail + "\n" + req2.Detail
	for _, want := range []string{"[" + first.Label() + "]", "[" + second.Label() + "]"} {
		if !contains(got, want) {
			t.Fatalf("approval details %q do not identify %q", got, want)
		}
	}
	if parent.peak.Load() != 1 {
		t.Fatalf("peak simultaneous approval prompts = %d", parent.peak.Load())
	}
}

func TestTaskAttributionKeepsTheOriginalPathVisible(t *testing.T) {
	ref := TaskRef{ID: "task-009", Name: "edit config"}
	request := attributeRequest(ref, permission.Request{
		Tool: "write", Effect: permission.EffectWrite, Path: "/workspace/config.toml",
	})
	for _, want := range []string{"task-009 edit config", "/workspace/config.toml"} {
		if !contains(request.Detail, want) {
			t.Fatalf("attributed write detail %q hid %q", request.Detail, want)
		}
	}
	if request.Path != "/workspace/config.toml" {
		t.Fatalf("attribution mutated policy path to %q", request.Path)
	}
}

func TestTaskStatusCarriesSessionTierCostAndCalls(t *testing.T) {
	manager := NewTaskManager(1)
	ref := manager.Reserve("inspect ledger", "", "t2", "primary-session")
	_, err := manager.Execute(context.Background(), ref, func(_ context.Context, handle *TaskHandle) (tools.Result, error) {
		handle.AttachSession("delegate-session")
		handle.RecordUsage(3, 12_345)
		return tools.Result{Content: "done"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	task := manager.List()[0]
	if task.ParentSessionID != "primary-session" || task.DelegateSessionID != "delegate-session" ||
		task.Tier != "t2" || task.CostMicroUSD != 12_345 || task.Calls != 3 || task.Status != TaskSucceeded {
		t.Fatalf("task attribution = %+v", task)
	}
}

func TestTaskQueueRefusesUnboundedNonterminalHistory(t *testing.T) {
	manager := NewTaskManager(1)
	for i := 0; i < taskHistoryLimit; i++ {
		ref := manager.Reserve(fmt.Sprintf("queued %d", i), "", "t1", "parent")
		if err := manager.register(&taskState{TaskSnapshot: TaskSnapshot{
			ID: ref.ID, Name: ref.Name, Tier: ref.Tier, ParentSessionID: ref.ParentSessionID, Status: TaskQueued,
		}}); err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
	}
	extra := manager.Reserve("one too many", "", "t1", "parent")
	err := manager.register(&taskState{TaskSnapshot: TaskSnapshot{ID: extra.ID, Status: TaskQueued}})
	if err == nil || !contains(err.Error(), "queue is full") {
		t.Fatalf("overflow register error = %v", err)
	}
	if got := len(manager.List()); got != taskHistoryLimit {
		t.Fatalf("task history grew to %d, want %d", got, taskHistoryLimit)
	}
}

type gatedAsker struct {
	started chan permission.Request
	release chan struct{}
	active  atomic.Int32
	peak    atomic.Int32
}

func (a *gatedAsker) Ask(_ context.Context, req permission.Request, _ permission.Outcome) (permission.Response, error) {
	now := a.active.Add(1)
	defer a.active.Add(-1)
	for old := a.peak.Load(); now > old && !a.peak.CompareAndSwap(old, now); old = a.peak.Load() {
	}
	a.started <- req
	<-a.release
	return permission.Response{Approved: true}, nil
}

func taskMap(tasks []TaskSnapshot) map[string]TaskSnapshot {
	out := make(map[string]TaskSnapshot, len(tasks))
	for _, task := range tasks {
		out[task.ID] = task
	}
	return out
}

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
