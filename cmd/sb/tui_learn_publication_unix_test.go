//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLearnDestinationInspectionRefusesSymlinkedSkillTree(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(workspace, ".agents")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := inspectLearnedSkillDestination(workspace, "escaped"); err == nil {
		t.Fatal("symlinked .agents tree was accepted")
	}
	if _, err := os.Lstat(filepath.Join(external, "skills")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspection touched symlink destination: %v", err)
	}
}

func TestLearnDestinationFIFOIsRefusedWithoutBlocking(t *testing.T) {
	workspace := t.TempDir()
	store := newLearnPublicationStore(t)
	dir := filepath.Join(workspace, ".agents", "skills", "fifo-skill")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "SKILL.md")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, exists, err := inspectLearnedSkillDestination(workspace, "fifo-skill")
		if err == nil && !exists {
			err = errors.New("FIFO destination was reported absent")
		}
		if err == nil {
			_, err = publishLearnedSkill(context.Background(), store, workspace, "fifo-skill", "safe")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO destination was accepted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("learn destination inspection blocked on a FIFO")
	}
}

func TestPublishLearnedSkillRefusesParentRetargetWithoutExternalMutation(t *testing.T) {
	workspace := t.TempDir()
	store := newLearnPublicationStore(t)
	external := t.TempDir()
	movedAgents := filepath.Join(t.TempDir(), "moved-agents")
	learnPublicationBeforeCommitTestHook = func() {
		if err := os.Rename(filepath.Join(workspace, ".agents"), movedAgents); err != nil {
			t.Errorf("rename .agents: %v", err)
			return
		}
		if err := os.Symlink(external, filepath.Join(workspace, ".agents")); err != nil {
			t.Errorf("replace .agents: %v", err)
		}
	}
	t.Cleanup(func() { learnPublicationBeforeCommitTestHook = nil })

	_, err := publishLearnedSkill(context.Background(), store, workspace, "retargeted", "safe content")
	if err == nil {
		t.Fatal("publication succeeded after its repository parent was retargeted")
	}
	if !strings.Contains(err.Error(), "changed identity") && !strings.Contains(err.Error(), "stale") &&
		!strings.Contains(err.Error(), "canceled") {
		t.Fatalf("parent retarget error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(external, "skills", "retargeted", "SKILL.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("publication mutated the external replacement: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(movedAgents, "skills", "retargeted", "SKILL.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("publication mutated the detached repository tree: %v", err)
	}
}
