package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/delegate"
	"github.com/switchboard-code/switchboard/internal/tools"
)

func TestTasksSurfaceFiltersAndLabelsParentSession(t *testing.T) {
	manager := delegate.NewTaskManager(2)
	current := "primary-current"
	currentRef := manager.Reserve("review changes", "", "t2", current)
	_, err := manager.Execute(context.Background(), currentRef, func(_ context.Context, handle *delegate.TaskHandle) (tools.Result, error) {
		handle.AttachSession("delegate-current")
		handle.RecordUsage(2, 8_765)
		return tools.Result{Content: "done"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	oldRef := manager.Reserve("old work", "", "t1", "primary-old")
	if _, err := manager.Execute(context.Background(), oldRef, func(context.Context, *delegate.TaskHandle) (tools.Result, error) {
		return tools.Result{Content: "old"}, nil
	}); err != nil {
		t.Fatal(err)
	}

	visible := tasksForSession(manager, current)
	if len(visible) != 1 || visible[0].ID != currentRef.ID {
		t.Fatalf("current-session tasks = %+v", visible)
	}
	got := renderTasks(visible, manager.MaxParallel(), current)
	for _, want := range []string{currentRef.ID, "succeeded", "t2", "$0.0088", "primary-current", "delegate-current", "2 calls"} {
		if !strings.Contains(got, want) {
			t.Fatalf("task surface hid %q:\n%s", want, got)
		}
	}
	for _, stale := range []string{oldRef.ID, "primary-old", "old work"} {
		if strings.Contains(got, stale) {
			t.Fatalf("task surface leaked another session's %q:\n%s", stale, got)
		}
	}
}

type recordingTaskSteerer struct {
	id, message string
	calls       int
}

func (s *recordingTaskSteerer) Steer(id, message string) error {
	s.id, s.message = id, message
	s.calls++
	return nil
}

func TestTaskSteerUsesTheOutboundCredentialGate(t *testing.T) {
	m := testModel(t)
	target := &recordingTaskSteerer{}
	prompt := "use " + testGitHubToken + " while checking the child"

	if cmd := guardedTaskSteer(m, target, "task-007", prompt); cmd != nil {
		t.Fatal("a secret-bearing task steer ran before the gate resolved")
	}
	if target.calls != 0 || m.dlg == nil {
		t.Fatalf("pre-gate calls = %d, dialog = %T", target.calls, m.dlg)
	}
	cmd := chooseRedact(m)
	if cmd != nil {
		_ = cmd()
	}
	if target.calls != 1 || target.id != "task-007" || strings.Contains(target.message, testGitHubToken) ||
		!strings.Contains(target.message, "[redacted: a GitHub token]") {
		t.Fatalf("redacted task steer = calls %d, id %q, message %q", target.calls, target.id, target.message)
	}

	target = &recordingTaskSteerer{}
	m.dlg = nil
	guardedTaskSteer(m, target, "task-008", prompt)
	done, _ := m.dlg.update(tea.KeyMsg{Type: tea.KeyEscape}, m.th)
	if !done || target.calls != 0 {
		t.Fatalf("dropping task steer resolved=%v calls=%d", done, target.calls)
	}
}

func TestTasksCancelTargetsOneDelegate(t *testing.T) {
	m := testModel(t)
	manager := delegate.NewTaskManager(2)
	previous := subagentTasks
	subagentTasks = manager
	t.Cleanup(func() { subagentTasks = previous })
	parent := m.app.loop.Session.ID()
	first := manager.Reserve("cancel me", "", "t1", parent)
	second := manager.Reserve("keep me", "", "t1", parent)
	started := make(chan string, 2)
	release := make(chan struct{})
	done := make(chan error, 2)
	for _, ref := range []delegate.TaskRef{first, second} {
		ref := ref
		go func() {
			_, err := manager.Execute(context.Background(), ref, func(ctx context.Context, _ *delegate.TaskHandle) (tools.Result, error) {
				started <- ref.ID
				select {
				case <-release:
					return tools.Result{Content: "done"}, nil
				case <-ctx.Done():
					return tools.Result{}, ctx.Err()
				}
			})
			done <- err
		}()
	}
	<-started
	<-started
	cmd := cmdTasks(m, "cancel "+first.ID)
	if cmd == nil {
		t.Fatal("cancel command returned no confirmation")
	}
	msg, ok := cmd().(noticeMsg)
	if !ok || !strings.Contains(msg.text, "other delegate tasks keep running") {
		t.Fatalf("cancel notice = %#v", msg)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("target task error = %v", err)
	}
	byID := map[string]delegate.TaskSnapshot{}
	for _, task := range manager.List() {
		byID[task.ID] = task
	}
	if byID[first.ID].Status != delegate.TaskCanceled || byID[second.ID].Status != delegate.TaskRunning {
		t.Fatalf("statuses after targeted cancel = %+v", byID)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// The message is everything after the id, spaces and all. Parsing it by
// slicing offsets is how a steer arrives truncated or with its first word
// eaten, so the shape is pinned here.
func TestSteerArgumentsKeepTheWholeMessage(t *testing.T) {
	for _, tc := range []struct {
		args    string
		wantID  string
		wantMsg string
	}{
		{"steer task-001 stop reading tests", "task-001", "stop reading tests"},
		{"steer task-001   look at cmd/sb, not internal  ", "task-001", "look at cmd/sb, not internal"},
		{"steer task-002 one", "task-002", "one"},
	} {
		id, msg, ok := parseSteerArgs(tc.args)
		if !ok {
			t.Fatalf("%q did not parse", tc.args)
		}
		if id != tc.wantID || msg != tc.wantMsg {
			t.Errorf("%q parsed as id=%q msg=%q, want id=%q msg=%q", tc.args, id, msg, tc.wantID, tc.wantMsg)
		}
	}
	for _, bad := range []string{"steer", "steer task-001", "steer task-001   "} {
		if _, _, ok := parseSteerArgs(bad); ok {
			t.Errorf("%q should not parse: a steer with no message is not a steer", bad)
		}
	}
}
