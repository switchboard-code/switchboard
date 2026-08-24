package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func TestStagedSessionIsHiddenUntilItsOwnerPublishes(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	base, err := store.Create(workspace, "test/local/base", "rev")
	if err != nil {
		t.Fatal(err)
	}
	baseID := base.ID()
	if err := base.Close(); err != nil {
		t.Fatal(err)
	}

	staged, err := store.CreateStaged(workspace, "test/local/child", "rev")
	if err != nil {
		t.Fatal(err)
	}
	stagedID, stagedPath := staged.ID(), staged.Path()
	if err := staged.AppendNote("info", "fully built but not adopted"); err != nil {
		t.Fatal(err)
	}
	assertStagedHidden(t, store, workspace, stagedID, baseID)
	if _, err := os.Stat(stagedPath); err != nil {
		t.Fatalf("staged crash evidence missing: %v", err)
	}

	if err := staged.Publish(); err != nil {
		t.Fatal(err)
	}
	if err := staged.Publish(); err != nil {
		t.Fatalf("second publish by owning handle: %v", err)
	}
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
	infos, err := store.List(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[0].ID != stagedID {
		t.Fatalf("published infos = %#v, want staged child first and base second", infos)
	}
	reopened, err := store.Open(stagedID)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.PublicationPending() {
		t.Fatal("reopened published session reported a pending publication")
	}
	if err := reopened.Publish(); !errors.Is(err, ErrPublicationOwnership) {
		t.Fatalf("Publish through replayed handle = %v, want ErrPublicationOwnership", err)
	}
	_ = reopened.Close()
}

func TestCrashStagedSessionRemainsHidden(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	staged, err := store.CreateStaged(workspace, "test/local/child", "rev")
	if err != nil {
		t.Fatal(err)
	}
	id, path := staged.ID(), staged.Path()
	if err := staged.Close(); err != nil { // simulate a process exit, not an owned discard
		t.Fatal(err)
	}

	restarted, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	assertStagedHidden(t, restarted, workspace, id, "")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("crash-staged evidence should remain for bounded maintenance: %v", err)
	}
}

func TestExpiredCrashStageIsCleanedConservatively(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := store.CreateStaged(t.TempDir(), "test/local/child", "rev")
	if err != nil {
		t.Fatal(err)
	}
	path := staged.Path()
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-stagedRetention - time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	report := restarted.MaintenanceReport()
	if report.Expired != 1 || report.Removed != 1 || report.Failed != 0 {
		t.Fatalf("maintenance report = %#v", report)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired staged log still exists: %v", err)
	}
}

