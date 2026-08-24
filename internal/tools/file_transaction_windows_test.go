//go:build windows

package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsTransactionalCreationUsesAchievablePostImageAndCheckpoint(t *testing.T) {
	r, root := newRegistry(t)
	recorder := newCheckpointRecorder(t, root)
	r.SetCheckpoints(recorder)
	recorder.Begin("create")
	if result := run(t, r, "write", map[string]any{
		"path": "new.txt", "content": "windows post-image",
	}); result.IsError {
		t.Fatalf("write failed after publication: %s", result.Content)
	}
	path := filepath.Join(root, "new.txt")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), restorableFileMode(0o644); got != want {
		t.Fatalf("mode=%o, want achievable mode %o", got, want)
	}
	if turns := recorder.Turns(); len(turns) != 1 || turns[0].Files != 1 || turns[0].Partial {
		t.Fatalf("checkpoint = %+v", turns)
	}
	if _, removed, _, failed, _, err := recorder.Undo(); err != nil || len(failed) != 0 || len(removed) != 1 {
		t.Fatalf("undo removed=%v failed=%v err=%v", removed, failed, err)
	}
}

func TestWindowsTransactionalEditPreservesReadOnlyState(t *testing.T) {
	r, root := newRegistry(t)
	path := filepath.Join(root, "readonly.txt")
	writeFile(t, path, "before")
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	run(t, r, "read", map[string]any{"path": "readonly.txt"})
	if result := run(t, r, "write", map[string]any{
		"path": "readonly.txt", "content": "after",
	}); result.IsError {
		t.Fatalf("write failed: %s", result.Content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o444 {
		t.Fatalf("read-only mode=%o, want 444", got)
	}
}
