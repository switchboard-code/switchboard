//go:build darwin || linux || windows

package checkpoint

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
)

func TestDurableUndoPrepareAbortAndCommitReleaseProcessResources(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	journalDir := filepath.Join(root, "sessions")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(journalDir, "child.log")
	target := filepath.Join(workspace, "target.txt")
	write(t, child, "staged child")

	// Warm lazy platform state and both cleanup paths before measuring. Disabling
	// GC after the warm-up keeps an accidentally abandoned *os.File finalizer
	// from hiding a descriptor or HANDLE leak during the measured operations.
	durableUndoResourceCycle(t, workspace, journalDir, child, target, 0, false)
	durableUndoResourceCycle(t, workspace, journalDir, child, target, 1, true)
	if _, err := checkpointProcessResourceCount(); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	previousGCPercent := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(previousGCPercent)

	before, err := checkpointProcessResourceCount()
	if err != nil {
		t.Fatal(err)
	}
	const cycles = 32
	for i := range cycles {
		durableUndoResourceCycle(t, workspace, journalDir, child, target, i+2, i%2 == 1)
	}
	after, err := checkpointProcessResourceCount()
	if err != nil {
		t.Fatal(err)
	}
	if growth := after - before; growth > 4 {
		t.Fatalf("%d durable prepare/abort-or-commit cycles grew process resources by %d (before %d, after %d)", cycles, growth, before, after)
	}

	// On Windows this also proves the native rename/delete operations released
	// every child handle; a leaked non-delete-sharing handle pins the directory.
	movedJournalDir := journalDir + ".moved"
	if err := os.Rename(journalDir, movedJournalDir); err != nil {
		t.Fatalf("renaming journal directory after repeated cleanup: %v", err)
	}
	if err := os.Rename(movedJournalDir, journalDir); err != nil {
		t.Fatalf("restoring renamed journal directory: %v", err)
	}
}

func durableUndoResourceCycle(t *testing.T, workspace, journalDir, child, target string, sequence int, commit bool) {
	t.Helper()
	write(t, target, "before")
	recorder := NewRecorder()
	identity := TurnIdentity{SessionID: "resource-release", OpeningMessage: sequence}
	recorder.BeginTurn(identity.SessionID, identity.OpeningMessage, "retry resource release")
	recorder.Record(target)
	write(t, target, "after")
	prepared, err := recorder.PrepareDurableUndoCurrent(identity, journalDir, workspace, child, "test-child-identity")
	if err != nil {
		t.Fatal(err)
	}
	if commit {
		result, err := prepared.ApplyAndCommit(func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if result.CleanupWarning != nil {
			t.Fatalf("committed durable cleanup warning: %v", result.CleanupWarning)
		}
		if got := readBack(t, target); got != "before" {
			t.Fatalf("committed restore content = %q, want before", got)
		}
	} else {
		if err := prepared.AbortDurable(); err != nil {
			t.Fatal(err)
		}
		if got := readBack(t, target); got != "after" {
			t.Fatalf("aborted restore content = %q, want after", got)
		}
	}
	if _, err := os.Lstat(filepath.Join(journalDir, durableUndoJournalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("durable journal remains after cleanup: %v", err)
	}
}
