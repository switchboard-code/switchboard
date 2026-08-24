package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/bisect"
	"github.com/switchboard-code/switchboard/internal/checkpoint"
	route "github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/watch"
)

func TestBisectRefusesWithoutADeclaredVerifier(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	os.WriteFile(path, []byte("v0"), 0o644)
	m.app.undo.Begin("a turn")
	m.app.undo.Record(path)
	os.WriteFile(path, []byte("v1"), 0o644)

	if cmd := cmdBisect(m, ""); cmd == nil {
		t.Fatal("refusal said nothing")
	} else {
		msg := cmd().(noticeMsg)
		if !strings.Contains(msg.text, "/watch") {
			t.Errorf("the refusal does not name the way to declare one: %q", msg.text)
		}
	}
	if m.bisect != nil {
		t.Error("a refused bisect is running anyway")
	}
}

func TestBisectRefusesWithNothingRecorded(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	cmd := cmdBisect(m, "go test ./...")
	if cmd == nil {
		t.Fatal("refusal said nothing")
	}
	msg := cmd().(noticeMsg)
	if !strings.Contains(msg.text, "nothing to bisect") {
		t.Errorf("an empty record should say so: %q", msg.text)
	}
}

func TestBisectRefusesAPartialTurn(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	dir := t.TempDir()
	big := filepath.Join(dir, "big.bin")
	os.WriteFile(big, []byte(strings.Repeat("x", 4<<20+1)), 0o644)
	token := "ghp_" + strings.Repeat("e", 36)
	m.app.undo.Begin("bulk change " + token)
	m.app.undo.Record(big)

	cmd := cmdBisect(m, "go test ./...")
	if cmd == nil {
		t.Fatal("refusal said nothing")
	}
	msg := cmd().(noticeMsg)
	if !strings.Contains(msg.text, "snapshot cap") {
		t.Errorf("a partial turn must be refused by name: %q", msg.text)
	}
	if strings.Contains(msg.text, token) || !strings.Contains(msg.text, "[redacted") {
		t.Errorf("a partial-turn refusal rendered its credential-shaped label: %q", msg.text)
	}
}

func TestBisectDoneNamesTheCulpritAndClearsBusy(t *testing.T) {
	m := testModel(t)
	run := &bisectRun{
		command: "go test ./...",
		labels:  []string{"first", "add the cache header", "third"},
		cancel:  func() {},
		rail:    m.tr.add(&entry{kind: kindInfo, text: "bisect"}),
	}
	m.bisect = run
	m.busy = true

	m.onBisectDone(bisectDoneMsg{res: bisect.Result{
		Outcome: bisect.Found,
		Culprit: 1,
		Fail:    bisect.Verdict{FirstFail: "--- FAIL: TestCache"},
		Probes:  4,
	}})

	if m.busy || m.bisect != nil {
		t.Error("a finished bisect left the session busy")
	}
	view := strings.Join(m.tr.flat, "\n")
	for _, want := range []string{"turn 2 of 3", "add the cache header", "--- FAIL: TestCache", "write and edit captured"} {
		if !strings.Contains(view, want) {
			t.Errorf("the report is missing %q:\n%s", want, view)
		}
	}
}

func TestBisectDoneOnCancellationSaysRestored(t *testing.T) {
	m := testModel(t)
	run := &bisectRun{
		command:   "go test ./...",
		labels:    []string{"only"},
		cancel:    func() {},
		cancelled: true,
		rail:      m.tr.add(&entry{kind: kindInfo, text: "bisect"}),
	}
	m.bisect = run
	m.busy = true

	m.onBisectDone(bisectDoneMsg{})
	if m.busy {
		t.Error("cancellation left the session busy")
	}
	if !strings.Contains(strings.Join(m.tr.flat, "\n"), "workspace is restored") {
		t.Errorf("a cancelled bisect must say the tree is back:\n%s", strings.Join(m.tr.flat, "\n"))
	}
}

// The moment a turn-end watch verdict goes red is the moment "which turn
// broke it" becomes askable, so /bisect is named there — once.
func TestRedWatchVerdictNamesBisectOnce(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	dir := t.TempDir()
	for i, name := range []string{"a.go", "b.go"} {
		path := filepath.Join(dir, name)
		os.WriteFile(path, []byte("v0"), 0o644)
		m.app.undo.Begin([]string{"first", "second"}[i])
		m.app.undo.Record(path)
		os.WriteFile(path, []byte("v1"), 0o644)
	}

	report := bindWatchReport(t, m, watchReportMsg{command: "go test ./...", turnEnd: true, rep: watch.Report{
		New:        []route.Failure{{Line: "--- FAIL: TestX", Signature: "sig"}},
		Signatures: []string{"sig"},
	}})
	m.onWatchReport(report)
	joined := strings.Join(m.tr.flat, "\n")
	if !strings.Contains(joined, "/bisect can name the turn") {
		t.Errorf("the red verdict did not teach /bisect:\n%s", joined)
	}

	m.onWatchReport(report)
	if strings.Count(strings.Join(m.tr.flat, "\n"), "/bisect can name the turn") != 1 {
		t.Error("the lesson repeated; once is the contract")
	}
}

