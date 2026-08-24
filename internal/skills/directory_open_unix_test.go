//go:build unix

package skills

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSkillDirectoryRejectsDirectAndSwappedFIFOsWithoutBlocking(t *testing.T) {
	t.Run("direct FIFO", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "skills")
		if err := unix.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			_, err := readSkillDirectory(path, maxSkillDirectoryEntries)
			result <- err
		}()
		select {
		case err := <-result:
			if err == nil {
				t.Fatal("skill directory accepted a FIFO")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("skill directory blocked on a FIFO")
		}
	})

	t.Run("swap to FIFO", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "skills")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		var swapErr error
		result := make(chan error, 1)
		go func() {
			_, err := readSkillDirectoryWithHook(path, maxSkillDirectoryEntries, func() {
				if err := os.Remove(path); err != nil {
					swapErr = err
					return
				}
				swapErr = unix.Mkfifo(path, 0o600)
			})
			result <- err
		}()
		select {
		case err := <-result:
			if err == nil {
				t.Fatal("skill directory accepted FIFO replacement")
			}
			if swapErr != nil {
				t.Fatal(swapErr)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("skill directory blocked after FIFO swap")
		}
	})
}
