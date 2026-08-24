package checkpoint

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestBoundReplacementCannotFollowParentSwap(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "target.txt")
	write(t, path, "before")

	recorder := NewRecorder()
	recorder.Begin("replace race")
	recorder.RecordState(path, true, 0o644, []byte("before"))
	write(t, path, "after")
	recorder.Commit(path, true, 0o644, sha256.Sum256([]byte("after")))

	moved, outside, outsidePath := replacementRacePaths(t, root, parent)
	hookRan := false
	recorder.beforeReplaceHook = func() error {
		hookRan = true
		return swapRestoreParent(parent, moved, outside)
	}
	outcome, _, err := recorder.UndoFile(path)
	if !hookRan {
		t.Fatal("replacement never reached the pre-publication race seam")
	}
	assertOutsideUnchanged(t, outsidePath)
	if !outcome.Published {
		// Windows may deny renaming an open directory capability, and hosts may
		// deny symlink creation. Refusal before publication is the safe result.
		if err == nil {
			t.Fatal("unpublished replacement returned no error")
		}
		assertAfterImageWherePresent(t, path, moved)
		return
	}
	if err == nil || !errors.Is(err, ErrStale) {
		t.Fatalf("published replacement error=%v, want a stale namespace report", err)
	}
	if got := readBack(t, filepath.Join(moved, "target.txt")); got != "before" {
		t.Fatalf("bound replacement restored %q in the captured directory, want before", got)
	}
}

func TestMissingTemporaryNameNeverScrubsInodeMovedOntoTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.txt")
	moved := path + ".postimage"
	write(t, path, "before")
	recorder := NewRecorder()
	recorder.Begin("temp moved onto target")
	recorder.RecordState(path, true, 0o644, []byte("before"))
	write(t, path, "after")
	recorder.Commit(path, true, 0o644, sha256.Sum256([]byte("after")))

	swapCompleted := false
	recorder.beforeReplaceHook = func() error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		var temp string
		for _, entry := range entries {
			if isRestoreTempName(entry.Name()) {
				temp = filepath.Join(dir, entry.Name())
				break
			}
		}
		if temp == "" {
			return errors.New("checkpoint temporary was not visible")
		}
		if err := os.Rename(path, moved); err != nil {
			return err
		}
		if err := os.Rename(temp, path); err != nil {
			_ = os.Rename(moved, path)
			return err
		}
		swapCompleted = true
		return nil
	}
	outcome, _, err := recorder.UndoFile(path)
	if !swapCompleted {
		if outcome.Published || err == nil {
			t.Fatalf("platform refused temp move with outcome=%+v err=%v", outcome, err)
		}
		return
	}
	if outcome.Published || !errors.Is(err, ErrStale) {
		t.Fatalf("temp-on-target outcome=%+v err=%v, want unpublished stale refusal", outcome, err)
	}
	if got := readBack(t, path); got != "before" {
		t.Fatalf("missing-name cleanup scrubbed target-linked temporary to %q", got)
	}
	if got := readBack(t, moved); got != "after" {
		t.Fatalf("post-image evidence changed to %q", got)
	}
}

func TestBoundRemovalCannotFollowParentSwap(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "target.txt")

	recorder := NewRecorder()
	recorder.Begin("remove race")
	recorder.RecordState(path, false, 0, nil)
	write(t, path, "after")
	recorder.Commit(path, true, 0o644, sha256.Sum256([]byte("after")))

	moved, outside, outsidePath := replacementRacePaths(t, root, parent)
	hookRan := false
	recorder.beforeRemoveHook = func() error {
		hookRan = true
		return swapRestoreParent(parent, moved, outside)
	}
	outcome, _, err := recorder.UndoFile(path)
	if !hookRan {
		t.Fatal("removal never reached the pre-publication race seam")
	}
	assertOutsideUnchanged(t, outsidePath)
	if !outcome.Published {
		if err == nil {
			t.Fatal("unpublished removal returned no error")
		}
		assertAfterImageWherePresent(t, path, moved)
		return
	}
	if err == nil || !errors.Is(err, ErrStale) {
		t.Fatalf("published removal error=%v, want a stale namespace report", err)
	}
	if _, statErr := os.Lstat(filepath.Join(moved, "target.txt")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("bound removal left the captured target behind: %v", statErr)
	}
}

