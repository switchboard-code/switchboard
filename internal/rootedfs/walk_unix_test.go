//go:build unix

package rootedfs

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestWalkRegularFilesChildSwapToFIFODoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	writeWalkFile(t, filepath.Join(dir, "child", "inside"))
	root := openWalkRoot(t, dir)
	result := make(chan struct {
		status WalkStatus
		err    error
	}, 1)
	var swapErr error
	go func() {
		status, err := WalkRegularFiles(context.Background(), root, ".", walkTestLimits(20),
			func(relative string, _ fs.FileInfo) bool {
				if filepath.Base(relative) != "child" {
					return false
				}
				swapErr = os.Rename(filepath.Join(dir, "child"), filepath.Join(dir, "child-moved"))
				if swapErr == nil {
					swapErr = unix.Mkfifo(filepath.Join(dir, "child"), 0o600)
				}
				return false
			},
			func(string, *os.Root, string, fs.FileInfo) error { return nil })
		result <- struct {
			status WalkStatus
			err    error
		}{status: status, err: err}
	}()
	select {
	case got := <-result:
		if swapErr != nil {
			t.Fatal(swapErr)
		}
		if got.err != nil {
			t.Fatal(got.err)
		}
		if !got.status.Partial() || got.status.Omitted == 0 {
			t.Fatalf("FIFO replacement status=%+v", got.status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("walk blocked opening a FIFO replacement")
	}
}
