package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
	"github.com/switchboard-code/switchboard/internal/session"
)

func newLearnPublicationStore(t *testing.T) *session.Store {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestPublishLearnedSkillUsesAbsentTargetCAS(t *testing.T) {
	workspace := t.TempDir()
	store := newLearnPublicationStore(t)
	want := "---\nname: release-checklist\ndescription: \"release it\"\n---\n\nRun the checks.\n"

	dest, err := publishLearnedSkill(context.Background(), store, workspace, "release-checklist", want)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if expected := filepath.Join(canonical, ".agents", "skills", "release-checklist", "SKILL.md"); dest != expected {
		t.Fatalf("destination = %q, want %q", dest, expected)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("published skill = %q, want %q", got, want)
	}
	if _, err := publishLearnedSkill(context.Background(), store, workspace, "release-checklist", "replacement"); err == nil {
		t.Fatal("second publication replaced an existing skill")
	}
	got, err = os.ReadFile(dest)
	if err != nil || string(got) != want {
		t.Fatalf("existing skill changed after collision: %q, %v", got, err)
	}
	_, exists, err := inspectLearnedSkillDestination(workspace, "release-checklist")
	if err != nil || !exists {
		t.Fatalf("destination inspection = exists %v, %v", exists, err)
	}
}

func TestPublishLearnedSkillRejectsOversizeBeforeCreatingRepositoryPaths(t *testing.T) {
	workspace := t.TempDir()
	store := newLearnPublicationStore(t)
	_, err := publishLearnedSkill(
		context.Background(), store, workspace, "too-large", strings.Repeat("x", maxLearnedSkillBytes+1),
	)
	if err == nil || !strings.Contains(err.Error(), "between 1") {
		t.Fatalf("oversize publication = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(workspace, ".agents")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversize publication touched repository: %v", err)
	}
}

func TestLearnPublicationLockIsOwnerPrivateAndCrossProcessExclusive(t *testing.T) {
	workspace := t.TempDir()
	store := newLearnPublicationStore(t)
	first, err := acquireLearnPublicationAuthority(context.Background(), store, workspace, "locked-skill")
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()
	ownerOnly, err := fileprivacy.IsOwnerOnly(first.lock)
	if err != nil || !ownerOnly {
		t.Fatalf("learn publication lock owner-only = %v, %v", ownerOnly, err)
	}
	if second, err := acquireLearnPublicationAuthority(context.Background(), store, workspace, "locked-skill"); err == nil || second != nil {
		if second != nil {
			_ = second.close()
		}
		t.Fatalf("second learn publication authority = %+v, %v", second, err)
	}
}

func TestInspectLearnedSkillDestinationDoesNotCreateDirectories(t *testing.T) {
	workspace := t.TempDir()
	dest, exists, err := inspectLearnedSkillDestination(workspace, "not-yet")
	if err != nil || exists {
		t.Fatalf("inspection = %q, exists %v, %v", dest, exists, err)
	}
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if dest != filepath.Join(canonical, ".agents", "skills", "not-yet", "SKILL.md") {
		t.Fatalf("destination = %q", dest)
	}
	if _, err := os.Lstat(filepath.Join(workspace, ".agents")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspection created repository directories: %v", err)
	}
}
