//go:build unix

package session

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPublicationRecoveryOpensMarkerThroughBoundDirectoryDuringParentSwap(t *testing.T) {
	store, _, _, logPath, markerPath := publishedStagedSession(t)
	_ = store
	parent := filepath.Dir(logPath)
	movedParent := parent + "-moved"
	identityBytes := readTestFile(t, markerPath)
	startFile, err := openSessionLog(logPath, false)
	if err != nil {
		t.Fatal(err)
	}
	start, err := readFirstSessionStart(startFile, logPath)
	_ = startFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	identity := publicationRecoveryIdentity(start.ID, start.PublicationID)

	beforeCalls := 0
	afterCalls := 0
	var hookErr error
	published, recoveryErr := ensurePublicationDurableExpectedWithHooks(logPath, identity,
		func(marker *os.File) error { return marker.Sync() },
		syncOpenedSessionDirectory,
		func() {
			beforeCalls++
			if err := os.Rename(parent, movedParent); err != nil {
				hookErr = err
				return
			}
			if err := os.Mkdir(parent, 0o700); err != nil {
				hookErr = err
			}
		},
		func() {
			afterCalls++
			if hookErr != nil {
				return
			}
			if err := os.Remove(parent); err != nil {
				hookErr = err
				return
			}
			if err := os.Rename(movedParent, parent); err != nil {
				hookErr = err
			}
		})
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if recoveryErr != nil || !published {
		t.Fatalf("rooted recovery during parent swap = %v, %v; want durable", published, recoveryErr)
	}
	if beforeCalls != 1 || afterCalls != 1 {
		t.Fatalf("parent swap hook calls = before %d after %d; want 1, 1", beforeCalls, afterCalls)
	}
	if got := readTestFile(t, markerPath); !bytes.Equal(got, identityBytes) {
		t.Fatal("rooted parent-swap recovery changed the publication marker")
	}
}

func TestPublishedStagedOpenParentFIFOReplacementDoesNotBlockOrMutate(t *testing.T) {
	store, _, id, logPath, markerPath := publishedStagedSession(t)
	parent := filepath.Dir(logPath)
	movedParent := parent + "-moved"
	logBefore := readTestFile(t, logPath)
	markerBefore := readTestFile(t, markerPath)
	hookCalls := 0
	var hookErr error
	store.openPublicationBeforeDirectory = func(path string) {
		hookCalls++
		if path != parent {
			hookErr = fmt.Errorf("directory hook path = %q, want %q", path, parent)
			return
		}
		if err := os.Rename(parent, movedParent); err != nil {
			hookErr = fmt.Errorf("moving publication parent: %w", err)
			return
		}
		if err := unix.Mkfifo(parent, 0o600); err != nil {
			hookErr = fmt.Errorf("installing publication parent FIFO: %w", err)
		}
	}

	done := make(chan error, 1)
	go func() {
		opened, err := store.Open(id)
		if opened != nil {
			_ = opened.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		assertPublicationResumeRecoveryError(t, err, nil)
	case <-time.After(2 * time.Second):
		t.Fatal("writable resume blocked on a publication-parent FIFO replacement")
	}
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if hookCalls != 1 {
		t.Fatalf("directory replacement hook calls = %d, want 1", hookCalls)
	}
	if got := readTestFile(t, filepath.Join(movedParent, filepath.Base(logPath))); !bytes.Equal(got, logBefore) {
		t.Fatal("parent FIFO refusal mutated the moved session log")
	}
	if got := readTestFile(t, filepath.Join(movedParent, filepath.Base(markerPath))); !bytes.Equal(got, markerBefore) {
		t.Fatal("parent FIFO refusal mutated the moved publication marker")
	}
}