func TestPreparedUndoDoesNotMistakeOutsideTwinForSuccessfulRollback(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "target.txt")
	write(t, path, "before")

	recorder := NewRecorder()
	identity := TurnIdentity{SessionID: "source", OpeningMessage: 2}
	recorder.BeginTurn(identity.SessionID, identity.OpeningMessage, "transaction race")
	recorder.RecordState(path, true, 0o644, []byte("before"))
	write(t, path, "after")
	recorder.Commit(path, true, 0o644, sha256.Sum256([]byte("after")))
	prepared, err := recorder.PrepareUndoCurrent(identity)
	if err != nil {
		t.Fatal(err)
	}

	moved, outside, outsidePath := replacementRacePaths(t, root, parent)
	swapCompleted := false
	recorder.beforeReplaceHook = func() error {
		if err := swapRestoreParent(parent, moved, outside); err != nil {
			return err
		}
		swapCompleted = true
		return nil
	}
	commitCalled := false
	_, err = prepared.ApplyAndCommit(func() error {
		commitCalled = true
		return nil
	})
	assertOutsideUnchanged(t, outsidePath)
	if commitCalled {
		t.Fatal("transaction committed after its restore namespace became stale")
	}
	if !swapCompleted {
		if err == nil {
			t.Fatal("platform refused the namespace swap without refusing the transaction")
		}
		return
	}
	if !errors.Is(err, ErrStale) || errors.Is(err, ErrDurableUndoRecoveryRequired) {
		t.Fatalf("transaction error=%v, want clean stale refusal after immediate rollback", err)
	}
	if got := readBack(t, filepath.Join(moved, "target.txt")); got != "after" {
		t.Fatalf("captured directory contains %q after immediate rollback, want original post-image", got)
	}
}

func TestScopedRecoveryCannotFollowWorkspaceRootSwap(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	parent := filepath.Join(workspace, "parent")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "target.txt")
	write(t, path, "after")

	scope, err := openRestoreScope(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer scope.close()
	parentInfo, err := scope.parentInfo(path)
	if err != nil {
		t.Fatal(err)
	}
	state := &fileState{
		existed: true, mode: 0o644, content: []byte("before"),
		after:  fingerprintBytes(true, 0o644, []byte("after")),
		parent: parentInfo, parentSet: true,
	}

	movedWorkspace := filepath.Join(base, "moved-workspace")
	outside := t.TempDir()
	outsidePath := filepath.Join(outside, "parent", "target.txt")
	if err := os.MkdirAll(filepath.Dir(outsidePath), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, outsidePath, "after")
	swapCompleted := false
	outcome := restoreInScope(scope, path, state, restoreHooks{beforeReplace: func() error {
		if err := os.Rename(workspace, movedWorkspace); err != nil {
			return fmt.Errorf("moving workspace at publication seam: %w", err)
		}
		if err := os.Symlink(outside, workspace); err != nil {
			return fmt.Errorf("installing outside workspace symlink at publication seam: %w", err)
		}
		swapCompleted = true
		return nil
	}})
	assertOutsideUnchanged(t, outsidePath)
	if !swapCompleted {
		if outcome.published || outcome.err == nil {
			t.Fatalf("platform refused workspace swap with outcome=%+v", outcome)
		}
		return
	}
	if outcome.published || !errors.Is(outcome.err, ErrStale) {
		t.Fatalf("scoped restore outcome=%+v, want rolled-back stale result", outcome)
	}
	if got := readBack(t, filepath.Join(movedWorkspace, "parent", "target.txt")); got != "after" {
		t.Fatalf("bound recovery rollback left %q in captured workspace, want after", got)
	}
}

