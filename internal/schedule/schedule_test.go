package schedule

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, dir
}

// The ledger survives a reopen with every field intact, and a workspace that
// never armed one simply has no file.
func TestRoundtrip(t *testing.T) {
	s, dir := openTemp(t)
	every, err := s.Add(Entry{Every: 30 * time.Minute, Prompt: "run the tests"})
	if err != nil {
		t.Fatal(err)
	}
	at, err := s.Add(Entry{At: "14:30", Prompt: "check the deploy"})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got := reopened.List()
	if len(got) != 2 {
		t.Fatalf("reopened ledger has %d entries, want 2", len(got))
	}
	byID := map[string]Entry{}
	for _, e := range got {
		byID[e.ID] = e
	}
	if byID[every.ID].Every != 30*time.Minute || byID[every.ID].Prompt != "run the tests" ||
		!byID[every.ID].NextFire.Equal(every.NextFire) {
		t.Errorf("recurring entry did not round-trip: %+v", byID[every.ID])
	}
	if byID[at.ID].At != "14:30" || byID[at.ID].Prompt != "check the deploy" ||
		!byID[at.ID].NextFire.Equal(at.NextFire) {
		t.Errorf("one-shot entry did not round-trip: %+v", byID[at.ID])
	}

	empty, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("a missing file is an empty ledger, not an error: %v", err)
	}
	defer empty.Close()
	if len(empty.List()) != 0 {
		t.Fatal("a missing file produced entries")
	}
}

// Ids are the lowest free short number, and a cancelled id is reused, so the
// listing stays typeable.
func TestIDsAreLowestFree(t *testing.T) {
	s, _ := openTemp(t)
	first, _ := s.Add(Entry{Every: time.Hour, Prompt: "one"})
	second, _ := s.Add(Entry{Every: time.Hour, Prompt: "two"})
	if first.ID != "s1" || second.ID != "s2" {
		t.Fatalf("ids = %s, %s; want s1, s2", first.ID, second.ID)
	}
	if !s.Cancel(first.ID) {
		t.Fatal("cancel of an existing id reported not found")
	}
	third, _ := s.Add(Entry{Every: time.Hour, Prompt: "three"})
	if third.ID != "s1" {
		t.Fatalf("the freed id was not reused: got %s", third.ID)
	}
	if s.Cancel("s9") {
		t.Fatal("cancel of an unknown id reported success")
	}
}