// The verdict folds behind the next typed prompt, so "fix it" after a
// bisect carries what the machine measured — and the fold redacts what
// the credential gate would hold, because verifier output is exactly the
// surface an env dump leaks a key through.
func TestBisectVerdictFoldsIntoTheNextPrompt(t *testing.T) {
	m := testModel(t)
	run := &bisectRun{
		command: "go test ./...",
		labels:  []string{"first", "break things"},
		cancel:  func() {},
		rail:    m.tr.add(&entry{kind: kindInfo, text: "bisect"}),
	}
	m.bisect = run
	m.busy = true
	m.onBisectDone(bisectDoneMsg{res: bisect.Result{
		Outcome: bisect.Found,
		Culprit: 1,
		Fail:    bisect.Verdict{FirstFail: "--- FAIL: TestX  sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcd-suffixAA"},
	}})

	folded := m.watchContext("fix it")
	if !strings.Contains(folded, "[bisect]") || !strings.Contains(folded, `"break things"`) {
		t.Errorf("the verdict did not fold behind the prompt:\n%s", folded)
	}
	if strings.Contains(folded, "sk-ant-api03-abcdefghijklmnopqrstuvwxyz") {
		t.Errorf("a key-shaped string in verifier output rode the fold unredacted:\n%s", folded)
	}
}

func TestBisectVerdictRedactsBeforeTheFailureCap(t *testing.T) {
	token := "sk-ant-api03-" + "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefghijklmnop"
	text := bisectInjectText("go test", "break things", bisect.Result{
		Outcome: bisect.Found,
		Culprit: 1,
		Fail:    bisect.Verdict{FirstFail: strings.Repeat("x", 179) + " " + token},
	})
	if strings.Contains(text, "sk-ant-api03-") || !strings.Contains(text, "[redacted") {
		t.Fatalf("a boundary-straddling key fragment survived the bisect cap:\n%s", text)
	}
}

// Whatever else ended the run, a restore failure must never be softened
// into "the workspace is restored" — that is the lie the contract exists
// to prevent.
func TestBisectDoneRefusesToClaimRestoredWhenItWasNot(t *testing.T) {
	m := testModel(t)
	run := &bisectRun{
		command:   "go test ./...",
		labels:    []string{"only"},
		cancel:    func() {},
		cancelled: true,
		rail:      m.tr.add(&entry{kind: kindInfo, text: "bisect"}),
	}
	m.bisect = run
	m.busy = true

	m.onBisectDone(bisectDoneMsg{err: errors.Join(context.Canceled, &bisect.RestoreError{Err: os.ErrPermission})})

	joined := strings.Join(m.tr.flat, "\n")
	if strings.Contains(joined, "workspace is restored") {
		t.Errorf("a failed restore was reported as restored:\n%s", joined)
	}
	if !strings.Contains(joined, "not fully restored") {
		t.Errorf("the restore failure is not named:\n%s", joined)
	}
}

// The warning is worthless if a queued prompt fires against the
// unrestored tree in the same breath: a restore failure drops the queue
// with its count instead of draining it.
func TestBisectRestoreFailureDropsTheQueue(t *testing.T) {
	m := testModel(t)
	run := &bisectRun{
		command: "go test ./...",
		labels:  []string{"only"},
		cancel:  func() {},
		rail:    m.tr.add(&entry{kind: kindInfo, text: "bisect"}),
	}
	m.bisect = run
	m.busy = true
	m.queue = []string{"fix the parser", "run the suite"}

	cmd := m.onBisectDone(bisectDoneMsg{err: &bisect.RestoreError{Err: os.ErrPermission}})
	if cmd != nil {
		t.Fatal("a turn started against a possibly-unrestored tree")
	}
	if len(m.queue) != 0 {
		t.Errorf("the queue survived to fire later: %v", m.queue)
	}
	joined := strings.Join(m.tr.flat, "\n")
	if !strings.Contains(joined, "2 queued prompts dropped") {
		t.Errorf("the drop is not named with its count:\n%s", joined)
	}
}
