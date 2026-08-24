//go:build unix

package schedule

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOpenDoesNotBlockOnFIFOLedger(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(dir, FileName), 0o600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := Open(dir)
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("FIFO ledger was accepted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("schedule open blocked on FIFO ledger")
	}
}

func TestOpenDoesNotBlockOnDirectoryReplacementFIFO(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "schedule")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	moved := dir + "-moved"
	var swapErr error
	scheduleDirectoryBeforeBindTestHook = func(got string) {
		if got != dir {
			return
		}
		swapErr = os.Rename(dir, moved)
		if swapErr == nil {
			swapErr = unix.Mkfifo(dir, 0o600)
		}
	}
	t.Cleanup(func() { scheduleDirectoryBeforeBindTestHook = nil })

	result := make(chan error, 1)
	go func() {
		_, err := Open(dir)
		result <- err
	}()
	select {
	case err := <-result:
		if swapErr != nil {
			t.Fatal(swapErr)
		}
		if err == nil {
			t.Fatal("schedule open accepted a FIFO directory replacement")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("schedule open blocked on a FIFO directory replacement")
	}
}

func TestOpenDoesNotBlockOnLockReplacementFIFO(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, lockName)
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var swapErr error
	scheduleLockBeforeOpenTestHook = func(name string) {
		if name != lockName {
			return
		}
		swapErr = os.Remove(lockPath)
		if swapErr == nil {
			swapErr = unix.Mkfifo(lockPath, 0o600)
		}
	}
	t.Cleanup(func() { scheduleLockBeforeOpenTestHook = nil })

	result := make(chan error, 1)
	go func() {
		_, err := Open(dir)
		result <- err
	}()
	select {
	case err := <-result:
		if swapErr != nil {
			t.Fatal(swapErr)
		}
		if err == nil {
			t.Fatal("schedule open accepted a FIFO lock replacement")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("schedule open blocked on a FIFO lock replacement")
	}
}

func TestSaveRefusesRetargetedScheduleDirectory(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "schedule")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	moved := dir + "-moved"
	outside := filepath.Join(parent, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}
	_, err = store.Add(Entry{Every: time.Hour, Prompt: "do not redirect"})
	if err == nil || !strings.Contains(err.Error(), "directory changed") {
		t.Fatalf("retargeted Add error = %v", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("retargeted save mutated outside directory: %v", entries)
	}
}

func TestSaveRollsBackLateScheduleDirectoryRetarget(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "schedule")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	moved := dir + "-moved"
	outside := filepath.Join(parent, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(outside, FileName)
	if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	var swapErr error
	schedulePublicationBeforeTestHook = func() {
		schedulePublicationBeforeTestHook = nil
		swapErr = os.Rename(dir, moved)
		if swapErr == nil {
			swapErr = os.Symlink(outside, dir)
		}
	}
	t.Cleanup(func() { schedulePublicationBeforeTestHook = nil })
	if _, err := store.Add(Entry{Every: time.Hour, Prompt: "late retarget"}); err == nil {
		t.Fatal("late-retargeted schedule publication succeeded")
	}
	if swapErr != nil {
		t.Fatal(swapErr)
	}
	got, err := os.ReadFile(sentinel)
	if err != nil || string(got) != "outside" {
		t.Fatalf("outside target changed: %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(moved, FileName)); !os.IsNotExist(err) {
		t.Fatalf("rolled-back schedule ledger remains in moved directory: %v", err)
	}
}

func TestOpenDoesNotBlockOnSchedulePublicationJournalFIFO(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(dir, schedulePublicationJournalName), 0o600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := Open(dir)
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("FIFO publication journal was accepted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("schedule open blocked on a FIFO publication journal")
	}
}
