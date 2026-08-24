//go:build unix

package checkpoint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishFileCASMissingTargetFinalCollisionPreservesWriter(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	journalDir := filepath.Join(base, "sessions")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace, "target.txt")
	parent, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	recorder := NewRecorder()
	if err := recorder.ConfigureRestoreCleanup(journalDir, workspace); err != nil {
		t.Fatal(err)
	}
	recorder.Begin("missing target collision")
	recorder.RecordState(path, false, 0, nil)
	boundNamespaceMutationTestHook = func() {
		boundNamespaceMutationTestHook = nil
		write(t, path, "external")
	}
	t.Cleanup(func() { boundNamespaceMutationTestHook = nil })
	published, err := recorder.PublishFileCAS(
		context.Background(), path, parent, filepath.Base(path),
		false, 0, nil, 0o644, []byte("agent"), nil,
	)
	boundNamespaceMutationTestHook = nil
	if published || err == nil {
		t.Fatalf("PublishFileCAS() = published %v, %v; want no-replace collision", published, err)
	}
	recorder.Abort(path)
	if got := readBack(t, path); got != "external" {
		t.Fatalf("external target = %q", got)
	}
	if !errors.Is(err, os.ErrExist) {
		t.Logf("platform collision error did not wrap os.ErrExist: %v", err)
	}
	assertNoPublicationArtifacts(t, workspace, journalDir)
}

func TestPublishFileCASParentMovedOutsideBeforeLinearizationNeverPublishesThroughOrphan(t *testing.T) {
	for _, existed := range []bool{false, true} {
		name := "create"
		if existed {
			name = "replace"
		}
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			workspace := filepath.Join(base, "workspace")
			parentPath := filepath.Join(workspace, "nested")
			journalDir := filepath.Join(base, "sessions")
			outside := filepath.Join(base, "outside")
			movedParent := filepath.Join(outside, "moved")
			for _, dir := range []string{parentPath, journalDir, outside} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(parentPath, "target.txt")
			expected := ""
			if existed {
				expected = "before"
				write(t, path, expected)
			}
			parent, err := os.OpenRoot(parentPath)
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()
			recorder := NewRecorder()
			if err := recorder.ConfigureRestoreCleanup(journalDir, workspace); err != nil {
				t.Fatal(err)
			}
			recorder.Begin("parent move outside")
			recorder.RecordState(path, existed, 0o644, []byte(expected))

			boundNamespaceLinearizationTestHook = func() {
				boundNamespaceLinearizationTestHook = nil
				if err := os.Rename(parentPath, movedParent); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(parentPath, 0o700); err != nil {
					t.Fatal(err)
				}
				write(t, path, "replacement namespace sentinel")
			}
			t.Cleanup(func() { boundNamespaceLinearizationTestHook = nil })
			published, err := recorder.PublishFileCAS(
				context.Background(), path, parent, filepath.Base(path),
				existed, 0o644, []byte(expected),
				0o644, []byte("agent"), nil,
			)
			boundNamespaceLinearizationTestHook = nil
			if published || err == nil {
				t.Fatalf("parent-move publication = published %v, %v; want unpublished refusal", published, err)
			}
			recorder.Abort(path)
			if got := readBack(t, path); got != "replacement namespace sentinel" {
				t.Fatalf("replacement namespace target = %q", got)
			}
			movedTarget := filepath.Join(movedParent, "target.txt")
			if existed {
				if got := readBack(t, movedTarget); got != "before" {
					t.Fatalf("moved pre-image = %q, want before", got)
				}
			} else if _, statErr := os.Lstat(movedTarget); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("missing target was published outside: %v", statErr)
			}
			if !hasPublicationTempLedger(t, journalDir) {
				t.Fatal("moved temporary did not retain recovery evidence")
			}

			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(parentPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(movedParent, parentPath); err != nil {
				t.Fatal(err)
			}
			if err := RecoverFilePublicationCleanup(journalDir, workspace); err != nil {
				t.Fatal(err)
			}
			assertNoPublicationArtifacts(t, workspace, journalDir)
		})
	}
}
