//go:build unix

package checkpoint

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const durableUndoCrashHelper = "SB_DURABLE_UNDO_CRASH_HELPER"

func TestDurableUndoSurvivesSIGKILLAtEveryCommitBoundary(t *testing.T) {
	for _, test := range []struct {
		boundary  string
		found     bool
		published bool
	}{
		{boundary: "journal-created"},
		{boundary: "journal-header"},
		{boundary: "journal-payload"},
		{boundary: "journal-file-sync"},
		{boundary: "journal-before-install"},
		{boundary: "journal-after-install", found: true},
		{boundary: "journal-reopened", found: true},
		{boundary: "before-restore", found: true},
		{boundary: "lease-reservation-before-sync", found: true},
		{boundary: "lease-reservation-after-sync", found: true},
		{boundary: "lease-binding-before-sync", found: true},
		{boundary: "lease-binding-after-sync", found: true},
		{boundary: "lease-displaced-before-sync", found: true},
		{boundary: "lease-displaced-after-sync", found: true},
		{boundary: "before-file-replace", found: true},
		{boundary: "after-restore-0", found: true},
		{boundary: "after-restore-1", found: true},
		{boundary: "before-publish", found: true},
		{boundary: "after-publish", found: true, published: true},
	} {
		t.Run(test.boundary, func(t *testing.T) {
			root := t.TempDir()
			ready := filepath.Join(root, "ready")
			command := exec.Command(os.Args[0], "-test.run=^TestDurableUndoCrashHelper$")
			command.Env = append(os.Environ(),
				durableUndoCrashHelper+"=1",
				"SB_DURABLE_UNDO_ROOT="+root,
				"SB_DURABLE_UNDO_BOUNDARY="+test.boundary,
			)
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				} else if !errors.Is(err, os.ErrNotExist) {
					_ = command.Process.Kill()
					_ = command.Wait()
					t.Fatal(err)
				}
				if time.Now().After(deadline) {
					_ = command.Process.Kill()
					_ = command.Wait()
					t.Fatalf("helper did not reach %s", test.boundary)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := command.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := command.Wait(); err == nil {
				t.Fatal("SIGKILL helper exited successfully")
			}

			workspace := filepath.Join(root, "workspace")
			journalDir := filepath.Join(root, "sessions")
			publishedPath := filepath.Join(root, "published")
			recovery, err := RecoverDurableUndo(journalDir, workspace, func(string, string) (bool, error) {
				_, err := os.Stat(publishedPath)
				if err == nil {
					return true, nil
				}
				if errors.Is(err, os.ErrNotExist) {
					return false, nil
				}
				return false, err
			})
			if err != nil {
				t.Fatal(err)
			}
			if recovery.Found != test.found || recovery.Published != test.published {
				t.Fatalf("recovery = %+v, want found=%v published=%v", recovery, test.found, test.published)
			}
			wantA, wantB := "after-a", "after-b"
			if test.published {
				wantA, wantB = "before-a", "before-b"
			}
			if got := readBack(t, filepath.Join(workspace, "a.txt")); got != wantA {
				t.Fatalf("a.txt = %q, want %q", got, wantA)
			}
			if got := readBack(t, filepath.Join(workspace, "b.txt")); got != wantB {
				t.Fatalf("b.txt = %q, want %q", got, wantB)
			}
			if _, err := os.Stat(filepath.Join(journalDir, durableUndoJournalName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("recovered journal still exists: %v", err)
			}
			walkErr := filepath.WalkDir(workspace, func(path string, entry os.DirEntry, err error) error {
				if err == nil && isRestoreTempName(entry.Name()) {
					t.Errorf("recovery left checkpoint temporary %s", path)
				}
				return err
			})
			if walkErr != nil {
				t.Fatal(walkErr)
			}
		})
	}
}

func TestDurableUndoCrashHelper(t *testing.T) {
	if os.Getenv(durableUndoCrashHelper) != "1" {
		return
	}
	root := os.Getenv("SB_DURABLE_UNDO_ROOT")
	boundary := os.Getenv("SB_DURABLE_UNDO_BOUNDARY")
	workspace := filepath.Join(root, "workspace")
	journalDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(journalDir, "child.log")
	write(t, child, "staged child")
	paths := []string{filepath.Join(workspace, "a.txt"), filepath.Join(workspace, "b.txt")}
	write(t, paths[0], "before-a")
	write(t, paths[1], "before-b")
	recorder := NewRecorder()
	identity := TurnIdentity{SessionID: "source", OpeningMessage: 4}
	recorder.BeginTurn(identity.SessionID, identity.OpeningMessage, "retry crash boundary")
	for i, path := range paths {
		recorder.Record(path)
		write(t, path, fmt.Sprintf("after-%c", 'a'+i))
	}
	recorder.durableUndoHook = func(at durableUndoBoundary, index int) {
		name := ""
		switch at {
		case durableUndoJournalCreated:
			name = "journal-created"
		case durableUndoJournalHeaderWritten:
			name = "journal-header"
		case durableUndoJournalPayloadWritten:
			name = "journal-payload"
		case durableUndoJournalFileSynced:
			name = "journal-file-sync"
		case durableUndoJournalBeforeInstall:
			name = "journal-before-install"
		case durableUndoJournalAfterInstall:
			name = "journal-after-install"
		case durableUndoJournalReopened:
			name = "journal-reopened"
		case durableUndoBeforeRestore:
			name = "before-restore"
		case durableUndoAfterRestore:
			name = fmt.Sprintf("after-restore-%d", index)
		case durableUndoBeforeCommit:
			name = "before-publish"
		case durableUndoAfterCommit:
			name = "after-publish"
		}
		if name == boundary {
			markDurableUndoCrashBoundary(t, filepath.Join(root, "ready"))
			select {}
		}
	}
	recorder.restoreTempLedgerHook = func(at restoreTempLedgerBoundary) {
		name := ""
		switch at {
		case restoreLedgerReservationBeforeSync:
			name = "lease-reservation-before-sync"
		case restoreLedgerReservationAfterSync:
			name = "lease-reservation-after-sync"
		case restoreLedgerBindingBeforeSync:
			name = "lease-binding-before-sync"
		case restoreLedgerBindingAfterSync:
			name = "lease-binding-after-sync"
		case restoreLedgerDisplacedBeforeSync:
			name = "lease-displaced-before-sync"
		case restoreLedgerDisplacedAfterSync:
			name = "lease-displaced-after-sync"
		}
		if name == boundary {
			markDurableUndoCrashBoundary(t, filepath.Join(root, "ready"))
			select {}
		}
	}
	prepared, err := recorder.PrepareDurableUndoCurrent(identity, journalDir, workspace, child, "test-child-identity")
	if err != nil {
		t.Fatal(err)
	}
	recorder.beforeReplaceHook = func() error {
		if boundary == "before-file-replace" {
			markDurableUndoCrashBoundary(t, filepath.Join(root, "ready"))
			select {}
		}
		return nil
	}
	_, err = prepared.ApplyAndCommit(func() error {
		return markDurableUndoCrashPublication(filepath.Join(root, "published"))
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("helper completed without reaching boundary %q", boundary)
}

func TestPrepareDurableUndoRejectsSymlinkChild(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	journalDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	realChild := filepath.Join(journalDir, "real.log")
	linkedChild := filepath.Join(journalDir, "linked.log")
	write(t, realChild, "not a session, but a real regular file")
	if err := os.Symlink(realChild, linkedChild); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(workspace, "target.txt")
	write(t, target, "before")
	recorder := NewRecorder()
	identity := TurnIdentity{SessionID: "source", OpeningMessage: 0}
	recorder.BeginTurn(identity.SessionID, identity.OpeningMessage, "symlink child")
	recorder.Record(target)
	write(t, target, "after")
	if _, err := recorder.PrepareDurableUndoCurrent(identity, journalDir, workspace, linkedChild, "test-child-identity"); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlink child preparation error = %v", err)
	}
}

func markDurableUndoCrashBoundary(t *testing.T, path string) {
	t.Helper()
	if err := markDurableUndoCrashPublication(path); err != nil {
		t.Fatal(err)
	}
}

func markDurableUndoCrashPublication(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString("durable\n"); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
