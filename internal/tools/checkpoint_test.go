package tools

import (
	"os"
	"path/filepath"
	"testing"
)

// The registry's side of the undo contract: write and edit capture prior
// state before mutating, and a restored file must be re-read before the
// next mutation because ForgetVersions dropped its recorded version.
func TestWriteAndEditCaptureIntoTheCheckpoint(t *testing.T) {
	r, root := newRegistry(t)
	rec := newCheckpointRecorder(t, root)
	r.SetCheckpoints(rec)

	existing := filepath.Join(root, "existing.txt")
	writeFile(t, existing, "before\n")

	rec.Begin("the turn under test")
	run(t, r, "read", map[string]any{"path": "existing.txt"})
	run(t, r, "edit", map[string]any{"path": "existing.txt", "old_string": "before", "new_string": "after"})
	run(t, r, "write", map[string]any{"path": "fresh.txt", "content": "made this turn\n"})

	restored, removed, _, failed, _, err := rec.Undo()
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 1 || len(removed) != 1 || len(failed) != 0 {
		t.Fatalf("restored=%v removed=%v failed=%v", restored, removed, failed)
	}
	data, err := os.ReadFile(existing)
	if err != nil || string(data) != "before\n" {
		t.Errorf("existing.txt = %q, want the pre-turn content", data)
	}
	if _, statErr := os.Stat(filepath.Join(root, "fresh.txt")); !os.IsNotExist(statErr) {
		t.Error("the file write created must be gone after undo")
	}

	// After ForgetVersions, an edit without a fresh read must refuse.
	r.ForgetVersions(restored)
	res := run(t, r, "edit", map[string]any{"path": "existing.txt", "old_string": "before", "new_string": "again"})
	if !res.IsError {
		t.Error("an edit after undo must demand a re-read first")
	}
}
