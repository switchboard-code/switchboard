//go:build unix

package checkpoint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func awaitCheckpointRead(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("checkpoint read blocked on a FIFO")
		return nil
	}
}

func replaceCheckpointPathWithFIFO(path string) error {
	if err := os.Remove(path); err != nil {
		return err
	}
	return unix.Mkfifo(path, 0o600)
}

func TestCheckpointDirectFIFOIsRejectedWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "target")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := fingerprintPath(path)
		result <- err
	}()
	if err := awaitCheckpointRead(t, result); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("direct FIFO fingerprint error = %v", err)
	}
}

func TestCheckpointRecordAndFinalizationSwapsToFIFOAreNonblocking(t *testing.T) {
	t.Run("record snapshot", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "recorded")
		if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
			t.Fatal(err)
		}
		before, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		var swapErr error
		result := make(chan error, 1)
		go func() {
			_, err := readCheckpointPathContent(path, before, maxFileBytes, func() {
				swapErr = replaceCheckpointPathWithFIFO(path)
			})
			result <- err
		}()
		if err := awaitCheckpointRead(t, result); err == nil {
			t.Fatal("record snapshot accepted a FIFO replacement")
		}
		if swapErr != nil {
			t.Fatal(swapErr)
		}
	})

	t.Run("turn finalization", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "finalized")
		if err := os.WriteFile(path, []byte("after"), 0o600); err != nil {
			t.Fatal(err)
		}
		var swapErr error
		result := make(chan error, 1)
		go func() {
			_, err := fingerprintPathBoundedWithHooks(path, maxFileBytes, func() {
				swapErr = replaceCheckpointPathWithFIFO(path)
			}, nil)
			result <- err
		}()
		if err := awaitCheckpointRead(t, result); err == nil {
			t.Fatal("turn finalization accepted a FIFO replacement")
		}
		if swapErr != nil {
			t.Fatal(swapErr)
		}
	})
}

func TestCheckpointReviewAndRetrySnapshotSwapToFIFOIsNonblocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "post-image")
	content := []byte("after")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	parent, parents, parentSet := parentIdentity(path)
	state := &fileState{parent: parent, parents: parents, parentSet: parentSet}
	expected := fingerprintBytes(true, restorableMode(info.Mode()), content)
	var swapErr error
	result := make(chan error, 1)
	go func() {
		_, err := readSnapshotCurrentWithHooks(path, expected, state, func() {
			swapErr = replaceCheckpointPathWithFIFO(path)
		}, nil, maxFileBytes)
		result <- err
	}()
	if err := awaitCheckpointRead(t, result); err == nil {
		t.Fatal("review/retry snapshot accepted a FIFO replacement")
	}
	if swapErr != nil {
		t.Fatal(swapErr)
	}
}

func TestCheckpointRootedFingerprintSwapToFIFOIsNonblocking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var swapErr error
	result := make(chan error, 1)
	go func() {
		_, err := fingerprintInRootWithHooks(root, "target", path, maxFileBytes, func() {
			swapErr = replaceCheckpointPathWithFIFO(path)
		}, nil, nil)
		result <- err
	}()
	if err := awaitCheckpointRead(t, result); err == nil {
		t.Fatal("rooted checkpoint fingerprint accepted a FIFO replacement")
	}
	if swapErr != nil {
		t.Fatal(swapErr)
	}
}

func TestCheckpointUndoTargetSwapToFIFOIsNonblocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "target")
	after := []byte("after")
	if err := os.WriteFile(path, after, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	parent, parents, parentSet := parentIdentity(path)
	state := &fileState{
		existed: true, mode: restorableMode(info.Mode()), content: []byte("before"),
		after:  fingerprintBytes(true, restorableMode(info.Mode()), after),
		parent: parent, parents: parents, parentSet: parentSet,
	}
	result := make(chan error, 1)
	go func() {
		outcome := restore(path, state, restoreHooks{beforeReplace: func() error {
			return replaceCheckpointPathWithFIFO(path)
		}})
		if outcome.published {
			result <- os.ErrInvalid
			return
		}
		result <- outcome.err
	}()
	if err := awaitCheckpointRead(t, result); err == nil {
		t.Fatal("undo accepted a FIFO replacement")
	}
}
