//go:build unix

package hooks

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLoadRootedRejectsFIFOAndParentEscape(t *testing.T) {
	t.Run("FIFO", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, ".switchboard"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mkfifo(filepath.Join(root, ".switchboard", FileName), 0o600); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			_, err := LoadRooted(root, filepath.Join(".switchboard", FileName), root)
			result <- err
		}()
		select {
		case err := <-result:
			if err == nil {
				t.Fatal("FIFO declaration was accepted")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("hook declaration read blocked on FIFO")
		}
	})

	t.Run("parent swap", func(t *testing.T) {
		root := t.TempDir()
		external := t.TempDir()
		parent := filepath.Join(root, ".switchboard")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		inside := "[[hooks.pre_tool]]\nrun='inside'\n"
		outside := "[[hooks.pre_tool]]\nrun='outside'\n"
		if err := os.WriteFile(filepath.Join(parent, FileName), []byte(inside), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(external, FileName), []byte(outside), 0o600); err != nil {
			t.Fatal(err)
		}
		var swapErr error
		set, err := loadRootedWithHook(root, filepath.Join(".switchboard", FileName), root, func() {
			swapErr = os.Rename(parent, parent+"-old")
			if swapErr == nil {
				swapErr = os.Symlink(external, parent)
			}
		})
		if swapErr != nil {
			t.Fatal(swapErr)
		}
		if err == nil || set != nil {
			t.Fatalf("parent swap loaded outside hooks: set=%+v err=%v", set, err)
		}
	})
}
