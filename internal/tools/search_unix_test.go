//go:build unix

package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSearchDirectorySwapToFIFODoesNotBlock(t *testing.T) {
	r, root := newRegistry(t)
	base := filepath.Join(root, "nested")
	writeFile(t, filepath.Join(base, "source.txt"), "ordinary\n")
	done := make(chan struct {
		result Result
		err    error
	}, 1)
	var swapErr error
	go func() {
		result, err := (&globTool{r: r}).globWithHook(context.Background(), base, "*.txt", func() {
			swapErr = os.Rename(base, base+"-moved")
			if swapErr == nil {
				swapErr = unix.Mkfifo(base, 0o600)
			}
		})
		done <- struct {
			result Result
			err    error
		}{result: result, err: err}
	}()
	select {
	case got := <-done:
		if swapErr != nil {
			t.Fatal(swapErr)
		}
		if got.err != nil {
			t.Fatal(got.err)
		}
		if !got.result.IsError {
			t.Fatalf("FIFO replacement was accepted: %+v", got.result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("search blocked opening a FIFO replacement")
	}
}
