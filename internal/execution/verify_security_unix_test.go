//go:build unix

package execution

import (
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestReadCheckDoesNotBlockOnFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox-check.json")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := readCheck(path)
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("FIFO sandbox cache was accepted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sandbox cache read blocked on FIFO")
	}
}
