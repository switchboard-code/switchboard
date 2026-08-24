//go:build unix

package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOwnedRemoveRecoveryCandidateFIFOSwapCannotBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.log")
	original := filepath.Join(dir, "original.log")
	if err := os.WriteFile(path, []byte("owned original"), 0o600); err != nil {
		t.Fatal(err)
	}
	expected, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, original); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("foreign replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	ownedRemoveBeforeCandidateOpenTestHook = func(candidate string) {
		if removeErr := os.Remove(candidate); removeErr != nil {
			t.Errorf("remove recovery candidate: %v", removeErr)
			return
		}
		if fifoErr := unix.Mkfifo(candidate, 0o600); fifoErr != nil {
			t.Errorf("replace recovery candidate with FIFO: %v", fifoErr)
		}
	}
	defer func() { ownedRemoveBeforeCandidateOpenTestHook = nil }()

	done := make(chan error, 1)
	go func() { done <- removePathIfSame(path, expected) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "recovery candidate changed identity") ||
			!strings.Contains(err.Error(), "replacement retained at") {
			t.Fatalf("FIFO swap error = %v, want explicit retained recovery evidence", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("owned removal blocked opening a FIFO recovery candidate")
	}

	if data, readErr := os.ReadFile(original); readErr != nil || string(data) != "owned original" {
		t.Fatalf("owned original changed: %q, %v", data, readErr)
	}
}
