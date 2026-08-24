package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/schedule"
)

// installSchedules opens a ledger on the model's per-workspace directory.
// Entries needing controlled fire times are seeded through the file, so the
// test does not depend on the wall clock it runs at.
func installSchedules(t *testing.T, m *tuiModel, seeded ...schedule.Entry) *schedule.Store {
	t.Helper()
	dir, err := m.app.store.WorkspaceDir(m.app.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(seeded) > 0 {
		data, err := json.Marshal(seeded)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, schedule.FileName), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := schedule.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	m.app.schedules = s
	return s
}

func flatText(t *testing.T, m *tuiModel) string {
	t.Helper()
	return strings.Join(m.tr.flat, "\n")
}

// The two arming commands land entries in the ledger with the shapes the
// prompts described, and the confirmation says what was armed.
func TestEveryAndAtArmEntries(t *testing.T) {
	m := testModel(t)
	s := installSchedules(t, m)

	if cmd := cmdEvery(m, "30m run the tests"); cmd != nil {
		m.Update(cmd())
	}
	if cmd := cmdAt(m, "14:30 check the deploy"); cmd != nil {
		m.Update(cmd())
	}

	entries := s.List()
	if len(entries) != 2 {
		t.Fatalf("ledger holds %d entries, want 2", len(entries))
	}
	byID := map[string]schedule.Entry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	every, ok := byID["s1"]
	if !ok || every.Every != 30*time.Minute || every.Prompt != "run the tests" || !every.Recurring() {
		t.Errorf("/every armed %+v", every)
	}
	if want := time.Now().Add(30 * time.Minute); every.NextFire.Sub(want).Abs() > time.Minute {
		t.Errorf("first fire = %s, want about %s", every.NextFire, want)
	}
	at, ok := byID["s2"]
	if !ok || at.At != "14:30" || at.Prompt != "check the deploy" || at.Recurring() {
		t.Errorf("/at armed %+v", at)
	}
	if at.NextFire.Hour() != 14 || at.NextFire.Minute() != 30 || !at.NextFire.After(time.Now()) {
		t.Errorf("one-shot fire = %s, want 14:30 local in the future", at.NextFire)
	}

	out := flatText(t, m)
	if !strings.Contains(out, "armed s1") || !strings.Contains(out, "armed s2") {
		t.Errorf("the transcript does not confirm the arms:\n%s", out)
	}
}

// /schedule lists what is armed with its kind and next fire, cancels by id,
// and names an id it does not know.
func TestScheduleListsAndCancels(t *testing.T) {
	m := testModel(t)
	s := installSchedules(t, m)
	cmdEvery(m, "30m run the tests")
	cmdAt(m, "14:30 check the deploy")

	cmdSchedule(m, "")
	out := flatText(t, m)
	for _, want := range []string{"s1", "every 30m0s", "s2", "at 14:30", "run the tests", "check the deploy", "in "} {
		if !strings.Contains(out, want) {
			t.Errorf("listing is missing %q:\n%s", want, out)
		}
	}

	if cmd := cmdSchedule(m, "cancel s1"); cmd != nil {
		m.Update(cmd())
	}
	if entries := s.List(); len(entries) != 1 || entries[0].ID != "s2" {
		t.Fatalf("after cancel the ledger holds %+v, want only s2", entries)
	}
	if cmd := cmdSchedule(m, "cancel s9"); cmd != nil {
		m.Update(cmd())
	}
	if out := flatText(t, m); !strings.Contains(out, "cancelled s1") || !strings.Contains(out, "no scheduled entry s9") {
		t.Errorf("cancel notices misread:\n%s", out)
	}

	if cmd := cmdSchedule(m, "bogus args here"); cmd != nil {
		m.Update(cmd())
	}
	if out := flatText(t, m); !strings.Contains(out, "usage: /schedule [cancel <id>]") {
		t.Errorf("a malformed /schedule did not teach the usage:\n%s", out)
	}
}

// The surface refuses what the ledger would: no spec, no prompt, a bad
// interval, a bad clock, an interval under the floor — each with its usage.
func TestScheduleCommandRefusals(t *testing.T) {
	m := testModel(t)
	s := installSchedules(t, m)

	for _, run := range []func(*tuiModel, string) tea.Cmd{cmdEvery, cmdAt} {
		if cmd := run(m, ""); cmd != nil {
			m.Update(cmd())
		}
		if cmd := run(m, "30m"); cmd != nil { // spec without a prompt
			m.Update(cmd())
		}
	}
	if cmd := cmdEvery(m, "soonish run things"); cmd != nil {
		m.Update(cmd())
	}
	if cmd := cmdEvery(m, "30s poll the queue"); cmd != nil {
		m.Update(cmd())
	}
	if cmd := cmdAt(m, "noon eat"); cmd != nil {
		m.Update(cmd())
	}

	out := flatText(t, m)
	for _, want := range []string{"usage: /every <interval> <prompt>", "usage: /at <HH:MM> <prompt>",
		"takes an interval", "shortest interval", "24-hour local clock time"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusals are missing %q:\n%s", want, out)
		}
	}
	if len(s.List()) != 0 {
		t.Errorf("a refused command still armed: %+v", s.List())
	}
}

// Without a ledger the commands degrade to a reason, never a crash.
func TestScheduleCommandsSayWhenUnavailable(t *testing.T) {
	m := testModel(t)
	m.app.schedulesErr = ": test ledger failure"
	for _, cmd := range []tea.Cmd{cmdEvery(m, "30m x"), cmdAt(m, "14:30 x"), cmdSchedule(m, "")} {
		if cmd != nil {
			m.Update(cmd())
		}
	}
	out := flatText(t, m)
	if got := strings.Count(out, "schedules are unavailable: test ledger failure"); got != 3 {
		t.Errorf("unavailable reason appeared %d times, want 3:\n%s", got, out)
	}
}

