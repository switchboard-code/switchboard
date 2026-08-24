//go:build unix

package extensions

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPinnedInstallCacheOpenDoesNotFollowReplacementFIFO(t *testing.T) {
	parent := t.TempDir()
	requested := filepath.Join(parent, "cache")
	root, cachePath, err := openInstallCache(requested)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	moved := filepath.Join(parent, "cache-moved")
	if err := os.Rename(cachePath, moved); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(cachePath, 0o600); err != nil {
		t.Fatal(err)
	}

	type result struct {
		file *os.File
		err  error
	}
	done := make(chan result, 1)
	go func() {
		file, err := openPinnedInstallCacheDirectory(root)
		done <- result{file: file, err: err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		defer got.file.Close()
		opened, err := got.file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.Stat(moved)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(opened, want) {
			t.Fatal("pinned cache open followed the replacement path")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pinned cache open blocked on a replacement FIFO")
	}
}

func TestInstallCacheValidationDoesNotBlockOnReplacementFIFO(t *testing.T) {
	parent := t.TempDir()
	cachePath := filepath.Join(parent, "cache")
	if err := os.Mkdir(cachePath, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(parent, "cache-moved")
	if err := os.Rename(cachePath, moved); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(cachePath, 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- validateInstallCacheDirectoryPath(cachePath, info)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("validation accepted a FIFO replacement")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("install cache validation blocked on a replacement FIFO")
	}
}
