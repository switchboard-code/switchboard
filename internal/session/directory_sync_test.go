package session

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCreateSyncsLogAndWorkspaceDirectoryEntries(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	var synced []string
	store.createDirectorySync = func(path string) error {
		synced = append(synced, path)
		return syncSessionDirectory(path)
	}
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	wantWorkspaceDir := filepath.Join(store.root, workspaceKey(workspace))
	if !slices.Equal(synced, []string{wantWorkspaceDir, store.root}) {
		t.Fatalf("synced directories = %q, want workspace then store root", synced)
	}
}

func TestCreateDirectorySyncFailureRemovesUnacknowledgedLog(t *testing.T) {
	for _, staged := range []bool{false, true} {
		for _, faultAt := range []int{1, 2} {
			name := map[bool]string{false: "ordinary", true: "staged"}[staged]
			name += map[int]string{1: "/workspace-directory", 2: "/store-root"}[faultAt]
			t.Run(name, func(t *testing.T) {
				store, err := NewStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				workspace := t.TempDir()
				prior, err := store.Create(workspace, "test/local/prior", "rev")
				if err != nil {
					t.Fatal(err)
				}
				priorID := prior.ID()
				if err := prior.Close(); err != nil {
					t.Fatal(err)
				}

				injected := errors.New("injected directory sync failure")
				syncCalls := 0
				store.createDirectorySync = func(path string) error {
					syncCalls++
					if syncCalls == faultAt {
						return injected
					}
					return syncSessionDirectory(path)
				}
				var created *Session
				if staged {
					created, err = store.CreateStaged(workspace, "test/local/new", "rev")
				} else {
					created, err = store.Create(workspace, "test/local/new", "rev")
				}
				if created != nil || !errors.Is(err, injected) {
					t.Fatalf("Create = (%v, %v), want nil and injected sync error", created, err)
				}

				dir := filepath.Join(store.root, workspaceKey(workspace))
				entries, readErr := os.ReadDir(dir)
				if readErr != nil {
					t.Fatal(readErr)
				}
				var logs []string
				for _, entry := range entries {
					if strings.HasSuffix(entry.Name(), ".log") {
						logs = append(logs, entry.Name())
					}
				}
				if len(logs) != 1 || logs[0] != priorID+".log" {
					t.Fatalf("logs after failed Create = %q, want only prior", logs)
				}
				latest, latestErr := store.Latest(workspace)
				if latestErr != nil {
					t.Fatal(latestErr)
				}
				defer latest.Close()
				if latest.ID() != priorID {
					t.Fatalf("Latest after failed Create = %s, want prior %s", latest.ID(), priorID)
				}
			})
		}
	}
}
