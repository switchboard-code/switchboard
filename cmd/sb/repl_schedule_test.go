package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/schedule"
)

// The REPL commands arm, list, and cancel against the same ledger the TUI
// uses; the notice text is the surface's own.
func TestREPLScheduleCommands(t *testing.T) {
	r, _, readOutput := newOverrideREPL(t, "small")
	s, err := schedule.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	r.schedules = s

	r.command(context.Background(), "/every 30m run the tests")
	r.command(context.Background(), "/at 14:30 check the deploy")
	// The listing is sorted by next fire, so the order depends on the wall
	// clock the test runs at; assert the set instead.
	entries := s.List()
	if len(entries) != 2 {
		t.Fatalf("ledger holds %d entries, want 2", len(entries))
	}
	byID := map[string]schedule.Entry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	if e, ok := byID["s1"]; !ok || !e.Recurring() || e.Prompt != "run the tests" {
		t.Errorf("the recurring entry misshapen or missing: %+v", entries)
	}
	if e, ok := byID["s2"]; !ok || e.Recurring() || e.Prompt != "check the deploy" {
		t.Errorf("the one-shot entry misshapen or missing: %+v", entries)
	}

	r.command(context.Background(), "/schedule")
	out := readOutput()
	for _, want := range []string{"s1", "every 30m0s", "s2", "at 14:30", "run the tests", "check the deploy"} {
		if !strings.Contains(out, want) {
			t.Errorf("/schedule listing is missing %q:\n%s", want, out)
		}
	}

	r.command(context.Background(), "/schedule cancel s1")
	r.command(context.Background(), "/schedule cancel s9")
	out = readOutput()
	if !strings.Contains(out, "cancelled s1") || !strings.Contains(out, "no scheduled entry s9") {
		t.Errorf("cancel notices misread:\n%s", out)
	}
	if entries := s.List(); len(entries) != 1 || entries[0].ID != "s2" {
		t.Fatalf("after cancel the ledger holds %+v, want only s2", entries)
	}
}

// A due entry fires through the same path a typed prompt takes: the provider
// sees the [scheduled sN] lead, the echo shows where the turn came from, and
// the one-shot leaves the ledger.
func TestREPLFiresDueSchedulesAsTurns(t *testing.T) {
	r, capture, readOutput := newOverrideREPL(t, "small")
	dir := t.TempDir()
	past := time.Now().Add(-time.Minute)
	seeded := []schedule.Entry{{ID: "s1", At: "08:00", Prompt: "standup notes please", Created: past, NextFire: past}}
	data, _ := json.Marshal(seeded)
	if err := os.WriteFile(filepath.Join(dir, schedule.FileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := schedule.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	r.schedules = s

	r.fireDueSchedules(context.Background())

	if len(capture.bodies) != 1 || !strings.Contains(capture.bodies[0], "[scheduled s1] standup notes please") {
		t.Fatalf("provider requests = %#v, want one turn carrying the prefixed prompt", capture.bodies)
	}
	if len(s.List()) != 0 {
		t.Errorf("the fired one-shot stayed in the ledger: %+v", s.List())
	}
	if out := readOutput(); !strings.Contains(out, "[scheduled s1] standup notes please") {
		t.Errorf("the fired prompt never echoed:\n%s", out)
	}
}

// A key-shaped prompt is refused at arm time on this surface: the kinds are
// named, the value is not printed, and nothing lands in the ledger.
func TestREPLScheduleRefusesKeyShapedPrompt(t *testing.T) {
	r, _, readOutput := newOverrideREPL(t, "small")
	s, err := schedule.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	r.schedules = s

	r.command(context.Background(), "/every 30m deploy with "+testGitHubToken)
	out := readOutput()
	if !strings.Contains(out, "not scheduled") || !strings.Contains(out, "GitHub token") {
		t.Errorf("the refusal does not name the finding kind:\n%s", out)
	}
	if strings.Contains(out, testGitHubToken) {
		t.Errorf("the refusal quotes the secret:\n%s", out)
	}
	if len(s.List()) != 0 {
		t.Errorf("a refused prompt armed anyway: %+v", s.List())
	}
}

// Without a ledger the commands say why and do nothing.
func TestREPLScheduleCommandsSayWhenUnavailable(t *testing.T) {
	r, _, readOutput := newOverrideREPL(t, "small")
	r.schedulesErr = ": test ledger failure"
	r.command(context.Background(), "/schedule")
	r.command(context.Background(), "/every 30m run the tests")
	out := readOutput()
	if got := strings.Count(out, "schedules are unavailable: test ledger failure"); got != 2 {
		t.Errorf("unavailable reason appeared %d times, want 2:\n%s", got, out)
	}
}
