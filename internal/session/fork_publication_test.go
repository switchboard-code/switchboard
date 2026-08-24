package session

import (
	"errors"
	"strings"
	"testing"
)

func TestForkPublicationStopsWithoutDiscardAfterVisibleDurabilityFailure(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	fork, err := store.CreateStaged(workspace, "test/local/fork", "rev")
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected marker sync failure")
	fork.publicationFault = func(step publicationStep) error {
		if step == publicationStepCommitSync {
			return injected
		}
		return nil
	}
	id, path := fork.ID(), fork.Path()
	err = publishForkDurably(fork)
	if err == nil || !strings.Contains(err.Error(), "became visible") || !strings.Contains(err.Error(), "restart Switchboard") {
		t.Fatalf("fork publication error = %v", err)
	}
	if fork.PublicationPending() {
		t.Fatal("visible fork was left rollbackable")
	}
	if published, statusErr := PublicationStatus(path); statusErr != nil || !published {
		t.Fatalf("visible fork status = %v, %v", published, statusErr)
	}
	reopened, err := store.Open(id)
	if err != nil {
		t.Fatalf("visible fork was discarded after durability failure: %v", err)
	}
	_ = reopened.Close()
}
