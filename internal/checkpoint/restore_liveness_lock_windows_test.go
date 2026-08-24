//go:build windows

package checkpoint

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWindowsUndoReplaceWithRestoreLivenessLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edited.txt")
	write(t, path, "before")
	r := NewRecorder()
	r.Begin("edit")
	r.Record(path)
	write(t, path, "after")

	restored, removed, skipped, failed, _, err := r.Undo()
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 1 || restored[0] != path || len(removed) != 0 || len(skipped) != 0 || len(failed) != 0 {
		t.Fatalf("undo = restored %v removed %v skipped %v failed %v", restored, removed, skipped, failed)
	}
	if got := readBack(t, path); got != "before" {
		t.Fatalf("restored content = %q, want before", got)
	}
}

func TestWindowsUndoRemoveWithRestoreLivenessLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "created.txt")
	r := NewRecorder()
	r.Begin("create")
	r.Record(path)
	write(t, path, "created")

	restored, removed, skipped, failed, _, err := r.Undo()
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 0 || len(removed) != 1 || removed[0] != path || len(skipped) != 0 || len(failed) != 0 {
		t.Fatalf("undo = restored %v removed %v skipped %v failed %v", restored, removed, skipped, failed)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created path remains after undo: %v", err)
	}
}

func TestWindowsRestoreLivenessLockAllowsReadAndExcludesRecovery(t *testing.T) {
	workspace := t.TempDir()
	scope, err := openRestoreScope(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer scope.close()
	tempName := ".switchboard-undo-" + strings.Repeat("a", 32)
	tempPath := filepath.Join(workspace, tempName)
	owner, err := scope.root.OpenFile(tempName, os.O_RDWR|os.O_CREATE|os.O_EXCL|os.O_SYNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.WriteString("checkpoint pre-image"); err != nil {
		_ = owner.Close()
		t.Fatal(err)
	}
	if err := owner.Sync(); err != nil {
		_ = owner.Close()
		t.Fatal(err)
	}
	if err := acquireRestoreLivenessLock(owner); err != nil {
		_ = owner.Close()
		t.Fatal(err)
	}
	identity, err := boundOpenFileIdentity(owner)
	if err != nil {
		_ = owner.Close()
		t.Fatal(err)
	}
	if got, err := os.ReadFile(tempPath); err != nil || string(got) != "checkpoint pre-image" {
		_ = owner.Close()
		t.Fatalf("independent read = %q, %v", got, err)
	}
	target := filepath.Join(workspace, "target.txt")
	owned := restoreCleanupIdentity{value: identity, owned: true}
	if err := cleanupExactWorkspaceTemp(scope, nil, target, tempName, owned); !errors.Is(err, ErrDurableUndoLocked) {
		_ = owner.Close()
		t.Fatalf("live recovery cleanup error = %v, want ErrDurableUndoLocked", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cleanupExactWorkspaceTemp(scope, nil, target, tempName, owned); err != nil {
		t.Fatalf("recovery cleanup after owner close: %v", err)
	}
	if _, err := os.Lstat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered temporary remains: %v", err)
	}
}

func TestWindowsReplacementTargetLivenessSerializesReplaceAndRemove(t *testing.T) {
	tests := []struct {
		name          string
		firstExisted  bool
		secondExisted bool
		wantContent   string
	}{
		{name: "replace blocks remove", firstExisted: true, secondExisted: false, wantContent: "first pre-image"},
		{name: "remove blocks replace", firstExisted: false, secondExisted: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "shared-target.txt")
			write(t, path, "shared post-image")
			first := newWindowsLivenessUndoRecorder(t, path, test.firstExisted, "first pre-image")
			second := newWindowsLivenessUndoRecorder(t, path, test.secondExisted, "second pre-image")

			entered := make(chan struct{})
			release := make(chan struct{})
			released := false
			defer func() {
				if !released {
					close(release)
				}
			}()
			first.publicationSeamHook = func() error {
				close(entered)
				<-release
				return nil
			}
			type undoResult struct {
				restored []string
				removed  []string
				failed   []string
				err      error
			}
			firstDone := make(chan undoResult, 1)
			go func() {
				restored, removed, _, failed, _, err := first.Undo()
				firstDone <- undoResult{restored: restored, removed: removed, failed: failed, err: err}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("first undo did not reach the locked publication seam")
			}

			restored, removed, _, failed, _, err := second.Undo()
			if err != nil {
				t.Fatal(err)
			}
			if len(restored) != 0 || len(removed) != 0 || len(failed) != 1 || !strings.Contains(failed[0], "locking") {
				t.Fatalf("competing undo = restored %v removed %v failed %v, want one pre-publication lock refusal", restored, removed, failed)
			}
			if got := readBack(t, path); got != "shared post-image" {
				t.Fatalf("competing undo changed target while owner was paused: %q", got)
			}

			close(release)
			released = true
			var completed undoResult
			select {
			case completed = <-firstDone:
			case <-time.After(5 * time.Second):
				t.Fatal("first undo did not finish after releasing its target lock")
			}
			if completed.err != nil || len(completed.failed) != 0 {
				t.Fatalf("first undo = %+v", completed)
			}
			if test.firstExisted {
				if len(completed.restored) != 1 || len(completed.removed) != 0 {
					t.Fatalf("first replace = restored %v removed %v", completed.restored, completed.removed)
				}
				if got := readBack(t, path); got != test.wantContent {
					t.Fatalf("first replacement content = %q, want %q", got, test.wantContent)
				}
			} else {
				if len(completed.restored) != 0 || len(completed.removed) != 1 {
					t.Fatalf("first remove = restored %v removed %v", completed.restored, completed.removed)
				}
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("first removal left target: %v", err)
				}
			}
			if details := second.Details(); len(details) != 1 || len(details[0].Paths) != 1 {
				t.Fatalf("lock-refused competing undo consumed its capture: %+v", details)
			}
		})
	}
}

func newWindowsLivenessUndoRecorder(t *testing.T, path string, existed bool, before string) *Recorder {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	recorder := NewRecorder()
	recorder.Begin("same-target liveness")
	mode := info.Mode()
	content := []byte(before)
	if !existed {
		mode = 0
		content = nil
	}
	recorder.RecordState(path, existed, mode, content)
	recorder.Commit(path, true, info.Mode(), sha256.Sum256([]byte("shared post-image")))
	return recorder
}
