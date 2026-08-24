package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDoneKeysMultiDigitReplicatesWithoutCollision(t *testing.T) {
	runs := []Run{
		{Arm: "a", TaskID: "task", Seed: 1},
		{Arm: "a", TaskID: "task", Seed: 11},
	}
	done := Done(runs)
	if len(done) != 2 {
		t.Fatalf("done has %d keys, want 2", len(done))
	}
	for _, seed := range []int{1, 11} {
		if !done[attemptKey("a", "task", seed)] {
			t.Errorf("replicate %d is not marked done", seed)
		}
	}
}

func TestReadJournalRejectsMalformedCompleteRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	if err := os.WriteFile(path, []byte("{\"TaskID\":\"first\"}\nnot-json\n{\"TaskID\":\"third\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadJournal(path); err == nil {
		t.Fatal("a malformed complete record was silently treated as a truncated tail")
	}
}

func TestReadJournalRejectsSchemaInvalidFinalRecordWithoutNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	if err := os.WriteFile(path, []byte("{\"Seed\":\"not-an-integer\"}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadJournal(path); err == nil {
		t.Fatal("a schema-invalid final record was silently treated as a truncated tail")
	}
}

func TestJournalResumeRepairsTruncatedFinalRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	if err := os.WriteFile(path, []byte("{\"TaskID\":\"first\"}\n{\"TaskID\":\"partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	journal, err := NewJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Append(Run{TaskID: "resumed"}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	runs, err := ReadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].TaskID != "first" || runs[1].TaskID != "resumed" {
		t.Fatalf("runs = %#v, want the complete record and resumed record", runs)
	}
}

func TestJournalResumePreservesValidFinalRecordWithoutNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	if err := os.WriteFile(path, []byte("{\"TaskID\":\"first\"}"), 0o600); err != nil {
		t.Fatal(err)
	}

	journal, err := NewJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Append(Run{TaskID: "second"}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	runs, err := ReadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].TaskID != "first" || runs[1].TaskID != "second" {
		t.Fatalf("runs = %#v, want both complete records", runs)
	}
}

func TestJournalResumeAppendsWithoutReplacingSurvivingBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	prefix := []byte("{\"TaskID\":\"first\"}\n")
	if err := os.WriteFile(path, prefix, 0o600); err != nil {
		t.Fatal(err)
	}

	journal, err := NewJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Append(Run{TaskID: "second"}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) <= len(prefix) || string(got[:len(prefix)]) != string(prefix) {
		t.Fatalf("journal prefix changed: %q", got)
	}
	var second Run
	if err := json.Unmarshal(got[len(prefix):], &second); err != nil || second.TaskID != "second" {
		t.Fatalf("appended record = %#v, %v", second, err)
	}
}
