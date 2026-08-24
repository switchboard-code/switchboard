package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/schedule"
	"github.com/switchboard-code/switchboard/internal/session"
)

func scheduleWorkspaceAlias(t *testing.T, target string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("directory symlink creation is not generally available to unprivileged Windows tests")
	}
	alias := filepath.Join(t.TempDir(), "workspace-alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	return alias
}

func createScheduleIdentitySession(t *testing.T, store *session.Store, workspace string) string {
	t.Helper()
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	id := sess.ID()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	return id
}

func armScheduleInWorkspaceDir(t *testing.T, store *session.Store, workspace, prompt string) (string, string) {
	t.Helper()
	dir, err := store.WorkspaceDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := schedule.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Add(schedule.Entry{Every: time.Hour, Prompt: prompt}); err != nil {
		ledger.Close()
		t.Fatal(err)
	}
	ledger.Close()
	return dir, filepath.Join(dir, schedule.FileName)
}

func TestOpenWorkspaceScheduleMigratesLiveLegacyAlias(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	canonical := t.TempDir()
	alias := scheduleWorkspaceAlias(t, canonical)
	createScheduleIdentitySession(t, store, alias)
	historicalDir, historicalPath := armScheduleInWorkspaceDir(t, store, alias, "preserve the reminder")

	ledger, err := openWorkspaceSchedule(store, canonical)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	got := ledger.List()
	if len(got) != 1 || got[0].Prompt != "preserve the reminder" {
		t.Fatalf("migrated schedules = %+v", got)
	}
	canonicalDir, err := store.WorkspaceDir(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if canonicalDir == historicalDir {
		t.Fatal("test did not create distinct legacy and canonical state directories")
	}
	if _, err := os.Stat(filepath.Join(canonicalDir, schedule.FileName)); err != nil {
		t.Fatalf("canonical schedule was not published: %v", err)
	}
	if _, err := os.Stat(historicalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy schedule survived migration: %v", err)
	}
}

func TestOpenWorkspaceScheduleRefusesUnboundMissingOrRetargetedAlias(t *testing.T) {
	for _, test := range []struct {
		name     string
		retarget bool
	}{
		{name: "missing"},
		{name: "retargeted", retarget: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := session.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			canonical := t.TempDir()
			alias := scheduleWorkspaceAlias(t, canonical)
			createScheduleIdentitySession(t, store, alias)
			_, historicalPath := armScheduleInWorkspaceDir(t, store, alias, "must stay with proven identity")
			if err := os.Remove(alias); err != nil {
				t.Fatal(err)
			}
			if test.retarget {
				if err := os.Symlink(t.TempDir(), alias); err != nil {
					t.Fatal(err)
				}
			}

			ledger, err := openWorkspaceSchedule(store, canonical)
			if err != nil {
				t.Fatal(err)
			}
			if got := ledger.List(); len(got) != 0 {
				ledger.Close()
				t.Fatalf("unproven alias migrated schedules: %+v", got)
			}
			ledger.Close()
			if _, err := os.Stat(historicalPath); err != nil {
				t.Fatalf("unproven historical schedule was changed: %v", err)
			}
		})
	}
}

func TestScheduleMigrationRevalidatesAliasAfterDiscovery(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	canonical := t.TempDir()
	alias := scheduleWorkspaceAlias(t, canonical)
	createScheduleIdentitySession(t, store, alias)
	_, historicalPath := armScheduleInWorkspaceDir(t, store, alias, "lose the discovery race safely")
	dirs, err := store.WorkspaceStateDirs(canonical)
	if err != nil || len(dirs) != 2 {
		t.Fatalf("workspace state discovery = %v, %v", dirs, err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), alias); err != nil {
		t.Fatal(err)
	}

	ledger, err := schedule.OpenMigrating(dirs[0], dirs[1:], func(historicalDir string) error {
		return store.ValidateWorkspaceStateDir(canonical, historicalDir)
	})
	if ledger != nil {
		ledger.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "no longer has a published session proving identity") {
		t.Fatalf("retarget after discovery migration error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirs[0], schedule.FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed revalidation published canonical schedule: %v", err)
	}
	if _, err := os.Stat(historicalPath); err != nil {
		t.Fatalf("failed revalidation removed historical schedule: %v", err)
	}
}

func TestOpenWorkspaceScheduleUsesDurableBindingAfterAliasRemoval(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	canonical := t.TempDir()
	alias := scheduleWorkspaceAlias(t, canonical)
	id := createScheduleIdentitySession(t, store, alias)
	_, historicalPath := armScheduleInWorkspaceDir(t, store, alias, "binding survives alias removal")
	resumed, err := store.OpenInWorkspace(id, canonical)
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}

	ledger, err := openWorkspaceSchedule(store, canonical)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	if got := ledger.List(); len(got) != 1 || got[0].Prompt != "binding survives alias removal" {
		t.Fatalf("durably bound migration = %+v", got)
	}
	if _, err := os.Stat(historicalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("durably bound historical schedule survived: %v", err)
	}
}

func TestOpenWorkspaceScheduleRefusesCanonicalLegacyConflict(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	canonical := t.TempDir()
	alias := scheduleWorkspaceAlias(t, canonical)
	createScheduleIdentitySession(t, store, alias)
	_, historicalPath := armScheduleInWorkspaceDir(t, store, alias, "historical reminder")
	_, canonicalPath := armScheduleInWorkspaceDir(t, store, canonical, "canonical reminder")

	ledger, err := openWorkspaceSchedule(store, canonical)
	if ledger != nil {
		ledger.Close()
	}
	if !errors.Is(err, schedule.ErrMigrationConflict) {
		t.Fatalf("conflicting workspace schedules = %v, want ErrMigrationConflict", err)
	}
	if _, err := os.Stat(canonicalPath); err != nil {
		t.Fatalf("conflict removed canonical ledger: %v", err)
	}
	if _, err := os.Stat(historicalPath); err != nil {
		t.Fatalf("conflict removed historical ledger: %v", err)
	}
}
