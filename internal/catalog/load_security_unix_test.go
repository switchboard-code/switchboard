//go:build unix

package catalog

import (
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestApplyOverridesDoesNotBlockOnFIFO(t *testing.T) {
	c, err := loadBundled()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), UserOverrideFile)
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- c.applyOverrides(path) }()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("FIFO model override was accepted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("model override read blocked on FIFO")
	}
}
