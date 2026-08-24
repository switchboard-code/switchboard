//go:build unix

package rootedfs

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOpenRootRejectsReplacementFIFOWithoutBlocking(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "root")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(directory, directory+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(directory, 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		root, err := OpenRoot(directory)
		if root != nil {
			_ = root.Close()
		}
		done <- err
	}()
	assertRootOpenRefusedPromptly(t, done)
}

func TestOpenRootRefusesEmptyPath(t *testing.T) {
	if root, err := OpenRoot(""); err == nil || root != nil {
		t.Fatalf("empty root = %v, %v", root, err)
	}
}

func TestOpenRootAtRejectsReplacementFIFOWithoutBlocking(t *testing.T) {
	parent := t.TempDir()
	root, err := OpenRoot(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	directory := filepath.Join(parent, "child")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(directory, directory+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(directory, 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		child, err := OpenRootAt(root, "child")
		if child != nil {
			_ = child.Close()
		}
		done <- err
	}()
	assertRootOpenRefusedPromptly(t, done)
}

func assertRootOpenRefusedPromptly(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("directory capability accepted a FIFO")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("directory capability blocked on a FIFO")
	}
}
