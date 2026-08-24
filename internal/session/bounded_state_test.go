package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCandidatePathsUsesBoundedWorkspaceInventory(t *testing.T) {
	root := t.TempDir()
	store := &Store{root: root}
	id := "20260824T010203-deadbeef"
	for i := range 8 {
		dir := filepath.Join(root, fmt.Sprintf("workspace-%02d", i))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if i == 7 {
			if err := os.WriteFile(filepath.Join(dir, id+".log"), []byte("candidate"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	matches, err := store.candidatePaths(id, 8)
	if err != nil || len(matches) != 1 {
		t.Fatalf("candidatePaths exact bound = %v, %v", matches, err)
	}
	if err := os.Mkdir(filepath.Join(root, "workspace-08"), 0o700); err != nil {
		t.Fatal(err)
	}
	if matches, err := store.candidatePaths(id, 8); !errors.Is(err, ErrSessionInventoryTooLarge) || matches != nil {
		t.Fatalf("candidatePaths over bound = %v, %v", matches, err)
	}
}

func TestStagedMaintenanceStopsBeforeLargeDirectorySelection(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "workspace")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < stagedScanLimit+2; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%04d.txt", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store := &Store{root: root}
	report := store.cleanupExpiredStaged(time.Now(), stagedCleanupLimit)
	if report.Scanned != stagedScanLimit || report.Removed != 0 {
		t.Fatalf("maintenance report = %+v", report)
	}
}
