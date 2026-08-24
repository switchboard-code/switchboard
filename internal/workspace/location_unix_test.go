//go:build unix

package workspace

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestReadBinaryRefusesFIFOWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := w.ReadBinary("pipe", 1024); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("FIFO read = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("FIFO refusal blocked for %v", elapsed)
	}
}

func TestReadBinaryStressRejectsConcurrentSymlinkSwap(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	inside := []byte("inside-safe-image")
	secret := []byte("outside-swap-secret")
	target := filepath.Join(root, "safe.png")
	heldLink := filepath.Join(root, "held-link")
	heldFile := filepath.Join(root, "held-file")
	outside := filepath.Join(parent, "outside")
	if err := os.WriteFile(target, inside, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, secret, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, heldLink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}

	const swaps = 256
	started := make(chan struct{})
	done := make(chan struct{})
	swapErr := make(chan error, 1)
	go func() {
		defer close(done)
		close(started)
		for range swaps {
			for _, move := range [][2]string{
				{target, heldFile},
				{heldLink, target},
				{target, heldLink},
				{heldFile, target},
			} {
				if err := os.Rename(move[0], move[1]); err != nil {
					swapErr <- err
					return
				}
				runtime.Gosched()
			}
		}
	}()
	<-started

	reads := 0
	var unexpected []byte
	unexpectedSet := false
readLoop:
	for {
		select {
		case <-done:
			break readLoop
		default:
		}
		doc, err := w.ReadBinary("safe.png", 1024)
		reads++
		if err == nil && !bytes.Equal(doc.Content, inside) {
			unexpected = append([]byte(nil), doc.Content...)
			unexpectedSet = true
			break readLoop
		}
	}
	<-done
	select {
	case err := <-swapErr:
		t.Fatal(err)
	default:
	}
	if reads == 0 {
		t.Fatal("swap stress completed without a concurrent read")
	}
	if unexpectedSet {
		t.Fatalf("concurrent swap returned non-workspace bytes: %q", unexpected)
	}
}

func TestReadBinaryDiscardsBytesWhenRootMovesAfterOpen(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	const secret = "moved-workspace-root-secret"
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := w.resolveReadPath("source.txt")
	if err != nil {
		t.Fatal(err)
	}
	document, err := w.readBinaryResolvedWithHook("source.txt", rel, 1024, func() {
		if err := os.Rename(root, filepath.Join(parent, "moved-workspace")); err != nil {
			t.Fatalf("moving opened workspace: %v", err)
		}
	})
	if !errors.Is(err, ErrStaleLocation) {
		t.Fatalf("moved-root read = %v, content %q", err, document.Content)
	}
	if bytes.Contains(document.Content, []byte(secret)) {
		t.Fatalf("moved-root read returned bytes: %q", document.Content)
	}
}
