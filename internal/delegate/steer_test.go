package delegate

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// running starts a task and holds it open so steering has something live to
// reach, the way a real subagent is mid-round when guidance arrives.
func running(t *testing.T, m *TaskManager, name string) (*TaskHandle, func()) {
	t.Helper()
	ref := m.Reserve(name, "do the thing", "t1", "parent-1")
	handles := make(chan *TaskHandle, 1)
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		m.Execute(context.Background(), ref, func(ctx context.Context, h *TaskHandle) (tools.Result, error) {
			handles <- h
			<-release
			return tools.Result{Content: "done"}, nil
		})
	}()
	handle := <-handles
	return handle, func() { close(release); wg.Wait() }
}

func messageText(m provider.Message) string {
	if len(m.Content) == 0 {
		return ""
	}
	text, _ := m.Content[0].(provider.Text)
	return text.Text
}

// A delegated task used to be a sealed envelope. Guidance now reaches a
// running subagent at its next round boundary, which is the only place a
// message can be handed to a model that is already working.
func TestSteerReachesARunningTaskAtItsNextRound(t *testing.T) {
	m := NewTaskManager(2)
	handle, finish := running(t, m, "scout")
	defer finish()
	id := handle.id

	if err := m.Steer(id, "stop reading tests, read the handler"); err != nil {
		t.Fatal(err)
	}
	if err := m.Steer(id, "and check the error path"); err != nil {
		t.Fatal(err)
	}

	// Sent but not yet taken up: the honest answer to "did it get my message".
	snap := snapshotFor(t, m, id)
	if snap.SteersSent != 2 || snap.SteersApplied != 0 {
		t.Fatalf("sent=%d applied=%d, want 2 sent and none applied yet", snap.SteersSent, snap.SteersApplied)
	}

	msgs := handle.injectSteering()
	if len(msgs) != 2 {
		t.Fatalf("the loop drained %d messages, want both", len(msgs))
	}
	text := messageText(msgs[0])
	if !strings.HasPrefix(text, steerPrefix) {
		t.Errorf("a steer must be labelled so the subagent can tell it from its charter: %q", text)
	}
	if !strings.Contains(text, "read the handler") {
		t.Errorf("the message did not survive: %q", text)
	}

	snap = snapshotFor(t, m, id)
	if snap.SteersApplied != 2 {
		t.Fatalf("applied=%d after the drain, want 2", snap.SteersApplied)
	}
	if again := handle.injectSteering(); len(again) != 0 {
		t.Fatal("a drained steer must not be delivered twice")
	}
}

func TestSteerRedactsBeforeItCrossesIntoTheChildSession(t *testing.T) {
	m := NewTaskManager(1)
	handle, finish := running(t, m, "scout")
	defer finish()
	if err := m.Steer(handle.id, "inspect with "+boundaryTestToken); err != nil {
		t.Fatal(err)
	}

	msgs := handle.injectSteering()
	if len(msgs) != 1 {
		t.Fatalf("drained %d steers, want one", len(msgs))
	}
	got := messageText(msgs[0])
	if strings.Contains(got, boundaryTestToken) ||
		!strings.Contains(got, "[redacted: a GitHub token]") {
		t.Fatalf("steer crossed the child boundary unsafely: %q", got)
	}
}

// Accepting a message nothing will read is worse than refusing it.
func TestSteerRefusesAFinishedTask(t *testing.T) {
	m := NewTaskManager(1)
	handle, finish := running(t, m, "scout")
	id := handle.id
	if err := m.Steer(id, "   "); err == nil {
		t.Fatal("an empty steer was accepted")
	}
	if err := m.Steer("no-such-task", "hello"); err == nil {
		t.Fatal("steering a task that does not exist was accepted")
	}
	finish() // the task completes

	if err := m.Steer(id, "too late"); err == nil {
		t.Fatal("steering a finished task was accepted")
	}
}