func TestStagedMaintenanceLeavesLockedPublishedAndForeignMarkerLogs(t *testing.T) {
	t.Run("locked", func(t *testing.T) {
		root := t.TempDir()
		store, _ := NewStore(root)
		staged, err := store.CreateStaged(t.TempDir(), "test/local/child", "rev")
		if err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-stagedRetention - time.Hour)
		if err := os.Chtimes(staged.Path(), old, old); err != nil {
			t.Fatal(err)
		}
		restarted, err := NewStore(root)
		if err != nil {
			t.Fatal(err)
		}
		if report := restarted.MaintenanceReport(); report.Locked != 1 || report.Removed != 0 {
			t.Fatalf("maintenance report = %#v", report)
		}
		if _, err := os.Stat(staged.Path()); err != nil {
			t.Fatalf("locked stage removed: %v", err)
		}
		_ = staged.CloseDiscardingStaged()
	})

	t.Run("published", func(t *testing.T) {
		root := t.TempDir()
		store, _ := NewStore(root)
		staged, err := store.CreateStaged(t.TempDir(), "test/local/child", "rev")
		if err != nil {
			t.Fatal(err)
		}
		if err := staged.Publish(); err != nil {
			t.Fatal(err)
		}
		path := staged.Path()
		id := staged.ID()
		_ = staged.Close()
		old := time.Now().Add(-stagedRetention - time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		restarted, err := NewStore(root)
		if err != nil {
			t.Fatal(err)
		}
		if report := restarted.MaintenanceReport(); report.Removed != 0 || report.Expired != 0 {
			t.Fatalf("maintenance report = %#v", report)
		}
		reopened, err := restarted.Open(id)
		if err != nil {
			t.Fatalf("published stage was removed: %v", err)
		}
		_ = reopened.Close()
	})

	t.Run("foreign marker", func(t *testing.T) {
		root := t.TempDir()
		store, _ := NewStore(root)
		staged, err := store.CreateStaged(t.TempDir(), "test/local/child", "rev")
		if err != nil {
			t.Fatal(err)
		}
		path := staged.Path()
		if err := os.WriteFile(publicationMarkerPath(path), []byte("foreign\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = staged.Close()
		old := time.Now().Add(-stagedRetention - time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		restarted, err := NewStore(root)
		if err != nil {
			t.Fatal(err)
		}
		if report := restarted.MaintenanceReport(); report.Refused != 1 || report.Removed != 0 {
			t.Fatalf("maintenance report = %#v", report)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("foreign-marker stage removed: %v", err)
		}
	})
}

func TestStagedMaintenanceRemovalIsBounded(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-stagedRetention - time.Hour)
	for i := 0; i < stagedCleanupLimit+3; i++ {
		staged, err := store.CreateStaged(t.TempDir(), "test/local/child", "rev")
		if err != nil {
			t.Fatal(err)
		}
		path := staged.Path()
		_ = staged.Close()
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	restarted, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if report := restarted.MaintenanceReport(); report.Removed != stagedCleanupLimit {
		t.Fatalf("maintenance removed %d, want bound %d: %#v", report.Removed, stagedCleanupLimit, report)
	}
}

func TestStagedMaintenanceRefusesTransientValidationFailure(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := store.CreateStaged(t.TempDir(), "test/local/child", "rev")
	if err != nil {
		t.Fatal(err)
	}
	path := staged.Path()
	_ = staged.Close()
	old := time.Now().Add(-stagedRetention - time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	transient := errors.New("transient marker read failure")
	store.maintenanceValidate = func(string, SessionStart) error { return transient }
	report := store.cleanupExpiredStaged(time.Now(), stagedCleanupLimit)
	if report.Removed != 0 || report.Refused != 1 || report.Failed != 1 {
		t.Fatalf("maintenance report = %#v", report)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("transient validation failure removed staged log: %v", err)
	}
}

func TestStagedMaintenanceRefusesMarkerThatCommitsAfterValidation(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := store.CreateStaged(t.TempDir(), "test/local/child", "rev")
	if err != nil {
		t.Fatal(err)
	}
	path := staged.Path()
	id, publicationID := staged.ID(), staged.state.publicationID
	_ = staged.Close()
	old := time.Now().Add(-stagedRetention - time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	store.maintenanceBeforeOwned = func(string) {
		if err := os.WriteFile(publicationMarkerPath(path), []byte(publicationMarker(id, publicationID)), 0o600); err != nil {
			t.Error(err)
		}
	}
	report := store.cleanupExpiredStaged(time.Now(), stagedCleanupLimit)
	if report.Removed != 0 || report.Refused != 1 {
		t.Fatalf("maintenance report = %#v", report)
	}
	if published, err := PublicationStatus(path); err != nil || !published {
		t.Fatalf("committed marker was deleted: status=%v err=%v", published, err)
	}
}

func TestStagedMaintenanceBasesExpiryOnLockedDescriptor(t *testing.T) {
	t.Run("fresh replacement before open", func(t *testing.T) {
		store, err := NewStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		staged, err := store.CreateStaged(t.TempDir(), "test/local/child", "rev")
		if err != nil {
			t.Fatal(err)
		}
		path := staged.Path()
		if err := staged.Close(); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-stagedRetention - time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		moved := path + ".old-entry"
		swapped := false
		store.maintenanceBeforeOpen = func(candidate string) {
			if candidate != path || swapped {
				return
			}
			swapped = true
			if err := os.Rename(path, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, before, 0o600); err != nil {
				t.Fatal(err)
			}
			fresh := time.Now().Add(time.Hour)
			if err := os.Chtimes(path, fresh, fresh); err != nil {
				t.Fatal(err)
			}
		}

		report := store.cleanupExpiredStaged(time.Now(), stagedCleanupLimit)
		if !swapped {
			t.Fatal("before-open replacement seam did not run")
		}
		if report.Removed != 0 || report.Expired != 0 {
			t.Fatalf("maintenance removed fresh replacement: %#v", report)
		}
		if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, before) {
			t.Fatalf("fresh replacement changed: %d bytes, %v", len(got), err)
		}
	})

	t.Run("refreshed before removal", func(t *testing.T) {
		store, err := NewStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		staged, err := store.CreateStaged(t.TempDir(), "test/local/child", "rev")
		if err != nil {
			t.Fatal(err)
		}
		path := staged.Path()
		if err := staged.Close(); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-stagedRetention - time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		store.maintenanceBeforeRemove = func(candidate string) {
			if candidate != path {
				return
			}
			fresh := time.Now().Add(time.Hour)
			if err := os.Chtimes(path, fresh, fresh); err != nil {
				t.Fatal(err)
			}
		}

		report := store.cleanupExpiredStaged(time.Now(), stagedCleanupLimit)
		if report.Removed != 0 {
			t.Fatalf("maintenance removed refreshed log: %#v", report)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("refreshed staged log was removed: %v", err)
		}
	})
}

func TestCleanupMarkerOwnedRequiresStrictIncompletePrefix(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	staged, err := store.CreateStaged(t.TempDir(), "test/local/child", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer staged.CloseDiscardingStaged()
	start := SessionStart{ID: staged.ID(), Staged: true, PublicationID: staged.state.publicationID}
	markerPath := publicationMarkerPath(staged.Path())
	complete := []byte(publicationMarker(start.ID, start.PublicationID))
	if err := os.WriteFile(markerPath, complete, 0o600); err != nil {
		t.Fatal(err)
	}
	if owned, err := cleanupMarkerOwned(staged.Path(), start); err != nil || owned {
		t.Fatalf("complete marker owned=%v err=%v, want refusal", owned, err)
	}
	if err := os.WriteFile(markerPath, complete[:len(complete)-1], 0o600); err != nil {
		t.Fatal(err)
	}
	if owned, err := cleanupMarkerOwned(staged.Path(), start); err != nil || !owned {
		t.Fatalf("strict partial marker owned=%v err=%v, want cleanup ownership", owned, err)
	}
}

func TestPublishRefusesMismatchedMarkerCollision(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	staged, err := store.CreateStaged(workspace, "test/local/child", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicationMarkerPath(staged.Path()), []byte("foreign marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := staged.Publish(); err == nil {
		t.Fatal("Publish accepted a mismatched marker collision")
	}
	if !staged.PublicationPending() {
		t.Fatal("failed collision publish cleared pending state")
	}
	assertStagedHidden(t, store, workspace, staged.ID(), "")
	if err := staged.CloseDiscardingStaged(); err == nil {
		t.Fatal("discard removed a log beside the mismatched marker")
	}
	if _, err := os.Stat(staged.Path()); err != nil {
		t.Fatalf("discard removed collision evidence: %v", err)
	}
}

func TestPublishRequiresCreatorLogToOwnItsPath(t *testing.T) {
	t.Run("replaced before marker", func(t *testing.T) {
		store, _ := NewStore(t.TempDir())
		staged, err := store.CreateStaged(t.TempDir(), "test/local/child", "rev")
		if err != nil {
			t.Fatal(err)
		}
		path := staged.Path()
		ownedPath := path + ".owned"
		if err := os.Rename(path, ownedPath); err != nil {
			t.Fatal(err)
		}
		replacement := []byte("replacement must survive")
		if err := os.WriteFile(path, replacement, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := staged.Publish(); err == nil {
			t.Fatal("Publish accepted a pathname that no longer names its creator log")
		}
		if _, err := os.Lstat(publicationMarkerPath(path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed Publish left marker: %v", err)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != string(replacement) {
			t.Fatalf("replacement changed: %q, %v", got, err)
		}
		_ = staged.Close()
	})

	t.Run("replaced as commit becomes visible", func(t *testing.T) {
		store, _ := NewStore(t.TempDir())
		staged, err := store.CreateStaged(t.TempDir(), "test/local/child", "rev")
		if err != nil {
			t.Fatal(err)
		}
		path := staged.Path()
		ownedPath := path + ".owned"
		replacement := []byte("late replacement must survive")
		swapped := false
		staged.publicationFault = func(step publicationStep) error {
			if step != publicationStepCommitVisible || swapped {
				return nil
			}
			swapped = true
			if err := os.Rename(path, ownedPath); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, replacement, 0o600); err != nil {
				t.Fatal(err)
			}
			return nil
		}
		if err := staged.Publish(); err == nil {
			t.Fatal("Publish committed after its creator log was replaced")
		}
		if !swapped {
			t.Fatal("commit replacement seam did not run")
		}
		markerBytes, err := os.ReadFile(publicationMarkerPath(path))
		if err != nil || string(markerBytes) != publicationMarker(staged.ID(), staged.state.publicationID) {
			t.Fatalf("failed commit did not preserve its marker evidence: %q, %v", markerBytes, err)
		}
		if published, err := PublicationStatus(path); err == nil && published {
			t.Fatal("replacement log became discoverable beside preserved marker evidence")
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != string(replacement) {
			t.Fatalf("late replacement changed: %q, %v", got, err)
		}
		if !staged.PublicationPending() {
			t.Fatal("failed path-bound commit cleared publication pending state")
		}
		_ = staged.Close()
	})

	t.Run("unlinked", func(t *testing.T) {
		store, _ := NewStore(t.TempDir())
		staged, err := store.CreateStaged(t.TempDir(), "test/local/child", "rev")
		if err != nil {
			t.Fatal(err)
		}
		path := staged.Path()
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := staged.Publish(); err == nil {
			t.Fatal("Publish accepted an unlinked creator log")
		}
		if _, err := os.Lstat(publicationMarkerPath(path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unlinked Publish left marker: %v", err)
		}
		_ = staged.Close()
	})
}

func TestCloseDiscardingStagedDoesNotDeletePathReplacement(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	staged, err := store.CreateStaged(t.TempDir(), "test/local/child", "rev")
	if err != nil {
		t.Fatal(err)
	}
	path := staged.Path()
	ownedPath := path + ".owned"
	replacement := []byte("do not delete this replacement")
	staged.discardBeforeClose = func(string) {
		if err := os.Rename(path, ownedPath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, replacement, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := staged.CloseDiscardingStaged(); err == nil {
		t.Fatal("discard reported success after its path was replaced")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(replacement) {
		t.Fatalf("discard deleted or changed replacement: %q, %v", got, err)
	}
	if _, err := os.Stat(ownedPath); err != nil {
		t.Fatalf("owned staged evidence disappeared: %v", err)
	}
}

func TestCloseDiscardingStagedPreservesCommitThatCompletesDuringCleanup(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	staged, err := store.CreateStaged(t.TempDir(), "test/local/child", "rev")
	if err != nil {
		t.Fatal(err)
	}
	path := staged.Path()
	markerPath := publicationMarkerPath(path)
	markerBytes := []byte(publicationMarker(staged.ID(), staged.state.publicationID))
	hookCalls := 0
	staged.discardBeforeInspect = func(string) {
		hookCalls++
		if err := os.WriteFile(markerPath, markerBytes, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := staged.CloseDiscardingStaged(); err != nil {
		t.Fatalf("discard beside newly committed marker: %v", err)
	}
	if hookCalls != 1 {
		t.Fatalf("marker inspection hook calls = %d, want 1", hookCalls)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("discard removed newly committed session log: %v", err)
	}
	if published, err := PublicationStatus(path); err != nil || !published {
		t.Fatalf("newly completed publication status = %v, %v; want committed", published, err)
	}
}

func TestCloseDiscardingStagedRecognizesCommitAtCloseSeam(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	staged, err := store.CreateStaged(t.TempDir(), "test/local/child", "rev")
	if err != nil {
		t.Fatal(err)
	}
	path := staged.Path()
	markerPath := publicationMarkerPath(path)
	markerBytes := []byte(publicationMarker(staged.ID(), staged.state.publicationID))
	hookCalls := 0
	staged.discardBeforeClose = func(string) {
		hookCalls++
		if err := os.WriteFile(markerPath, markerBytes, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := staged.CloseDiscardingStaged(); err != nil {
		t.Fatalf("discard beside commit completed at removal seam: %v", err)
	}
	if hookCalls != 1 {
		t.Fatalf("removal hook calls = %d, want 1", hookCalls)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("discard removed session log committed at removal seam: %v", err)
	}
	if published, err := PublicationStatus(path); err != nil || !published {
		t.Fatalf("removal-seam publication status = %v, %v; want committed", published, err)
	}
}

func TestCloseDiscardingStagedRefusesForeignMarkerWithoutRemovingLog(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	staged, err := store.CreateStaged(t.TempDir(), "test/local/child", "rev")
	if err != nil {
		t.Fatal(err)
	}
	path := staged.Path()
	markerPath := publicationMarkerPath(path)
	foreign := []byte("foreign publication marker\n")
	if err := os.WriteFile(markerPath, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := staged.CloseDiscardingStaged(); err == nil {
		t.Fatal("discard accepted a foreign publication marker")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("discard removed log beside foreign marker: %v", err)
	}
	if got, err := os.ReadFile(markerPath); err != nil || !bytes.Equal(got, foreign) {
		t.Fatalf("discard changed foreign marker = %q, %v", got, err)
	}
}

func TestStagedMaintenanceDoesNotDeletePathOrMarkerReplacements(t *testing.T) {
	t.Run("commit observed before removal", func(t *testing.T) {
		root := t.TempDir()
		store, _ := NewStore(root)
		staged, err := store.CreateStaged(t.TempDir(), "test/local/child", "rev")
		if err != nil {
			t.Fatal(err)
		}
		path := staged.Path()
		markerPath := publicationMarkerPath(path)
		markerBytes := []byte(publicationMarker(staged.ID(), staged.state.publicationID))
		_ = staged.Close()
		old := time.Now().Add(-stagedRetention - time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		hookCalls := 0
		store.maintenanceBeforeRemove = func(string) {
			hookCalls++
			if err := os.WriteFile(markerPath, markerBytes, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		report := store.cleanupExpiredStaged(time.Now(), stagedCleanupLimit)
		if hookCalls != 1 || report.Removed != 0 || report.Refused != 1 {
			t.Fatalf("maintenance report = %#v, hook calls %d; want one observed refusal", report, hookCalls)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("maintenance removed newly committed log: %v", err)
		}
		if published, err := PublicationStatus(path); err != nil || !published {
			t.Fatalf("new maintenance-seam publication = %v, %v; want visible", published, err)
		}
	})

	t.Run("log replacement", func(t *testing.T) {
		root := t.TempDir()
		store, _ := NewStore(root)
		staged, err := store.CreateStaged(t.TempDir(), "test/local/child", "rev")
		if err != nil {
			t.Fatal(err)
		}
		path := staged.Path()
		ownedPath := path + ".owned"
		_ = staged.Close()
		old := time.Now().Add(-stagedRetention - time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		replacement := []byte("maintenance replacement")
		store.maintenanceBeforeRemove = func(string) {
			if err := os.Rename(path, ownedPath); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, replacement, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		report := store.cleanupExpiredStaged(time.Now(), stagedCleanupLimit)
		if report.Removed != 0 || report.Failed != 1 {
			t.Fatalf("maintenance report = %#v", report)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != string(replacement) {
			t.Fatalf("maintenance deleted or changed replacement: %q, %v", got, err)
		}
		if _, err := os.Stat(ownedPath); err != nil {
			t.Fatalf("owned staged evidence disappeared: %v", err)
		}
	})

	t.Run("marker replacement", func(t *testing.T) {
		root := t.TempDir()
		store, _ := NewStore(root)
		staged, err := store.CreateStaged(t.TempDir(), "test/local/child", "rev")
		if err != nil {
			t.Fatal(err)
		}
		path := staged.Path()
		markerPath := publicationMarkerPath(path)
		marker := publicationMarker(staged.ID(), staged.state.publicationID)
		if err := os.WriteFile(markerPath, []byte(marker[:len(marker)-1]), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = staged.Close()
		old := time.Now().Add(-stagedRetention - time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		ownedMarker := markerPath + ".owned"
		foreign := []byte("foreign marker replacement\n")
		store.maintenanceBeforeMarkerRemove = func(string) {
			if err := os.Rename(markerPath, ownedMarker); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(markerPath, foreign, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		report := store.cleanupExpiredStaged(time.Now(), stagedCleanupLimit)
		if report.Removed != 1 || report.Failed != 1 {
			t.Fatalf("maintenance report = %#v", report)
		}
		if got, err := os.ReadFile(markerPath); err != nil || string(got) != string(foreign) {
			t.Fatalf("maintenance deleted or changed marker replacement: %q, %v", got, err)
		}
		if _, err := os.Stat(ownedMarker); err != nil {
			t.Fatalf("owned partial marker disappeared: %v", err)
		}
	})
}

func TestPublicationStatusDistinguishesCommittedPartialAndForeignMarkers(t *testing.T) {
	t.Run("regular", func(t *testing.T) {
		store, _ := NewStore(t.TempDir())
		sess, err := store.Create(t.TempDir(), "test/local/regular", "rev")
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		if published, err := PublicationStatus(sess.Path()); err != nil || !published {
			t.Fatalf("PublicationStatus = %v, %v; want published", published, err)
		}
	})

	t.Run("absent and owned partial", func(t *testing.T) {
		store, _ := NewStore(t.TempDir())
		sess, err := store.CreateStaged(t.TempDir(), "test/local/staged", "rev")
		if err != nil {
			t.Fatal(err)
		}
		defer sess.CloseDiscardingStaged()
		if published, err := PublicationStatus(sess.Path()); err != nil || published {
			t.Fatalf("absent marker status = %v, %v; want unpublished", published, err)
		}
		marker := publicationMarker(sess.ID(), sess.state.publicationID)
		if err := os.WriteFile(publicationMarkerPath(sess.Path()), []byte(marker[:len(marker)/2]), 0o600); err != nil {
			t.Fatal(err)
		}
		if published, err := PublicationStatus(sess.Path()); err != nil || published {
			t.Fatalf("partial marker status = %v, %v; want unpublished", published, err)
		}
	})

	t.Run("committed", func(t *testing.T) {
		store, _ := NewStore(t.TempDir())
		sess, err := store.CreateStaged(t.TempDir(), "test/local/staged", "rev")
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		if err := sess.Publish(); err != nil {
			t.Fatal(err)
		}
		if published, err := PublicationStatus(sess.Path()); err != nil || !published {
			t.Fatalf("committed marker status = %v, %v; want published", published, err)
		}
	})

	t.Run("foreign", func(t *testing.T) {
		store, _ := NewStore(t.TempDir())
		sess, err := store.CreateStaged(t.TempDir(), "test/local/staged", "rev")
		if err != nil {
			t.Fatal(err)
		}
		defer sess.CloseDiscardingStaged()
		if err := os.WriteFile(publicationMarkerPath(sess.Path()), []byte("foreign\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if published, err := PublicationStatus(sess.Path()); err == nil || published {
			t.Fatalf("foreign marker status = %v, %v; want error", published, err)
		}
	})
}

func TestPublicationRecoveryFlushesVisibleMarkerBeforeDeclaringCommit(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.CreateStaged(t.TempDir(), "test/local/staged", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	identity, err := sess.PublicationRecoveryIdentity()
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := sess.PublishDurably()
	if err != nil || !outcome.Visible || !outcome.Durable {
		t.Fatalf("publishing fixture = %+v, %v", outcome, err)
	}

	injectedMarker := errors.New("injected marker sync failure")
	published, err := ensurePublicationDurableExpected(sess.Path(), identity,
		func(*os.File) error { return injectedMarker },
		func(*os.File) error { t.Fatal("directory sync ran after marker sync failed"); return nil })
	if published || !errors.Is(err, injectedMarker) {
		t.Fatalf("marker-sync recovery = %v, %v", published, err)
	}

	injectedDirectory := errors.New("injected directory sync failure")
	published, err = ensurePublicationDurableExpected(sess.Path(), identity,
		func(*os.File) error { return nil },
		func(*os.File) error { return injectedDirectory })
	if published || !errors.Is(err, injectedDirectory) {
		t.Fatalf("directory-sync recovery = %v, %v", published, err)
	}

	published, err = EnsurePublicationDurableExpected(sess.Path(), identity)
	if err != nil || !published {
		t.Fatalf("durable recovery = %v, %v; want committed", published, err)
	}
}

func TestPublicationRecoveryRejectsMarkerReplacementAfterPersistenceBarrier(t *testing.T) {
	tests := []struct {
		name             string
		replaceAfterSync bool
	}{
		{name: "marker sync", replaceAfterSync: true},
		{name: "directory sync"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			sess, err := store.CreateStaged(t.TempDir(), "test/local/staged", "rev")
			if err != nil {
				t.Fatal(err)
			}
			defer sess.Close()
			identity, err := sess.PublicationRecoveryIdentity()
			if err != nil {
				t.Fatal(err)
			}
			if outcome, err := sess.PublishDurably(); err != nil || !outcome.Visible || !outcome.Durable {
				t.Fatalf("publishing fixture = %+v, %v", outcome, err)
			}

			markerPath := publicationMarkerPath(sess.Path())
			markerBytes, err := os.ReadFile(markerPath)
			if err != nil {
				t.Fatal(err)
			}
			movedPath := markerPath + ".synced"
			replaced := false
			replace := func() error {
				if replaced {
					return nil
				}
				replaced = true
				if err := os.Rename(markerPath, movedPath); err != nil {
					return err
				}
				return os.WriteFile(markerPath, markerBytes, 0o600)
			}
			directorySyncs := 0
			published, recoveryErr := ensurePublicationDurableExpected(sess.Path(), identity,
				func(marker *os.File) error {
					if err := marker.Sync(); err != nil {
						return err
					}
					if tt.replaceAfterSync {
						return replace()
					}
					return nil
				},
				func(directory *os.File) error {
					directorySyncs++
					if err := syncOpenedSessionDirectory(directory); err != nil {
						return err
					}
					if !tt.replaceAfterSync {
						return replace()
					}
					return nil
				})
			if recoveryErr == nil || published {
				t.Fatalf("replacement recovery = %v, %v; want refusal", published, recoveryErr)
			}
			if !replaced {
				t.Fatal("replacement seam did not run")
			}
			if tt.replaceAfterSync && directorySyncs != 0 {
				t.Fatalf("directory syncs after marker replacement = %d, want 0", directorySyncs)
			}
			if got, err := os.ReadFile(markerPath); err != nil || !bytes.Equal(got, markerBytes) {
				t.Fatalf("exact replacement changed = %q, %v", got, err)
			}

			published, err = EnsurePublicationDurableExpected(sess.Path(), identity)
			if err != nil || !published {
				t.Fatalf("stable exact replacement recovery = %v, %v; want committed", published, err)
			}
		})
	}
}

func TestPublicationRecoveryRejectsInPlaceMutationAfterMarkerSync(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.CreateStaged(t.TempDir(), "test/local/staged", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	identity, err := sess.PublicationRecoveryIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := sess.PublishDurably(); err != nil || !outcome.Visible || !outcome.Durable {
		t.Fatalf("publishing fixture = %+v, %v", outcome, err)
	}
	markerPath := publicationMarkerPath(sess.Path())
	markerBytes, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	directorySyncs := 0
	published, recoveryErr := ensurePublicationDurableExpected(sess.Path(), identity,
		func(marker *os.File) error {
			if err := marker.Sync(); err != nil {
				return err
			}
			return os.WriteFile(markerPath, markerBytes[:len(markerBytes)-1], 0o600)
		},
		func(*os.File) error {
			directorySyncs++
			return nil
		})
	if recoveryErr == nil || published {
		t.Fatalf("in-place mutation recovery = %v, %v; want refusal", published, recoveryErr)
	}
	if directorySyncs != 0 {
		t.Fatalf("directory syncs after in-place marker mutation = %d, want 0", directorySyncs)
	}
	after, err := os.Stat(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("mutation test replaced the marker instead of changing its opened inode")
	}
	if err := os.WriteFile(markerPath, markerBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	published, err = EnsurePublicationDurableExpected(sess.Path(), identity)
	if err != nil || !published {
		t.Fatalf("stable restored-marker recovery = %v, %v; want durable", published, err)
	}
}

func TestPublicationRecoveryRejectsExactInPlaceRewriteAcrossPersistenceBarriers(t *testing.T) {
	for _, tt := range []struct {
		name             string
		rewriteAfterSync bool
	}{
		{name: "after marker sync", rewriteAfterSync: true},
		{name: "after directory sync"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			sess, err := store.CreateStaged(t.TempDir(), "test/local/staged", "rev")
			if err != nil {
				t.Fatal(err)
			}
			defer sess.Close()
			identity, err := sess.PublicationRecoveryIdentity()
			if err != nil {
				t.Fatal(err)
			}
			if outcome, err := sess.PublishDurably(); err != nil || !outcome.Visible || !outcome.Durable {
				t.Fatalf("publishing fixture = %+v, %v", outcome, err)
			}
			markerPath := publicationMarkerPath(sess.Path())
			markerBytes, err := os.ReadFile(markerPath)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(markerPath)
			if err != nil {
				t.Fatal(err)
			}
			rewritten := false
			rewrite := func() error {
				if rewritten {
					return nil
				}
				rewritten = true
				return os.WriteFile(markerPath, markerBytes, 0o600)
			}
			directorySyncs := 0
			published, recoveryErr := ensurePublicationDurableExpected(sess.Path(), identity,
				func(marker *os.File) error {
					if err := marker.Sync(); err != nil {
						return err
					}
					if tt.rewriteAfterSync {
						return rewrite()
					}
					return nil
				},
				func(directory *os.File) error {
					directorySyncs++
					if err := syncOpenedSessionDirectory(directory); err != nil {
						return err
					}
					if !tt.rewriteAfterSync {
						return rewrite()
					}
					return nil
				})
			if recoveryErr == nil || published {
				t.Fatalf("exact in-place rewrite recovery = %v, %v; want refusal", published, recoveryErr)
			}
			if !rewritten {
				t.Fatal("exact rewrite seam did not run")
			}
			if tt.rewriteAfterSync && directorySyncs != 0 {
				t.Fatalf("directory syncs after exact marker rewrite = %d, want 0", directorySyncs)
			}
			after, err := os.Stat(markerPath)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) {
				t.Fatal("exact rewrite test replaced the marker inode")
			}
			if got, err := os.ReadFile(markerPath); err != nil || !bytes.Equal(got, markerBytes) {
				t.Fatalf("exact rewrite changed marker bytes = %q, %v", got, err)
			}
			published, err = EnsurePublicationDurableExpected(sess.Path(), identity)
			if err != nil || !published {
				t.Fatalf("stable exact-rewrite recovery = %v, %v; want durable", published, err)
			}
		})
	}
}

func TestPublicationRecoveryRejectsDirectoryEntryABAAfterDirectorySync(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.CreateStaged(t.TempDir(), "test/local/staged", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	identity, err := sess.PublicationRecoveryIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := sess.PublishDurably(); err != nil || !outcome.Visible || !outcome.Durable {
		t.Fatalf("publishing fixture = %+v, %v", outcome, err)
	}
	markerPath := publicationMarkerPath(sess.Path())
	markerBytes, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	published, recoveryErr := ensurePublicationDurableExpected(sess.Path(), identity,
		func(marker *os.File) error { return marker.Sync() },
		func(directory *os.File) error {
			return publicationMarkerDirectoryABA(markerPath, markerBytes,
				func() error { return syncOpenedSessionDirectory(directory) })
		})
	if recoveryErr == nil || published {
		t.Fatalf("directory-ABA recovery = %v, %v; want refusal", published, recoveryErr)
	}
	if got, err := os.ReadFile(markerPath); err != nil || !bytes.Equal(got, markerBytes) {
		t.Fatalf("directory ABA did not restore original marker = %q, %v", got, err)
	}
	published, err = EnsurePublicationDurableExpected(sess.Path(), identity)
	if err != nil || !published {
		t.Fatalf("stable directory-ABA recovery = %v, %v; want durable", published, err)
	}
}

func TestPublicationRecoveryDoesNotReportMissingWhenMarkerAppearsAfterRootedOpen(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	staged, err := store.CreateStaged(t.TempDir(), "test/local/staged", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	identity, err := staged.PublicationRecoveryIdentity()
	if err != nil {
		t.Fatal(err)
	}
	markerPath := publicationMarkerPath(staged.Path())
	markerBytes := []byte(publicationMarker(staged.ID(), staged.state.publicationID))
	afterCalls := 0
	published, recoveryErr := ensurePublicationDurableExpectedWithHooks(staged.Path(), identity,
		func(*os.File) error { t.Fatal("marker sync ran after the rooted open reported absence"); return nil },
		func(*os.File) error { t.Fatal("directory sync ran after the rooted open reported absence"); return nil },
		nil,
		func() {
			afterCalls++
			if err := os.WriteFile(markerPath, markerBytes, 0o600); err != nil {
				t.Fatal(err)
			}
		})
	if recoveryErr == nil || published {
		t.Fatalf("marker-appearance recovery = %v, %v; want refusal, not unpublished", published, recoveryErr)
	}
	if afterCalls != 1 {
		t.Fatalf("after-open hook calls = %d, want 1", afterCalls)
	}
}

func TestPublishDurablyRejectsMarkerIdentityChangesAcrossPersistenceBarriers(t *testing.T) {
	tests := []struct {
		name          string
		step          publicationStep
		exactReplace  bool
		visible       bool
		movedOriginal bool
	}{
		{name: "unlink before directory sync", step: publicationStepDirectorySync},
		{name: "exact replacement before directory sync", step: publicationStepDirectorySync, exactReplace: true, visible: true, movedOriginal: true},
		{name: "exact replacement after directory sync", step: publicationStepClose, exactReplace: true, visible: true, movedOriginal: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			staged, err := store.CreateStaged(t.TempDir(), "test/local/staged", "rev")
			if err != nil {
				t.Fatal(err)
			}
			markerPath := publicationMarkerPath(staged.Path())
			movedPath := markerPath + ".synced"
			markerBytes := []byte(publicationMarker(staged.ID(), staged.state.publicationID))
			mutated := false
			var mutationErr error
			staged.publicationFault = func(step publicationStep) error {
				if step != tt.step || mutated {
					return nil
				}
				mutated = true
				if tt.exactReplace {
					mutationErr = os.Rename(markerPath, movedPath)
					if mutationErr == nil {
						mutationErr = os.WriteFile(markerPath, markerBytes, 0o600)
					}
				} else {
					mutationErr = os.Remove(markerPath)
				}
				return mutationErr
			}

			outcome, publishErr := staged.PublishDurably()
			if mutationErr != nil {
				t.Fatal(mutationErr)
			}
			if !mutated {
				t.Fatal("marker identity mutation seam did not run")
			}
			if publishErr == nil || outcome.Visible != tt.visible || outcome.Durable {
				t.Fatalf("PublishDurably() = %+v, %v; want visible=%v durable=false error", outcome, publishErr, tt.visible)
			}
			if tt.movedOriginal {
				if got, err := os.ReadFile(movedPath); err != nil || !bytes.Equal(got, markerBytes) {
					t.Fatalf("synced original marker = %q, %v", got, err)
				}
				if got, err := os.ReadFile(markerPath); err != nil || !bytes.Equal(got, markerBytes) {
					t.Fatalf("unsynced exact replacement = %q, %v", got, err)
				}
			}

			staged.publicationFault = nil
			outcome, err = staged.PublishDurably()
			if err != nil || !outcome.Visible || !outcome.Durable {
				t.Fatalf("stable retry = %+v, %v; want durable commit", outcome, err)
			}
			if err := staged.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPublishDurablyRejectsInPlaceMarkerMutationAcrossPersistenceBarriers(t *testing.T) {
	for _, tt := range []struct {
		name string
		step publicationStep
	}{
		{name: "before directory sync", step: publicationStepDirectorySync},
		{name: "after directory sync", step: publicationStepClose},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			staged, err := store.CreateStaged(t.TempDir(), "test/local/staged", "rev")
			if err != nil {
				t.Fatal(err)
			}
			markerPath := publicationMarkerPath(staged.Path())
			markerBytes := []byte(publicationMarker(staged.ID(), staged.state.publicationID))
			mutated := false
			var before, after os.FileInfo
			var mutationErr error
			staged.publicationFault = func(step publicationStep) error {
				if step != tt.step || mutated {
					return nil
				}
				mutated = true
				before, mutationErr = os.Stat(markerPath)
				if mutationErr == nil {
					mutationErr = os.WriteFile(markerPath, markerBytes[:len(markerBytes)-1], 0o600)
				}
				if mutationErr == nil {
					after, mutationErr = os.Stat(markerPath)
				}
				return mutationErr
			}
			outcome, publishErr := staged.PublishDurably()
			if mutationErr != nil {
				t.Fatal(mutationErr)
			}
			if !mutated || !os.SameFile(before, after) {
				t.Fatal("mutation seam did not change the existing marker inode")
			}
			if publishErr == nil || outcome.Visible || outcome.Durable {
				t.Fatalf("in-place marker mutation PublishDurably = %+v, %v; want invisible non-durable refusal", outcome, publishErr)
			}
			staged.publicationFault = nil
			outcome, err = staged.PublishDurably()
			if err != nil || !outcome.Visible || !outcome.Durable {
				t.Fatalf("stable retry = %+v, %v; want durable", outcome, err)
			}
			if err := staged.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPublishDurablyRejectsExactInPlaceRewriteAcrossPersistenceBarriers(t *testing.T) {
	for _, tt := range []struct {
		name string
		step publicationStep
	}{
		{name: "before directory sync", step: publicationStepDirectorySync},
		{name: "after directory sync", step: publicationStepClose},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			staged, err := store.CreateStaged(t.TempDir(), "test/local/staged", "rev")
			if err != nil {
				t.Fatal(err)
			}
			markerPath := publicationMarkerPath(staged.Path())
			markerBytes := []byte(publicationMarker(staged.ID(), staged.state.publicationID))
			rewritten := false
			var before, after os.FileInfo
			var rewriteErr error
			staged.publicationFault = func(step publicationStep) error {
				if step != tt.step || rewritten {
					return nil
				}
				rewritten = true
				before, rewriteErr = os.Stat(markerPath)
				if rewriteErr == nil {
					rewriteErr = os.WriteFile(markerPath, markerBytes, 0o600)
				}
				if rewriteErr == nil {
					after, rewriteErr = os.Stat(markerPath)
				}
				return rewriteErr
			}
			outcome, publishErr := staged.PublishDurably()
			if rewriteErr != nil {
				t.Fatal(rewriteErr)
			}
			if !rewritten || !os.SameFile(before, after) {
				t.Fatal("exact rewrite seam did not change the existing marker inode")
			}
			if publishErr == nil || !outcome.Visible || outcome.Durable {
				t.Fatalf("exact in-place rewrite PublishDurably = %+v, %v; want visible non-durable refusal", outcome, publishErr)
			}
			if got, err := os.ReadFile(markerPath); err != nil || !bytes.Equal(got, markerBytes) {
				t.Fatalf("exact rewrite changed marker bytes = %q, %v", got, err)
			}
			staged.publicationFault = nil
			outcome, err = staged.PublishDurably()
			if err != nil || !outcome.Visible || !outcome.Durable {
				t.Fatalf("stable retry = %+v, %v; want durable", outcome, err)
			}
			if err := staged.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPublishDurablyRejectsDirectoryEntryABAAfterDirectorySync(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	staged, err := store.CreateStaged(t.TempDir(), "test/local/staged", "rev")
	if err != nil {
		t.Fatal(err)
	}
	markerPath := publicationMarkerPath(staged.Path())
	markerBytes := []byte(publicationMarker(staged.ID(), staged.state.publicationID))
	abaRan := false
	var abaErr error
	staged.publicationFault = func(step publicationStep) error {
		if step != publicationStepClose || abaRan {
			return nil
		}
		abaRan = true
		abaErr = publicationMarkerDirectoryABA(markerPath, markerBytes,
			func() error { return syncSessionDirectory(filepath.Dir(markerPath)) })
		return abaErr
	}
	outcome, publishErr := staged.PublishDurably()
	if abaErr != nil {
		t.Fatal(abaErr)
	}
	if !abaRan {
		t.Fatal("directory ABA seam did not run")
	}
	if publishErr == nil || !outcome.Visible || outcome.Durable {
		t.Fatalf("directory-ABA PublishDurably = %+v, %v; want visible non-durable refusal", outcome, publishErr)
	}
	if got, err := os.ReadFile(markerPath); err != nil || !bytes.Equal(got, markerBytes) {
		t.Fatalf("directory ABA did not restore original marker = %q, %v", got, err)
	}
	staged.publicationFault = nil
	outcome, err = staged.PublishDurably()
	if err != nil || !outcome.Visible || !outcome.Durable {
		t.Fatalf("stable directory-ABA retry = %+v, %v; want durable", outcome, err)
	}
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestVisiblePublicationRetryRejectsDirectoryEntryABAAfterDirectorySync(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	staged, err := store.CreateStaged(t.TempDir(), "test/local/staged", "rev")
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("leave publication visible but not durable")
	staged.publicationFault = func(step publicationStep) error {
		if step == publicationStepDirectorySync {
			return injected
		}
		return nil
	}
	outcome, err := staged.PublishDurably()
	if !errors.Is(err, injected) || !outcome.Visible || outcome.Durable {
		t.Fatalf("visible fixture = %+v, %v", outcome, err)
	}
	staged.publicationFault = nil
	markerPath := publicationMarkerPath(staged.Path())
	markerBytes, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	start := SessionStart{ID: staged.state.ID, Staged: true, PublicationID: staged.state.publicationID}
	staged.mu.Lock()
	outcome, retryErr := staged.ensureVisiblePublicationDurableLockedWith(start,
		func(marker *os.File) error { return marker.Sync() },
		func(directory *os.File) error {
			return publicationMarkerDirectoryABA(markerPath, markerBytes,
				func() error { return syncOpenedSessionDirectory(directory) })
		})
	staged.mu.Unlock()
	if retryErr == nil || !outcome.Visible || outcome.Durable {
		t.Fatalf("directory-ABA visible retry = %+v, %v; want visible non-durable refusal", outcome, retryErr)
	}
	outcome, err = staged.PublishDurably()
	if err != nil || !outcome.Visible || !outcome.Durable {
		t.Fatalf("stable visible retry = %+v, %v; want durable", outcome, err)
	}
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationFailurePreservesCommitCompletedAfterInitialVisibilityCheck(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	staged, err := store.CreateStaged(t.TempDir(), "test/local/staged", "rev")
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("fail before commit byte")
	staged.publicationFault = func(step publicationStep) error {
		if step == publicationStepCommitWrite {
			return injected
		}
		return nil
	}
	hookCalls := 0
	staged.publicationFailCheck = func(markerPath string) {
		hookCalls++
		markerBytes := []byte(publicationMarker(staged.ID(), staged.state.publicationID))
		if err := os.WriteFile(markerPath, markerBytes, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	outcome, publishErr := staged.PublishDurably()
	if !errors.Is(publishErr, injected) || !outcome.Visible || outcome.Durable {
		t.Fatalf("late failure commit = %+v, %v; want visible non-durable", outcome, publishErr)
	}
	if hookCalls != 1 {
		t.Fatalf("failure inspection hook calls = %d, want 1", hookCalls)
	}
	if _, err := os.Stat(staged.Path()); err != nil {
		t.Fatalf("failure cleanup removed committed log: %v", err)
	}
	if published, err := PublicationStatus(staged.Path()); err != nil || !published {
		t.Fatalf("late failure commit status = %v, %v; want visible", published, err)
	}
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestVisiblePublicationRetryRejectsExactReplacementWhoseDescriptorWasNotSynced(t *testing.T) {
	tests := []struct {
		name             string
		replaceAfterSync bool
	}{
		{name: "after marker sync", replaceAfterSync: true},
		{name: "after directory sync"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			staged, err := store.CreateStaged(t.TempDir(), "test/local/staged", "rev")
			if err != nil {
				t.Fatal(err)
			}
			injected := errors.New("leave publication visible but not durable")
			staged.publicationFault = func(step publicationStep) error {
				if step == publicationStepDirectorySync {
					return injected
				}
				return nil
			}
			outcome, err := staged.PublishDurably()
			if !errors.Is(err, injected) || !outcome.Visible || outcome.Durable {
				t.Fatalf("visible fixture = %+v, %v", outcome, err)
			}
			staged.publicationFault = nil

			markerPath := publicationMarkerPath(staged.Path())
			markerBytes, err := os.ReadFile(markerPath)
			if err != nil {
				t.Fatal(err)
			}
			movedPath := markerPath + ".synced"
			replaced := false
			replace := func() error {
				if replaced {
					return nil
				}
				replaced = true
				if err := os.Rename(markerPath, movedPath); err != nil {
					return err
				}
				return os.WriteFile(markerPath, markerBytes, 0o600)
			}
			directorySyncs := 0
			start := SessionStart{ID: staged.state.ID, Staged: true, PublicationID: staged.state.publicationID}
			staged.mu.Lock()
			outcome, retryErr := staged.ensureVisiblePublicationDurableLockedWith(start,
				func(marker *os.File) error {
					if err := marker.Sync(); err != nil {
						return err
					}
					if tt.replaceAfterSync {
						return replace()
					}
					return nil
				},
				func(directory *os.File) error {
					directorySyncs++
					if err := syncOpenedSessionDirectory(directory); err != nil {
						return err
					}
					if !tt.replaceAfterSync {
						return replace()
					}
					return nil
				})
			staged.mu.Unlock()
			if retryErr == nil || !outcome.Visible || outcome.Durable {
				t.Fatalf("identity-raced retry = %+v, %v; want visible non-durable refusal", outcome, retryErr)
			}
			if !replaced {
				t.Fatal("replacement seam did not run")
			}
			if tt.replaceAfterSync && directorySyncs != 0 {
				t.Fatalf("directory syncs after marker replacement = %d, want 0", directorySyncs)
			}
			if staged.publicationDurable {
				t.Fatal("identity-raced retry marked the unsynced replacement durable")
			}

			outcome, err = staged.PublishDurably()
			if err != nil || !outcome.Visible || !outcome.Durable {
				t.Fatalf("stable retry = %+v, %v; want durable commit", outcome, err)
			}
			if err := staged.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPublishFaultBoundariesNeverReturnErrorWithValidMarker(t *testing.T) {
	tests := []struct {
		name      string
		step      publicationStep
		committed bool
	}{
		{"open", publicationStepOpen, false},
		{"prefix write", publicationStepPrefixWrite, false},
		{"prefix sync", publicationStepPrefixSync, false},
		{"commit write", publicationStepCommitWrite, false},
		{"commit visible", publicationStepCommitVisible, true},
		{"commit sync", publicationStepCommitSync, true},
		{"close", publicationStepClose, true},
		{"directory sync", publicationStepDirectorySync, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			workspace := t.TempDir()
			staged, err := store.CreateStaged(workspace, "test/local/child", "rev")
			if err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected publication fault")
			staged.publicationFault = func(step publicationStep) error {
				if step == tt.step {
					return injected
				}
				return nil
			}
			err = staged.Publish()
			start := SessionStart{ID: staged.ID(), Staged: true, PublicationID: staged.state.publicationID}
			visible := validatePublishedMarker(staged.Path(), start) == nil
			if (err == nil) != tt.committed {
				t.Fatalf("Publish error = %v, committed want %v", err, tt.committed)
			}
			if visible != tt.committed {
				t.Fatalf("marker visible = %v, committed want %v", visible, tt.committed)
			}
			if err != nil && visible {
				t.Fatal("Publish returned an error beside a valid marker")
			}
			if tt.committed {
				if staged.PublicationPending() {
					t.Fatal("committed publication remained pending")
				}
				_ = staged.Close()
			} else {
				assertStagedHidden(t, store, workspace, staged.ID(), "")
				_ = staged.CloseDiscardingStaged()
			}
		})
	}
}

func TestDiscoveryCannotSeeMarkerPrefixDuringPublish(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	staged, err := store.CreateStaged(workspace, "test/local/child", "rev")
	if err != nil {
		t.Fatal(err)
	}
	reached := make(chan struct{})
	proceed := make(chan struct{})
	staged.publicationFault = func(step publicationStep) error {
		if step == publicationStepCommitWrite {
			close(reached)
			<-proceed
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- staged.Publish() }()
	<-reached
	assertStagedHidden(t, store, workspace, staged.ID(), "")
	close(proceed)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	infos, err := store.List(workspace)
	if err != nil || len(infos) != 1 || infos[0].ID != staged.ID() {
		t.Fatalf("post-commit List = %#v, %v", infos, err)
	}
	_ = staged.Close()
}

func TestEveryExportedPathReaderRejectsUnpublishedSession(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	staged, err := store.CreateStaged(t.TempDir(), "test/local/child", "rev")
	if err != nil {
		t.Fatal(err)
	}
	path := staged.Path()
	readers := []struct {
		name string
		read func() error
	}{
		{"ReadState", func() error { _, err := ReadState(path); return err }},
		{"ReadRaces", func() error { _, err := ReadRaces(path); return err }},
		{"ReadPermissions", func() error { _, err := ReadPermissions(path); return err }},
		{"ReadTimeline", func() error { _, err := ReadTimeline(path); return err }},
		{"ReadUsages", func() error { _, err := ReadUsages(path); return err }},
		{"ReadWorkspace", func() error { _, err := ReadWorkspace(path); return err }},
		{"ReadOpening", func() error { _, err := ReadOpening(path); return err }},
		{"ReadOpeningSummary", func() error { _, err := ReadOpeningSummary(path); return err }},
		{"ReadFileEdits", func() error { _, err := ReadFileEdits(path); return err }},
		{"ReadTurnCosts", func() error { _, err := ReadTurnCosts(path); return err }},
		{"ReadAccountingLedger", func() error { _, err := ReadAccountingLedger(path); return err }},
	}
	for _, reader := range readers {
		t.Run(reader.name, func(t *testing.T) {
			if err := reader.read(); !errors.Is(err, ErrSessionUnpublished) {
				t.Fatalf("error = %v, want ErrSessionUnpublished", err)
			}
		})
	}
	_ = staged.CloseDiscardingStaged()
}

func TestLiveForkCannotPublishHistoryFromAnUnpublishedSource(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	staged, err := store.CreateStaged(t.TempDir(), "test/local/child", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := staged.AppendMessage(provider.UserText("hidden opening")); err != nil {
		t.Fatal(err)
	}
	if err := staged.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "hidden answer"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ForkSession(staged, 2); !errors.Is(err, ErrSessionUnpublished) {
		t.Fatalf("ForkSession error = %v, want ErrSessionUnpublished", err)
	}
	_ = staged.CloseDiscardingStaged()
}

func TestUnpublishedGatePrecedesInvalidLaterPayload(t *testing.T) {
	tests := []struct {
		name string
		body func(t *testing.T) []byte
	}{
		{
			name: "invalid json frame",
			body: func(*testing.T) []byte {
				body := []byte(`{"seq":2,"at":"2026-01-01T00:00:00Z","type":"usage","payload":{`)
				return []byte(fmt.Sprintf("%08x %08x %s\n", len(body), crc32.Checksum(body, crcTable), body))
			},
		},
		{
			name: "semantic invalid payload",
			body: func(t *testing.T) []byte {
				payload, err := json.Marshal(Usage{Usage: provider.Usage{InputTokens: -1}})
				if err != nil {
					t.Fatal(err)
				}
				frame, err := encodeRecord(Record{Seq: 2, At: time.Now().UTC(), Type: RecordUsage, Payload: payload})
				if err != nil {
					t.Fatal(err)
				}
				return frame
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			workspace := t.TempDir()
			staged, err := store.CreateStaged(workspace, "test/local/child", "rev")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := staged.f.Seek(0, io.SeekEnd); err != nil {
				t.Fatal(err)
			}
			if _, err := staged.f.Write(tt.body(t)); err != nil {
				t.Fatal(err)
			}
			if err := staged.f.Sync(); err != nil {
				t.Fatal(err)
			}
			if _, err := store.validateCandidate(staged.Path(), candidateExpectation{id: staged.ID(), workspace: workspace}); !errors.Is(err, ErrSessionUnpublished) {
				t.Fatalf("candidate error = %v, want publication refusal before later payload", err)
			}
			infos, err := store.List(workspace)
			if err != nil || len(infos) != 0 {
				t.Fatalf("List exposed staged invalid tail: %#v, %v", infos, err)
			}
			_ = staged.CloseDiscardingStaged()
		})
	}
}

func TestPublishDurablyRejectsChildLogTruncationBeforeAndAfterCommitCapture(t *testing.T) {
	for _, tt := range []struct {
		name string
		step publicationStep
	}{
		{name: "before marker open", step: publicationStepOpen},
		{name: "after marker sync", step: publicationStepDirectorySync},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			staged, err := store.CreateStaged(t.TempDir(), "test/local/staged", "rev")
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(staged.Path())
			if err != nil {
				t.Fatal(err)
			}
			mutated := false
			var mutationErr error
			staged.publicationFault = func(step publicationStep) error {
				if step != tt.step || mutated {
					return nil
				}
				mutated = true
				mutationErr = os.Truncate(staged.Path(), 0)
				return mutationErr
			}
			outcome, publishErr := staged.PublishDurably()
			if mutationErr != nil {
				t.Fatal(mutationErr)
			}
			if !mutated {
				t.Fatal("child truncation seam did not run")
			}
			if publishErr == nil || !outcome.Visible || outcome.Durable {
				t.Fatalf("truncated-child PublishDurably = %+v, %v; want visible non-durable refusal", outcome, publishErr)
			}
			after, err := os.Stat(staged.Path())
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) || after.Size() != 0 {
				t.Fatalf("child truncation did not preserve the creator inode: before=%v after=%v", before.Size(), after.Size())
			}
			if err := staged.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPublicationRecoveryRejectsChildLogTruncationBeforeAndAfterCommitCapture(t *testing.T) {
	for _, phase := range []string{"before marker open", "after marker sync"} {
		t.Run(phase, func(t *testing.T) {
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			sess, err := store.CreateStaged(t.TempDir(), "test/local/staged", "rev")
			if err != nil {
				t.Fatal(err)
			}
			identity, err := sess.PublicationRecoveryIdentity()
			if err != nil {
				t.Fatal(err)
			}
			if outcome, err := sess.PublishDurably(); err != nil || !outcome.Visible || !outcome.Durable {
				t.Fatalf("publishing fixture = %+v, %v", outcome, err)
			}
			directorySyncs := 0
			truncate := func() {
				if err := os.Truncate(sess.Path(), 0); err != nil {
					t.Fatal(err)
				}
			}
			var beforeOpen func()
			if phase == "before marker open" {
				beforeOpen = truncate
			}
			published, recoveryErr := ensurePublicationDurableExpectedWithHooks(sess.Path(), identity,
				func(marker *os.File) error {
					if err := marker.Sync(); err != nil {
						return err
					}
					if phase == "after marker sync" {
						truncate()
					}
					return nil
				},
				func(*os.File) error {
					directorySyncs++
					return nil
				}, beforeOpen, nil)
			if recoveryErr == nil || published {
				t.Fatalf("truncated-child recovery = %v, %v; want refusal", published, recoveryErr)
			}
			if directorySyncs != 0 {
				t.Fatalf("directory syncs after child truncation = %d, want 0", directorySyncs)
			}
			if err := sess.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestVisiblePublicationRetryRejectsChildLogTruncationBeforeAndAfterCommitCapture(t *testing.T) {
	for _, phase := range []string{"before retry", "after marker sync"} {
		t.Run(phase, func(t *testing.T) {
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			staged, err := store.CreateStaged(t.TempDir(), "test/local/staged", "rev")
			if err != nil {
				t.Fatal(err)
			}
			injected := errors.New("leave publication visible but not durable")
			staged.publicationFault = func(step publicationStep) error {
				if step == publicationStepDirectorySync {
					return injected
				}
				return nil
			}
			outcome, err := staged.PublishDurably()
			if !errors.Is(err, injected) || !outcome.Visible || outcome.Durable {
				t.Fatalf("visible fixture = %+v, %v", outcome, err)
			}
			staged.publicationFault = nil
			if phase == "before retry" {
				if err := os.Truncate(staged.Path(), 0); err != nil {
					t.Fatal(err)
				}
			}
			start := SessionStart{ID: staged.state.ID, Staged: true, PublicationID: staged.state.publicationID}
			directorySyncs := 0
			staged.mu.Lock()
			outcome, retryErr := staged.ensureVisiblePublicationDurableLockedWith(start,
				func(marker *os.File) error {
					if err := marker.Sync(); err != nil {
						return err
					}
					if phase == "after marker sync" {
						return os.Truncate(staged.Path(), 0)
					}
					return nil
				},
				func(*os.File) error {
					directorySyncs++
					return nil
				})
			staged.mu.Unlock()
			if retryErr == nil || !outcome.Visible || outcome.Durable {
				t.Fatalf("truncated-child visible retry = %+v, %v; want visible non-durable refusal", outcome, retryErr)
			}
			if directorySyncs != 0 {
				t.Fatalf("directory syncs after child truncation = %d, want 0", directorySyncs)
			}
			if err := staged.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPublishDurablyRejectsExactChildLogRewriteAfterMarkerSync(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	staged, err := store.CreateStaged(t.TempDir(), "test/local/staged", "rev")
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(staged.Path())
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(staged.Path())
	if err != nil {
		t.Fatal(err)
	}
	rewritten := false
	var rewriteErr error
	staged.publicationFault = func(step publicationStep) error {
		if step != publicationStepDirectorySync || rewritten {
			return nil
		}
		rewritten = true
		rewriteErr = rewriteExactSessionLogPreservingMtime(staged.Path(), original, before)
		return rewriteErr
	}
	outcome, publishErr := staged.PublishDurably()
	if rewriteErr != nil {
		t.Fatal(rewriteErr)
	}
	if !rewritten {
		t.Fatal("exact child rewrite seam did not run")
	}
	if publishErr == nil || !outcome.Visible || outcome.Durable {
		t.Fatalf("exact-child-rewrite PublishDurably = %+v, %v; want visible non-durable refusal", outcome, publishErr)
	}
	assertExactSessionLogRewrite(t, staged.Path(), original, before)
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationRecoveryRejectsExactChildLogRewriteAfterMarkerSync(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.CreateStaged(t.TempDir(), "test/local/staged", "rev")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := sess.PublicationRecoveryIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := sess.PublishDurably(); err != nil || !outcome.Visible || !outcome.Durable {
		t.Fatalf("publishing fixture = %+v, %v", outcome, err)
	}
	original, err := os.ReadFile(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	directorySyncs := 0
	published, recoveryErr := ensurePublicationDurableExpected(sess.Path(), identity,
		func(marker *os.File) error {
			if err := marker.Sync(); err != nil {
				return err
			}
			return rewriteExactSessionLogPreservingMtime(sess.Path(), original, before)
		},
		func(*os.File) error {
			directorySyncs++
			return nil
		})
	if recoveryErr == nil || published {
		t.Fatalf("exact-child-rewrite recovery = %v, %v; want refusal", published, recoveryErr)
	}
	if directorySyncs != 0 {
		t.Fatalf("directory syncs after exact child rewrite = %d, want 0", directorySyncs)
	}
	assertExactSessionLogRewrite(t, sess.Path(), original, before)
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestVisiblePublicationRetryRejectsExactChildLogRewriteAfterMarkerSync(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	staged, err := store.CreateStaged(t.TempDir(), "test/local/staged", "rev")
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("leave publication visible but not durable")
	staged.publicationFault = func(step publicationStep) error {
		if step == publicationStepDirectorySync {
			return injected
		}
		return nil
	}
	outcome, err := staged.PublishDurably()
	if !errors.Is(err, injected) || !outcome.Visible || outcome.Durable {
		t.Fatalf("visible fixture = %+v, %v", outcome, err)
	}
	staged.publicationFault = nil
	original, err := os.ReadFile(staged.Path())
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(staged.Path())
	if err != nil {
		t.Fatal(err)
	}
	start := SessionStart{ID: staged.state.ID, Staged: true, PublicationID: staged.state.publicationID}
	directorySyncs := 0
	staged.mu.Lock()
	outcome, retryErr := staged.ensureVisiblePublicationDurableLockedWith(start,
		func(marker *os.File) error {
			if err := marker.Sync(); err != nil {
				return err
			}
			return rewriteExactSessionLogPreservingMtime(staged.Path(), original, before)
		},
		func(*os.File) error {
			directorySyncs++
			return nil
		})
	staged.mu.Unlock()
	if retryErr == nil || !outcome.Visible || outcome.Durable {
		t.Fatalf("exact-child-rewrite visible retry = %+v, %v; want visible non-durable refusal", outcome, retryErr)
	}
	if directorySyncs != 0 {
		t.Fatalf("directory syncs after exact child rewrite = %d, want 0", directorySyncs)
	}
	assertExactSessionLogRewrite(t, staged.Path(), original, before)
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
}

func rewriteExactSessionLogPreservingMtime(path string, original []byte, before os.FileInfo) error {
	if err := os.WriteFile(path, original, 0o600); err != nil {
		return err
	}
	return os.Chtimes(path, before.ModTime(), before.ModTime())
}

func assertExactSessionLogRewrite(t *testing.T, path string, original []byte, before os.FileInfo) {
	t.Helper()
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("exact rewrite replaced the child log inode")
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("exact rewrite did not restore size+mtime: before=(%d,%v) after=(%d,%v)",
			before.Size(), before.ModTime(), after.Size(), after.ModTime())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("exact rewrite did not restore the original child bytes")
	}
}

func assertStagedHidden(t *testing.T, store *Store, workspace, stagedID, latestID string) {
	t.Helper()
	infos, err := store.List(workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range infos {
		if info.ID == stagedID {
			t.Fatalf("List exposed staged session %s", stagedID)
		}
	}
	all, err := store.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range all {
		for _, info := range group {
			if info.ID == stagedID {
				t.Fatalf("ListAll exposed staged session %s", stagedID)
			}
		}
	}
	if _, err := store.Open(stagedID); !errors.Is(err, ErrSessionUnpublished) {
		t.Fatalf("Open staged error = %v, want ErrSessionUnpublished", err)
	}
	if latestID == "" {
		if _, err := store.Latest(workspace); !errors.Is(err, ErrNoSessions) {
			t.Fatalf("Latest error = %v, want ErrNoSessions", err)
		}
		return
	}
	latest, err := store.Latest(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if latest.ID() != latestID {
		t.Fatalf("Latest = %s, want %s", latest.ID(), latestID)
	}
	_ = latest.Close()
}

func publicationMarkerDirectoryABA(markerPath string, markerBytes []byte, syncReplacement func() error) error {
	movedPath := markerPath + ".aba-original"
	if err := os.Rename(markerPath, movedPath); err != nil {
		return err
	}
	replacement, err := createPrivateSessionFile(markerPath)
	if err != nil {
		return err
	}
	if _, err := replacement.Write(markerBytes); err != nil {
		return errors.Join(err, replacement.Close())
	}
	if err := replacement.Sync(); err != nil {
		return errors.Join(err, replacement.Close())
	}
	if err := replacement.Close(); err != nil {
		return err
	}
	if err := syncReplacement(); err != nil {
		return err
	}
	if err := os.Remove(markerPath); err != nil {
		return err
	}
	return os.Rename(movedPath, markerPath)
}

func TestPublicationMarkerPathIsBesideLog(t *testing.T) {
	// Keep the sidecar convention pinned: cleanup and schema compatibility
	// rely on it not masquerading as another .log candidate.
	path := filepath.Join("store", "session.log")
	if got := publicationMarkerPath(path); got != path+".published" {
		t.Fatalf("publication marker path = %q", got)
	}
}
