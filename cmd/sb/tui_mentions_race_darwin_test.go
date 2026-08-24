//go:build darwin

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"
)

// This is the end-to-end regression for the mention check/read race. macOS
// provides an atomic pathname exchange, so every read sees either the regular
// workspace file or an outside-root symlink and never a partially staged test
// fixture.
func TestExpandMentionsStressRefusesAtomicOutsideSwap(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	inside := []byte("inside-safe-image")
	secret := []byte("outside-image-secret")
	out := filepath.Join(parent, "outside")
	victim := filepath.Join(workspace, "safe.png")
	alternate := filepath.Join(workspace, "alternate")
	if err := os.WriteFile(out, secret, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, inside, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(out, alternate); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	swap := func() error {
		return unix.RenameatxNp(unix.AT_FDCWD, victim, unix.AT_FDCWD, alternate, unix.RENAME_SWAP)
	}
	if err := swap(); err != nil {
		t.Skipf("atomic pathname swap unavailable: %v", err)
	}
	if _, images := expandPromptMentions(workspace, "inspect @safe.png"); len(images) != 0 {
		t.Fatal("static outside-root symlink unexpectedly attached")
	}
	if err := swap(); err != nil {
		t.Fatal(err)
	}

	previous := runtime.GOMAXPROCS(max(runtime.GOMAXPROCS(0), 4))
	defer runtime.GOMAXPROCS(previous)
	const attempts = 20000
	var stop atomic.Bool
	var next atomic.Uint64
	var leaked atomic.Bool
	swapErrors := make(chan error, 1)
	var swapWG sync.WaitGroup
	swapWG.Add(1)
	go func() {
		defer swapWG.Done()
		for !stop.Load() {
			if err := swap(); err != nil {
				select {
				case swapErrors <- err:
				default:
				}
				return
			}
		}
	}()

	workers := max(runtime.GOMAXPROCS(0)-1, 2)
	var readerWG sync.WaitGroup
	readerWG.Add(workers)
	for range workers {
		go func() {
			defer readerWG.Done()
			for !leaked.Load() {
				if next.Add(1) > attempts {
					return
				}
				_, images := expandPromptMentions(workspace, "inspect @safe.png")
				for _, image := range images {
					if bytes.Equal(image.Data, secret) {
						leaked.Store(true)
						return
					}
					if !bytes.Equal(image.Data, inside) {
						leaked.Store(true)
						return
					}
				}
			}
		}()
	}
	readerWG.Wait()
	stop.Store(true)
	swapWG.Wait()
	select {
	case err := <-swapErrors:
		t.Fatal(err)
	default:
	}
	if leaked.Load() {
		t.Fatal("atomic pathname swap attached bytes other than the contained workspace file")
	}
}