func TestSteerRefusesTaskAfterCancelLinearizes(t *testing.T) {
	m := NewTaskManager(1)
	handle, finish := running(t, m, "scout")
	finished := false
	defer func() {
		if !finished {
			finish()
		}
	}()

	if err := m.Cancel(handle.id); err != nil {
		t.Fatal(err)
	}
	if err := m.Steer(handle.id, "too late"); err == nil || !strings.Contains(err.Error(), string(TaskCanceling)) {
		t.Fatalf("steer after cancel error = %v", err)
	}
	snap := snapshotFor(t, m, handle.id)
	if snap.Status != TaskCanceling || snap.SteersSent != 0 || snap.SteersApplied != 0 {
		t.Fatalf("canceling task accepted steer state: %+v", snap)
	}
	if pending := handle.injectSteering(); len(pending) != 0 {
		t.Fatalf("canceling task retained steer messages: %+v", pending)
	}

	finish()
	finished = true
	snap = snapshotFor(t, m, handle.id)
	if snap.Status != TaskCanceled || snap.Error != context.Canceled.Error() || snap.SteersSent != 0 {
		t.Fatalf("canceled task terminal state = %+v", snap)
	}
}

// What a task is doing is answerable without opening its sub-session, and the
// tail is bounded because this is a status line, not a transcript.
func TestActivityIsRecordedAndBounded(t *testing.T) {
	m := NewTaskManager(1)
	handle, finish := running(t, m, "scout")
	defer finish()
	for i := 0; i < maxActivityLines*3; i++ {
		handle.RecordActivity(strings.Repeat("x", maxActivityBytes*2))
	}
	snap := snapshotFor(t, m, handle.id)
	if len(snap.Activity) != maxActivityLines {
		t.Fatalf("activity holds %d lines, want the %d bound", len(snap.Activity), maxActivityLines)
	}
	for _, line := range snap.Activity {
		if len(line) > maxActivityBytes+len("…") {
			t.Fatalf("an activity line is %d bytes, past its bound", len(line))
		}
	}
}

func snapshotFor(t *testing.T, m *TaskManager, id string) TaskSnapshot {
	t.Helper()
	for _, task := range m.List() {
		if task.ID == id {
			return task
		}
	}
	t.Fatalf("no task %s", id)
	return TaskSnapshot{}
}

// "It failed" and "it failed after three greps that came back empty" send the
// delegating agent to different next moves, so a failure carries what the
// subagent did. A good answer does not: the preamble promised the final
// message is what survives, and appending a log would contradict it.
func TestFailureCarriesWhatTheSubagentDidAndSuccessDoesNot(t *testing.T) {
	// Two live tasks at once, so the manager needs room for both: a second
	// Execute on a one-slot manager waits for the first to release.
	m := NewTaskManager(2)
	handle, finish := running(t, m, "scout")
	defer finish()
	handle.RecordActivity("grep failed pattern-a")
	handle.RecordActivity("grep failed pattern-b")

	report := handle.activityReport()
	if !strings.Contains(report, "pattern-a") || !strings.Contains(report, "pattern-b") {
		t.Fatalf("the report omitted what it did: %q", report)
	}
	if !strings.Contains(report, "most recent last") {
		t.Errorf("the report should say which end is which: %q", report)
	}

	// A task that did nothing reports nothing rather than an empty heading.
	quiet, quietDone := running(t, m, "quiet")
	defer quietDone()
	if got := quiet.activityReport(); got != "" {
		t.Errorf("a task with no activity reported %q", got)
	}
}

// A user-role message that does not mark itself injected reads as the opening
// of a turn to session.OpensTurn, which is what /fork, /retry, and blame cut
// on. An unmarked steer puts a phantom turn boundary inside the errand.
func TestASteerDoesNotOpenATurn(t *testing.T) {
	m := NewTaskManager(1)
	handle, finish := running(t, m, "scout")
	defer finish()

	if err := m.Steer(handle.id, "look at the handler"); err != nil {
		t.Fatal(err)
	}
	msgs := handle.injectSteering()
	if len(msgs) != 1 {
		t.Fatalf("drained %d messages, want 1", len(msgs))
	}
	if !msgs[0].Injected {
		t.Error("a steer must mark itself injected, like everything else on this seam")
	}
	if session.OpensTurn(msgs[0]) {
		t.Error("a steer was mistaken for the start of a turn")
	}
}
