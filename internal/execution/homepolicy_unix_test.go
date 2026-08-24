//go:build unix

package execution

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSafeHomeDescendantRejectsReplacementFIFOWithoutBlocking(t *testing.T) {
	parent := t.TempDir()
	home := filepath.Join(parent, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(home, home+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(home, 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan bool, 1)
	go func() {
		_, ok := safeHomeDescendant(home, ".cache", false)
		done <- ok
	}()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("replacement FIFO was accepted as a home root")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("opening a replacement FIFO as a home root blocked")
	}
}
