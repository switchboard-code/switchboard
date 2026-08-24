//go:build unix

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestHistoryParentBindingDoesNotBlockOnFIFOReplacement(t *testing.T) {
	base := t.TempDir()
	parentPath := filepath.Join(base, "history")
	if err := os.Mkdir(parentPath, 0o700); err != nil {
		t.Fatal(err)
	}
	expected, err := os.Lstat(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	moved := parentPath + "-moved"
	var swapErr error
	historyParentBeforeRootOpenTestHook = func(path string) {
		if path != parentPath {
			return
		}
		swapErr = os.Rename(parentPath, moved)
		if swapErr == nil {
			swapErr = unix.Mkfifo(parentPath, 0o600)
		}
	}
	t.Cleanup(func() { historyParentBeforeRootOpenTestHook = nil })

	done := make(chan error, 1)
	go func() {
		parent, err := openHistoryBoundParent(filepath.Join(parentPath, "history.jsonl"), expected)
		if parent != nil {
			_ = parent.close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if swapErr != nil {
			t.Fatal(swapErr)
		}
		if err == nil {
			t.Fatal("prompt history accepted a FIFO parent replacement")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("prompt history parent binding blocked on a FIFO replacement")
	}
}
