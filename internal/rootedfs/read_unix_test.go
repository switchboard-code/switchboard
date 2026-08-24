//go:build unix

package rootedfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func awaitRead(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("rooted read blocked")
		return nil
	}
}

func TestReadFileRejectsFIFOAndParentEscapeWithoutBlocking(t *testing.T) {
	t.Run("direct FIFO", func(t *testing.T) {
		root := t.TempDir()
		if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			_, err := ReadFile(root, "pipe", 1024)
			result <- err
		}()
		if err := awaitRead(t, result); err == nil {
			t.Fatal("FIFO was accepted")
		}
	})

	t.Run("regular to FIFO", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "source")
		if err := os.WriteFile(path, []byte("inside"), 0o600); err != nil {
			t.Fatal(err)
		}
		var swapErr error
		result := make(chan error, 1)
		go func() {
			_, err := ReadFileWithHook(root, "source", 1024, func() {
				swapErr = os.Remove(path)
				if swapErr == nil {
					swapErr = unix.Mkfifo(path, 0o600)
				}
			})
			result <- err
		}()
		if err := awaitRead(t, result); err == nil {
			t.Fatal("FIFO replacement was accepted")
		}
		if swapErr != nil {
			t.Fatal(swapErr)
		}
	})

	t.Run("parent to outside symlink", func(t *testing.T) {
		root := t.TempDir()
		external := t.TempDir()
		parent := filepath.Join(root, "nested")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(parent, "source"), []byte("inside"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(external, "source"), []byte("outside-secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		var swapErr error
		data, err := ReadFileWithHook(root, filepath.Join("nested", "source"), 1024, func() {
			swapErr = os.Rename(parent, parent+"-old")
			if swapErr == nil {
				swapErr = os.Symlink(external, parent)
			}
		})
		if swapErr != nil {
			t.Fatal(swapErr)
		}
		if err == nil || strings.Contains(string(data), "outside-secret") {
			t.Fatalf("parent swap escaped root: data=%q err=%v", data, err)
		}
	})
}