// A due entry fires as an ordinary turn opening with the [scheduled sN] lead,
// and the one-shot leaves the ledger in the same step.
func TestFireScheduledOpensATurn(t *testing.T) {
	m := testModel(t)
	past := time.Now().Add(-time.Minute)
	s := installSchedules(t, m, schedule.Entry{ID: "s1", At: "08:00", Prompt: "standup notes", Created: past, NextFire: past})

	cmd := m.fireScheduled()
	if cmd == nil {
		t.Fatal("the poller did not re-arm its clock")
	}
	if !strings.Contains(flatText(t, m), "[scheduled s1] standup notes") {
		t.Errorf("the fired prompt is not in the transcript as a user turn:\n%s", flatText(t, m))
	}
	if !m.busy {
		t.Error("the fired prompt did not open a turn")
	}
	if len(s.List()) != 0 {
		t.Errorf("the fired one-shot stayed in the ledger: %+v", s.List())
	}
}

// A fire that finds a turn in flight joins the queue behind it, prefix and
// all, rather than interrupting.
func TestFireScheduledQueuesBehindARunningTurn(t *testing.T) {
	m := testModel(t)
	m.busy = true
	past := time.Now().Add(-time.Minute)
	installSchedules(t, m, schedule.Entry{ID: "s1", At: "08:00", Prompt: "standup notes", Created: past, NextFire: past})

	m.fireScheduled()
	if len(m.queue) != 1 || m.queue[0] != "[scheduled s1] standup notes" {
		t.Fatalf("queue = %v, want the prefixed prompt waiting", m.queue)
	}
	if !strings.Contains(flatText(t, m), "queued") {
		t.Errorf("the transcript does not say the fire queued:\n%s", flatText(t, m))
	}
}

// A fire that finds a dialog open waits for the next tick: the entry is not
// taken, so nothing fires underneath a prompt the user is still answering.
func TestFireScheduledDefersToAnOpenDialog(t *testing.T) {
	m := testModel(t)
	past := time.Now().Add(-time.Minute)
	s := installSchedules(t, m, schedule.Entry{ID: "s1", At: "08:00", Prompt: "standup notes", Created: past, NextFire: past})
	m.dlg = &pickerDialog{title: "resume a session"}

	cmd := m.fireScheduled()
	if cmd == nil {
		t.Fatal("the poller did not re-arm its clock")
	}
	if len(s.List()) != 1 {
		t.Errorf("the entry was taken while a dialog owned the input: %+v", s.List())
	}
	if m.busy || len(m.queue) != 0 {
		t.Errorf("a turn began under an open dialog: busy=%v queue=%v", m.busy, m.queue)
	}
}

// Arming a key-shaped prompt opens the same gate a typed prompt meets, and
// nothing lands in the ledger before the user answers; redact arms the
// redacted copy.
func TestArmScheduleHoldsAKeyBehindTheGate(t *testing.T) {
	m := testModel(t)
	s := installSchedules(t, m)

	if cmd := cmdEvery(m, "30m deploy with "+testGitHubToken); cmd != nil {
		t.Error("a gated arm returned a command")
	}
	if m.dlg == nil {
		t.Fatal("a key-shaped prompt opened no gate")
	}
	if len(s.List()) != 0 {
		t.Fatal("the entry armed before the gate answered")
	}
	cmd := chooseRedact(m)
	if cmd != nil {
		m.Update(cmd())
	}
	entries := s.List()
	if len(entries) != 1 {
		t.Fatalf("after redact the ledger holds %d entries, want 1", len(entries))
	}
	if strings.Contains(entries[0].Prompt, testGitHubToken) ||
		!strings.Contains(entries[0].Prompt, "[redacted: a GitHub token]") {
		t.Errorf("the armed prompt carries the key or hides the redaction: %q", entries[0].Prompt)
	}
}

// The listing row carries both clocks: the relative gap and, when the day is
// not today, the date.
func TestScheduleLineRendersKindAndFire(t *testing.T) {
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.Local)
	every := schedule.Entry{ID: "s2", Every: 30 * time.Minute, Prompt: "run the tests", NextFire: now.Add(12 * time.Minute)}
	line := scheduleLine(every, now)
	for _, want := range []string{"s2", "every 30m0s", "in 12m", "(10:12)", "run the tests"} {
		if !strings.Contains(line, want) {
			t.Errorf("recurring row missing %q: %q", want, line)
		}
	}
	at := schedule.Entry{ID: "s1", At: "14:30", Prompt: "check", NextFire: now.AddDate(0, 0, 1).Add(4*time.Hour + 30*time.Minute)}
	line = scheduleLine(at, now)
	for _, want := range []string{"at 14:30", "Aug 23 14:30"} {
		if !strings.Contains(line, want) {
			t.Errorf("one-shot row missing %q: %q", want, line)
		}
	}
	if strings.Contains(scheduleLine(every, now), "Aug 22") {
		t.Errorf("a same-day fire named its day: %q", scheduleLine(every, now))
	}
}

func TestScheduleLineRedactsCredentialBeforeTruncatingLegacyPrompt(t *testing.T) {
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.Local)
	entry := schedule.Entry{
		ID: "s1", At: "10:30", NextFire: now.Add(30 * time.Minute),
		Prompt: strings.Repeat("x", 38) + testGitHubToken,
	}
	line := scheduleLine(entry, now)
	if strings.Contains(line, "ghp_") || strings.Contains(line, testGitHubToken) {
		t.Fatalf("schedule row exposed a credential fragment: %q", line)
	}
}
