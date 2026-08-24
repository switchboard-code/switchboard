package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A steer drains into the turn at the round boundary, marked so a log reader
// can tell it from a turn's opening, and it drains once.
func TestSteerRoundDrainsMarkedAndOnce(t *testing.T) {
	m := testModel(t)
	m.app.queueSteer("fix the test first")
	m.app.queueSteer("then run the suite")

	out := m.app.inject()
	if len(out) != 2 {
		t.Fatalf("inject returned %d messages, want 2", len(out))
	}
	for i, msg := range out {
		if !msg.Injected {
			t.Errorf("message %d is not marked injected", i)
		}
		if !msg.UserSteer {
			t.Errorf("message %d is not marked as a user-authored steer", i)
		}
	}
	if got := out[0].AuthoredText(); got != "[steer] fix the test first" {
		t.Errorf("first = %q, want the [steer] lead", got)
	}
	if !injectionShaped(out[0]) {
		t.Error("the [steer] lead was not recognized as injection-shaped")
	}
	if again := m.app.inject(); len(again) != 0 {
		t.Fatalf("a drained steer came back: %d messages", len(again))
	}
}

// ctrl+s with a turn in flight puts the composed words into the turn, not the
// queue behind it, and says so.
func TestSteerKeySteersARunningTurn(t *testing.T) {
	m := testModel(t)
	m.busy = true
	m.ta.SetValue("stop refactoring, fix the test")

	cmd := m.steerKey()
	if cmd != nil {
		t.Fatalf("steering returned a command: %v", cmd)
	}
	if m.ta.Value() != "" {
		t.Fatal("the composer kept a steered prompt")
	}
	if len(m.queue) != 0 {
		t.Fatalf("a steer joined the after-turn queue: %v", m.queue)
	}
	out := m.app.steerRound()
	if len(out) != 1 || out[0].AuthoredText() != "[steer] stop refactoring, fix the test" {
		t.Fatalf("pending steers = %+v", out)
	}
	flat := strings.Join(m.tr.flat, "\n")
	if !strings.Contains(flat, "stop refactoring, fix the test") || !strings.Contains(flat, "next round boundary") {
		t.Errorf("the transcript does not show what was steered:\n%s", flat)
	}
}

// With nothing running, ctrl+s is an ordinary send: steering nobody is just
// typing.
func TestSteerKeyAtRestSubmits(t *testing.T) {
	m := testModel(t)
	m.ta.SetValue("/queue")

	if cmd := m.steerKey(); cmd != nil {
		m.Update(cmd())
	}
	if m.ta.Value() != "" {
		t.Fatal("the composer kept a submitted line")
	}
	if !strings.Contains(strings.Join(m.tr.flat, "\n"), "queued") {
		t.Errorf("an idle ctrl+s did not behave like enter:\n%s", strings.Join(m.tr.flat, "\n"))
	}
}

// A steer the turn ended before draining is not dropped: it was typed first,
// so it leads the queue.
func TestSteerOutlivingItsTurnLeadsTheQueue(t *testing.T) {
	m := testModel(t)
	m.queue = []string{"queued later"}
	m.app.queueSteer("typed first")

	m.foldSteers()
	if len(m.queue) != 2 || m.queue[0] != "typed first" || m.queue[1] != "queued later" {
		t.Fatalf("queue after the fold = %v, want the steer first", m.queue)
	}
	if len(m.app.takeSteers()) != 0 {
		t.Fatal("the fold left a steer to be folded twice")
	}
}

// /steer with text and a turn running takes the same path as ctrl+s; bare, it
// teaches rather than erroring.
func TestSteerCommandMatchesTheKey(t *testing.T) {
	m := testModel(t)
	m.busy = true

	cmdSteer(m, "hold off on the rename")
	out := m.app.steerRound()
	if len(out) != 1 || out[0].AuthoredText() != "[steer] hold off on the rename" {
		t.Fatalf("pending steers = %+v", out)
	}

	if cmd := cmdSteer(m, ""); cmd == nil {
		t.Fatal("bare /steer said nothing")
	}
}

func TestSteerCommandQueuesDuringSessionOperation(t *testing.T) {
	m := testModel(t)
	_, generation, _, err := m.startOperation("compact")
	if err != nil {
		t.Fatal(err)
	}
	defer m.finishOperation(generation, false)

	if cmd := cmdSteer(m, "preserve this correction"); cmd != nil {
		t.Fatalf("operation-time /steer returned a command: %v", cmd)
	}
	if pending := m.app.takeSteers(); len(pending) != 0 {
		t.Fatalf("operation with no round boundary accepted steers: %v", pending)
	}
	if len(m.queue) != 1 || m.queue[0] != "preserve this correction" {
		t.Fatalf("operation-time /steer queue = %v", m.queue)
	}
	if flat := strings.Join(m.tr.flat, "\n"); !strings.Contains(flat, "current operation finishes") {
		t.Fatalf("operation-time /steer did not explain its disposition:\n%s", flat)
	}
}

func TestSteerKeepsExactAuthoredTextBesideMentionExpansion(t *testing.T) {
	m := testModel(t)
	path := filepath.Join(m.app.workspace, "notes.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("machine evidence, not a user scope change"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cmd := m.steer("inspect @notes.txt"); cmd != nil {
		t.Fatalf("plain steer returned a command: %v", cmd)
	}
	messages := m.app.steerRound()
	if len(messages) != 1 {
		t.Fatalf("steer messages = %d", len(messages))
	}
	message := messages[0]
	if got := message.AuthoredText(); got != "[steer] inspect @notes.txt" {
		t.Fatalf("authored steer = %q", got)
	}
	if got := message.Text(); !strings.Contains(got, "machine evidence, not a user scope change") {
		t.Fatalf("provider steer lost mention expansion: %q", got)
	}
}
