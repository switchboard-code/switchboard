package schedule

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenRefusesOversizeLedger(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxLedgerBytes + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if store, err := Open(dir); err == nil || store != nil {
		t.Fatalf("oversize ledger = %+v, %v", store, err)
	}
}

func TestAddRefusesLedgerThatWouldExceedReadBound(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.Add(Entry{Every: time.Hour, Prompt: strings.Repeat("x", maxLedgerBytes)})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize Add error = %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(dir, FileName)); !os.IsNotExist(statErr) {
		t.Fatalf("oversize Add published a ledger: %v", statErr)
	}
	if _, statErr := os.Lstat(filepath.Join(dir, schedulePublicationJournalName)); !os.IsNotExist(statErr) {
		t.Fatalf("oversize Add created publication state before enforcing its cap: %v", statErr)
	}
}

func TestAddRefusesAChangedAbsentPreimage(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	external := []byte("[]")
	if err := os.WriteFile(filepath.Join(dir, FileName), external, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add(Entry{Every: time.Hour, Prompt: "must not clobber"}); err == nil {
		t.Fatal("Add accepted a ledger created after Open")
	}
	got, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(external) {
		t.Fatalf("external ledger was overwritten: %q", got)
	}
}

func TestPublishedScheduleErrorKeepsCommittedInMemoryImage(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	injected := errors.New("injected post-publication failure")
	schedulePublicationAfterTestHook = func(published bool, _ error) error {
		if !published {
			t.Fatal("post-publication hook observed an unpublished result")
		}
		return injected
	}
	t.Cleanup(func() { schedulePublicationAfterTestHook = nil })
	entry, err := store.Add(Entry{Every: time.Hour, Prompt: "committed"})
	schedulePublicationAfterTestHook = nil
	if !errors.Is(err, injected) || entry.ID == "" {
		t.Fatalf("Add() = %+v, %v; want committed entry and injected error", entry, err)
	}
	listed := store.List()
	if len(listed) != 1 || listed[0].ID != entry.ID {
		t.Fatalf("published entry was rolled back in memory: %+v", listed)
	}
	store.Close()
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	listed = reopened.List()
	if len(listed) != 1 || listed[0].ID != entry.ID {
		t.Fatalf("published entry was not durable: %+v", listed)
	}
}

func TestScheduleRecoveryIgnoresOtherComponentJournalNamespace(t *testing.T) {
	dir := t.TempDir()
	foreign := filepath.Join(dir, ".switchboard-restore-0123456789abcdef0123456789abcdef")
	if err := os.WriteFile(foreign, []byte("not a schedule transaction"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("foreign component cleanup record blocked schedule open: %v", err)
	}
	store.Close()
	got, err := os.ReadFile(foreign)
	if err != nil || string(got) != "not a schedule transaction" {
		t.Fatalf("schedule recovery touched a foreign component record: %q, %v", got, err)
	}
}