// TakeDue returns what is due and moves the ledger past it in the same step:
// the recurring entry's next fire is measured from now, the one-shot is gone,
// and the not-yet-due entry is untouched — on disk, not just in memory. The
// file is written directly so the fire times do not depend on the wall clock
// the test happens to run at.
func TestTakeDueAdvancesAndRemoves(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	entries := []Entry{
		{ID: "s1", Every: 2 * time.Hour, Prompt: "rec", Created: now, NextFire: now.Add(-time.Minute)},
		{ID: "s2", At: "08:15", Prompt: "once", Created: now, NextFire: now.Add(-time.Minute)},
		{ID: "s3", Every: 24 * time.Hour, Prompt: "later", Created: now, NextFire: now.Add(24 * time.Hour)},
	}
	data, _ := json.Marshal(entries)
	if err := os.WriteFile(filepath.Join(dir, FileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	due, err := s.TakeDue(now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 {
		t.Fatalf("due = %d entries, want the recurring and the one-shot", len(due))
	}
	s.Close()

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got := reopened.List()
	if len(got) != 2 {
		t.Fatalf("after TakeDue the ledger holds %d entries, want 2 (one-shot removed)", len(got))
	}
	for _, e := range got {
		switch e.ID {
		case "s1":
			want := now.Add(2 * time.Hour)
			if e.NextFire.Sub(want).Abs() > time.Second {
				t.Errorf("recurring next fire = %s, want now+Every ≈ %s", e.NextFire, want)
			}
		case "s3":
			want := now.Add(24 * time.Hour)
			if e.NextFire.Sub(want).Abs() > time.Second {
				t.Errorf("the not-due entry moved to %s, want ≈ %s", e.NextFire, want)
			}
		default:
			t.Errorf("the one-shot s2 survived TakeDue")
		}
	}
}

// An entry whose ticks all passed while sb was down fires once, never once
// per missed tick, and reschedules from now.
func TestOverdueRecurringFiresOnceWithoutCatchingUp(t *testing.T) {
	dir := t.TempDir()
	past := time.Now().Add(-5 * time.Hour)
	entries := []Entry{{
		ID: "s1", Every: time.Hour, Prompt: "standup",
		Created: past, NextFire: past, // five ticks overdue
	}}
	data, _ := json.Marshal(entries)
	if err := os.WriteFile(filepath.Join(dir, FileName), data, 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	due, err := s.TakeDue(now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Prompt != "standup" {
		t.Fatalf("due = %+v, want the one overdue entry exactly once", due)
	}
	got := s.List()
	if len(got) != 1 {
		t.Fatalf("ledger holds %d entries, want the recurring one kept", len(got))
	}
	want := now.Add(time.Hour)
	if got[0].NextFire.Sub(want).Abs() > time.Second {
		t.Fatalf("rescheduled to %s, want now+Every ≈ %s — no catch-up ticks", got[0].NextFire, want)
	}
}

// A cap on one drain lets a surface fire one entry per tick and keep the
// rest armed: the second- and third-due entries wait for their own tick.
func TestTakeDueLimitKeepsTheRestArmed(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	entries := []Entry{
		{ID: "s1", At: "08:15", Prompt: "one", Created: now, NextFire: now.Add(-time.Minute)},
		{ID: "s2", At: "08:16", Prompt: "two", Created: now, NextFire: now.Add(-time.Minute)},
		{ID: "s3", At: "08:17", Prompt: "three", Created: now, NextFire: now.Add(-time.Minute)},
	}
	data, _ := json.Marshal(entries)
	if err := os.WriteFile(filepath.Join(dir, FileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	due, err := s.TakeDue(now, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Prompt != "one" {
		t.Fatalf("due = %+v, want only the soonest", due)
	}
	if got := s.List(); len(got) != 2 {
		t.Fatalf("the ledger holds %d entries after a limited drain, want 2", len(got))
	}
}

// One process owns the ledger at a time, because two pollers on one file
// would each fire the same entry. The second opener is told why, and the
// first owner's Close hands it over.
func TestSecondOpenIsLocked(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open err = %v, want ErrLocked", err)
	}
	s.Close()
	again, err := Open(dir)
	if err != nil {
		t.Fatalf("Open after the owner's Close: %v", err)
	}
	again.Close()
}

// The cap and the interval floor refuse at the ledger, not only in the
// command surface, because the file can be a hand edit away from either.
func TestAddRefusals(t *testing.T) {
	s, _ := openTemp(t)
	if _, err := s.Add(Entry{Every: 30 * time.Second, Prompt: "too tight"}); err == nil ||
		!strings.Contains(err.Error(), "shortest interval") {
		t.Errorf("an interval under the floor was not refused: %v", err)
	}
	if _, err := s.Add(Entry{Every: time.Hour, At: "14:30", Prompt: "both"}); err == nil {
		t.Error("an entry that is both kinds was not refused")
	}
	if _, err := s.Add(Entry{Prompt: "neither"}); err == nil {
		t.Error("an entry that is neither kind was not refused")
	}
	if _, err := s.Add(Entry{Every: time.Hour}); err == nil {
		t.Error("a promptless entry was not refused")
	}
	if _, err := s.Add(Entry{At: "tea time", Prompt: "bad clock"}); err == nil {
		t.Error("an unparseable clock was not refused")
	}

	for i := 0; i < MaxEntries; i++ {
		if _, err := s.Add(Entry{Every: time.Hour, Prompt: "filler"}); err != nil {
			t.Fatalf("entry %d under the cap refused: %v", i, err)
		}
	}
	if _, err := s.Add(Entry{Every: time.Hour, Prompt: "one too many"}); err == nil ||
		!strings.Contains(err.Error(), "32") {
		t.Errorf("the 33rd entry was not refused with the cap named: %v", err)
	}
}

// A corrupt file is an error and is left alone: a parse failure must never
// delete reminders.
func TestCorruptFileErrorsAndSurvives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	bad := []byte("{ not json, despite the braces")
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("a corrupt ledger opened without an error")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(bad) {
		t.Fatal("the corrupt file was rewritten")
	}
}

// The wall clock is the promise: a time still ahead today lands today, one
// that passed lands tomorrow, and "now" itself is tomorrow.
func TestNextAtTodayOrTomorrow(t *testing.T) {
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.Local)
	later, err := nextAt(now, "14:30")
	if err != nil {
		t.Fatal(err)
	}
	if later.Day() != 22 || later.Hour() != 14 || later.Minute() != 30 {
		t.Errorf("14:30 typed at 10:00 landed on %s, want today 14:30", later)
	}
	earlier, err := nextAt(now, "09:00")
	if err != nil {
		t.Fatal(err)
	}
	if earlier.Day() != 23 || earlier.Hour() != 9 {
		t.Errorf("09:00 typed at 10:00 landed on %s, want tomorrow 09:00", earlier)
	}
	edge, err := nextAt(now, "10:00")
	if err != nil {
		t.Fatal(err)
	}
	if edge.Day() != 23 {
		t.Errorf("a clock reading of right now landed on %s, want tomorrow", edge)
	}
}
