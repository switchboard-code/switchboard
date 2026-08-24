//go:build windows

package session

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsLockFileExRejectsASecondWriterAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.log")
	first, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	if err := acquireLock(first); err != nil {
		t.Fatal(err)
	}
	if err := acquireLock(second); !errors.Is(err, ErrSessionLocked) {
		t.Fatalf("second lock err = %v, want ErrSessionLocked", err)
	}
	if err := releaseLock(first); err != nil {
		t.Fatal(err)
	}
	if err := acquireLock(second); err != nil {
		t.Fatalf("lock did not release: %v", err)
	}
	if err := releaseLock(second); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsLockFileExAllowsIndependentTranscriptRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.log")
	want := []byte("session transcript remains readable")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	owner, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()

	if err := acquireLock(owner); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := releaseLock(owner); err != nil {
			t.Errorf("release lock: %v", err)
		}
	}()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read transcript through an independent handle: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("transcript = %q, want %q", got, want)
	}
}

func TestWindowsLiveSessionKeepsWriterExclusiveWhileAllowingReadAndFork(t *testing.T) {
	store, source := forkFixture(t)

	if second, err := store.Open(source.ID()); !errors.Is(err, ErrSessionLocked) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second writer err = %v, want ErrSessionLocked", err)
	}

	state, err := ReadState(source.Path())
	if err != nil {
		t.Fatalf("read live session through an independent handle: %v", err)
	}
	if state.ID != source.ID() || len(state.Messages) != len(source.State().Messages) {
		t.Fatalf("read state = ID %q with %d messages, want ID %q with %d",
			state.ID, len(state.Messages), source.ID(), len(source.State().Messages))
	}

	child, err := store.ForkSession(source, len(source.State().Messages))
	if err != nil {
		t.Fatalf("fork live session through an independent handle: %v", err)
	}
	defer child.Close()
	if child.ID() == source.ID() || len(child.State().Messages) != len(state.Messages) {
		t.Fatalf("fork = ID %q with %d messages, source ID %q with %d",
			child.ID(), len(child.State().Messages), source.ID(), len(state.Messages))
	}
}