func TestReplacementReportsPublishedFailureForSwappedTemporaryInode(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "target.txt")
	write(t, path, "before")

	recorder := NewRecorder()
	recorder.Begin("temporary swap")
	recorder.RecordState(path, true, 0o644, []byte("before"))
	write(t, path, "after")
	recorder.Commit(path, true, 0o644, sha256.Sum256([]byte("after")))

	recorder.beforeReplaceHook = func() error {
		entries, err := os.ReadDir(root)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !isRestoreTempName(entry.Name()) {
				continue
			}
			temp := filepath.Join(root, entry.Name())
			if err := os.Rename(temp, temp+".captured"); err != nil {
				return err
			}
			if err := os.WriteFile(temp, []byte("before"), 0o644); err != nil {
				return err
			}
			return nil
		}
		return errors.New("restore temporary was not visible at the publication seam")
	}
	outcome, _, err := recorder.UndoFile(path)
	if outcome.Published || !errors.Is(err, ErrStale) {
		t.Fatalf("temporary swap outcome=%+v error=%v, want unpublished stale refusal", outcome, err)
	}
	if got := readBack(t, path); got != "after" {
		t.Fatalf("temporary swap changed target to %q", got)
	}
}

func TestPreparedUndoRollsBackSwappedTemporaryInodeAndSkipsCommit(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "target.txt")
	write(t, path, "before")

	recorder := NewRecorder()
	identity := TurnIdentity{SessionID: "source", OpeningMessage: 9}
	recorder.BeginTurn(identity.SessionID, identity.OpeningMessage, "temporary transaction swap")
	recorder.RecordState(path, true, 0o644, []byte("before"))
	write(t, path, "after")
	recorder.Commit(path, true, 0o644, sha256.Sum256([]byte("after")))
	prepared, err := recorder.PrepareUndoCurrent(identity)
	if err != nil {
		t.Fatal(err)
	}

	swapped := false
	recorder.beforeReplaceHook = func() error {
		if swapped {
			return nil
		}
		swapped = true
		entries, err := os.ReadDir(root)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !isRestoreTempName(entry.Name()) {
				continue
			}
			temp := filepath.Join(root, entry.Name())
			if err := os.Rename(temp, temp+".captured"); err != nil {
				return err
			}
			return os.WriteFile(temp, []byte("before"), 0o644)
		}
		return errors.New("restore temporary was not visible at the publication seam")
	}
	commitCalled := false
	_, err = prepared.ApplyAndCommit(func() error {
		commitCalled = true
		return nil
	})
	if err == nil || !errors.Is(err, ErrStale) {
		t.Fatalf("transaction temporary swap error=%v, want stale refusal", err)
	}
	if commitCalled {
		t.Fatal("transaction commit ran after a foreign inode was published")
	}
	if got := readBack(t, path); got != "after" {
		t.Fatalf("transaction did not roll the target forward: %q", got)
	}
	if info, ok := recorder.CurrentTurn(identity); !ok || info.Files != 1 {
		t.Fatalf("checkpoint evidence was retired after failed publication: info=%+v ok=%v", info, ok)
	}
}

func replacementRacePaths(t *testing.T, root, parent string) (moved, outside, outsidePath string) {
	t.Helper()
	moved = filepath.Join(root, "moved-parent")
	outside = t.TempDir()
	outsidePath = filepath.Join(outside, "target.txt")
	write(t, outsidePath, "after")
	return moved, outside, outsidePath
}

func swapRestoreParent(parent, moved, outside string) error {
	if err := os.Rename(parent, moved); err != nil {
		return fmt.Errorf("moving parent at publication seam: %w", err)
	}
	if err := os.Symlink(outside, parent); err != nil {
		return fmt.Errorf("installing outside parent symlink at publication seam: %w", err)
	}
	return nil
}

func assertOutsideUnchanged(t *testing.T, path string) {
	t.Helper()
	if got := readBack(t, path); got != "after" {
		t.Fatalf("restore escaped its bound parent and changed outside target to %q", got)
	}
}

func assertAfterImageWherePresent(t *testing.T, original, moved string) {
	t.Helper()
	for _, path := range []string{original, filepath.Join(moved, "target.txt")} {
		content, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != "after" {
			t.Fatalf("unpublished restore changed %s to %q", path, content)
		}
		return
	}
	t.Fatal("unpublished restore lost the recorded post-image")
}
