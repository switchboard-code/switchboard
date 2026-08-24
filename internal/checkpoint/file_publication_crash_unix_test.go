//go:build unix

package checkpoint

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const publishFileCASCrashHelper = "SB_PUBLISH_FILE_CAS_CRASH_HELPER"

func TestPublishFileCASCrashRecoveryLeavesNoWorkspaceTemporary(t *testing.T) {
	for _, test := range []struct {
		boundary string
		want     string
	}{
		{boundary: "binding-after-sync", want: "before"},
		{boundary: "displaced-after-sync", want: "before"},
		{boundary: "parent-moved-before-linearization", want: "before"},
		{boundary: "after-namespace", want: "after"},
		{boundary: "after-replace", want: "after"},
	} {
		t.Run(test.boundary, func(t *testing.T) {
			base := t.TempDir()
			ready := filepath.Join(base, "ready")
			command := exec.Command(os.Args[0], "-test.run=^TestPublishFileCASCrashHelper$")
			command.Env = append(os.Environ(),
				publishFileCASCrashHelper+"=1",
				"SB_PUBLISH_FILE_CAS_ROOT="+base,
				"SB_PUBLISH_FILE_CAS_BOUNDARY="+test.boundary,
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
				t.Fatal("crash helper exited successfully")
			}

			workspace := filepath.Join(base, "workspace")
			journalDir := filepath.Join(base, "sessions")
			target := filepath.Join(workspace, "target.txt")
			if test.boundary == "parent-moved-before-linearization" {
				movedParent := filepath.Join(base, "outside-moved")
				parentPath := filepath.Join(workspace, "nested")
				if got := readBack(t, filepath.Join(movedParent, "target.txt")); got != "before" {
					t.Fatalf("moved target before recovery = %q, want before", got)
				}
				if err := os.Rename(movedParent, parentPath); err != nil {
					t.Fatal(err)
				}
				target = filepath.Join(parentPath, "target.txt")
			}
			if _, err := RecoverDurableUndo(journalDir, workspace, func(string, string) (bool, error) {
				return false, nil
			}); err != nil {
				t.Fatal(err)
			}
			if got := readBack(t, target); got != test.want {
				t.Fatalf("recovered target = %q, want %q", got, test.want)
			}
			assertNoPublicationArtifacts(t, workspace, journalDir)
		})
	}
}

func TestPublishFileCASCrashHelper(t *testing.T) {
	if os.Getenv(publishFileCASCrashHelper) != "1" {
		return
	}
	base := os.Getenv("SB_PUBLISH_FILE_CAS_ROOT")
	boundary := os.Getenv("SB_PUBLISH_FILE_CAS_BOUNDARY")
	workspace := filepath.Join(base, "workspace")
	journalDir := filepath.Join(base, "sessions")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	parentPath := workspace
	if boundary == "parent-moved-before-linearization" {
		parentPath = filepath.Join(workspace, "nested")
		if err := os.MkdirAll(parentPath, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(parentPath, "target.txt")
	write(t, path, "before")
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	recorder := NewRecorder()
	if err := recorder.ConfigureRestoreCleanup(journalDir, workspace); err != nil {
		t.Fatal(err)
	}
	recorder.Begin("crash publication")
	recorder.RecordState(path, true, 0o644, []byte("before"))
	recorder.restoreTempLedgerHook = func(at restoreTempLedgerBoundary) {
		name := ""
		switch at {
		case restoreLedgerBindingAfterSync:
			name = "binding-after-sync"
		case restoreLedgerDisplacedAfterSync:
			name = "displaced-after-sync"
		}
		if name == boundary {
			markPublishFileCASCrashBoundary(t, filepath.Join(base, "ready"))
			select {}
		}
	}
	recorder.afterReplaceHook = func() error {
		if boundary == "after-replace" {
			markPublishFileCASCrashBoundary(t, filepath.Join(base, "ready"))
			select {}
		}
		return nil
	}
	boundAfterNamespaceTestHook = func() error {
		if boundary == "after-namespace" {
			markPublishFileCASCrashBoundary(t, filepath.Join(base, "ready"))
			select {}
		}
		return nil
	}
	boundNamespaceLinearizationTestHook = func() {
		if boundary != "parent-moved-before-linearization" {
			return
		}
		boundNamespaceLinearizationTestHook = nil
		if err := os.Rename(parentPath, filepath.Join(base, "outside-moved")); err != nil {
			t.Fatal(err)
		}
		markPublishFileCASCrashBoundary(t, filepath.Join(base, "ready"))
		select {}
	}
	if _, err := recorder.PublishFileCAS(
		context.Background(), path, parent, filepath.Base(path),
		true, 0o644, []byte("before"), 0o644, []byte("after"), nil,
	); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("helper completed without reaching boundary %q", boundary)
}

func markPublishFileCASCrashBoundary(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_SYNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
