//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
)

func TestBisectVerifierWithholdsTruncatedOutput(t *testing.T) {
	token := "ghp_" + strings.Repeat("e", 36)
	command := "printf '%40000s' x; printf '%s' " + shellSingleQuote(token) + "; exit 1"
	verdict := runVerifier(context.Background(), command, t.TempDir())
	if verdict.Passed || verdict.Err != nil {
		t.Fatalf("truncated failing verifier returned %#v", verdict)
	}
	if strings.Contains(verdict.FirstFail, token) || strings.Contains(verdict.FirstFail, "ghp_") ||
		!strings.Contains(verdict.FirstFail, "withheld") {
		t.Fatalf("truncated verifier exposed output: %q", verdict.FirstFail)
	}
}

func TestAbnormalTUIExitWaitsForBisectWorkspaceRestore(t *testing.T) {
	m := testModel(t)
	m.app.lifetime = newTUILifetime()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.app.workspace = dir
	path := filepath.Join(dir, "state.txt")
	if err := os.WriteFile(path, []byte("fine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := checkpoint.NewRecorder()
	recorder.Begin("break the state")
	recorder.Record(path)
	if err := os.WriteFile(path, []byte("broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.app.undo = recorder

	started := filepath.Join(dir, "past-verifier-started")
	command := "if grep -q broken " + shellSingleQuote(path) + "; then echo '--- FAIL: current'; exit 1; fi; " +
		": > " + shellSingleQuote(started) + "; while :; do sleep 0.01; done"
	if cmd := cmdBisect(m, command); cmd == nil {
		t.Fatal("bisect did not start")
	}
	waitForWatchPath(t, started)
	if got, err := os.ReadFile(path); err != nil || string(got) != "fine\n" {
		t.Fatalf("bisect did not reach its historical probe: %q, %v", got, err)
	}

	terminalErr := errors.New("terminal disconnected")
	if err := runTUIProgram(tuiProgramFunc(func() (tea.Model, error) { return m, terminalErr }), m); !errors.Is(err, terminalErr) {
		t.Fatalf("runTUIProgram error = %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "broken\n" {
		t.Fatalf("TUI exit returned before bisect restored the checkout: %q, %v", got, err)
	}
	if m.bisect != nil || m.busy {
		t.Fatalf("bisect teardown retained ownership: run=%v busy=%v", m.bisect != nil, m.busy)
	}
	select {
	case <-m.app.lifetime.Done():
	case <-time.After(time.Second):
		t.Fatal("TUI lifetime stayed open after abnormal exit")
	}
}
