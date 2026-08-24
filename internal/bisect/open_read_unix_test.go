//go:build unix

package bisect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestBisectCaptureRejectsFIFOAndOversizeWithoutBlocking(t *testing.T) {
	t.Run("direct FIFO", func(t *testing.T) {
		workspace := canonicalTempDir(t)
		path := filepath.Join(workspace, "target")
		if err := unix.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		authority, err := bindWorkspaceAuthority(workspace)
		if err != nil {
			t.Fatal(err)
		}
		defer authority.close()
		result := make(chan error, 1)
		go func() {
			_, err := authority.capture(path, nil)
			result <- err
		}()
		select {
		case err := <-result:
			if err == nil || !strings.Contains(err.Error(), "regular file") {
				t.Fatalf("FIFO capture error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("bisect capture blocked on a FIFO")
		}
	})

	t.Run("swap to FIFO", func(t *testing.T) {
		workspace := canonicalTempDir(t)
		path := filepath.Join(workspace, "target")
		if err := os.WriteFile(path, []byte("current"), 0o600); err != nil {
			t.Fatal(err)
		}
		authority, err := bindWorkspaceAuthority(workspace)
		if err != nil {
			t.Fatal(err)
		}
		defer authority.close()
		var swapErr error
		result := make(chan error, 1)
		go func() {
			_, err := authority.capture(path, func() {
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
				t.Fatal("bisect capture accepted FIFO replacement")
			}
			if swapErr != nil {
				t.Fatal(swapErr)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("bisect capture blocked after FIFO swap")
		}
	})

	t.Run("oversize", func(t *testing.T) {
		workspace := canonicalTempDir(t)
		path := filepath.Join(workspace, "target")
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(maxBisectFileBytes + 1); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		authority, err := bindWorkspaceAuthority(workspace)
		if err != nil {
			t.Fatal(err)
		}
		defer authority.close()
		if _, err := authority.capture(path, nil); err == nil || !strings.Contains(err.Error(), "checkpoint file limit") {
			t.Fatalf("oversize capture error = %v", err)
		}
	})
}
