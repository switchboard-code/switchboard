package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/session"
)

func newHistoryMigrationStore(t *testing.T) *session.Store {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestWorkspaceHistoryMigrationMovesLiveAliasPrivately(t *testing.T) {
	setHistoryHome(t, t.TempDir())
	store := newHistoryMigrationStore(t)
	canonical := t.TempDir()
	alias := scheduleWorkspaceAlias(t, canonical)
	createScheduleIdentitySession(t, store, alias)
	appendHistory(alias, "historical prompt")
	historicalPath, err := historyPath(alias)
	if err != nil {
		t.Fatal(err)
	}

	if err := migrateWorkspaceHistory(store, canonical); err != nil {
		t.Fatal(err)
	}
	canonicalPath, err := historyPath(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if got := loadHistory(canonical); len(got) != 1 || got[0] != "historical prompt" {
		t.Fatalf("migrated canonical history = %#v", got)
	}
	if ownerOnly, err := historyPathIsOwnerOnly(canonicalPath); err != nil || !ownerOnly {
		t.Fatalf("canonical history is owner-only = %v, err=%v", ownerOnly, err)
	}
	if _, err := os.Stat(historicalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("historical history survived completed migration: %v", err)
	}
}

func TestWorkspaceHistoryMigrationRefusesUnboundMissingOrRetargetedAlias(t *testing.T) {
	for _, test := range []struct {
		name     string
		retarget bool
	}{
		{name: "missing"},
		{name: "retargeted", retarget: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			setHistoryHome(t, t.TempDir())
			store := newHistoryMigrationStore(t)
			canonical := t.TempDir()
			alias := scheduleWorkspaceAlias(t, canonical)
			createScheduleIdentitySession(t, store, alias)
			appendHistory(alias, "identity-bound prompt")
			historicalPath, _ := historyPath(alias)
			if err := os.Remove(alias); err != nil {
				t.Fatal(err)
			}
			if test.retarget {
				if err := os.Symlink(t.TempDir(), alias); err != nil {
					t.Fatal(err)
				}
			}

			if err := migrateWorkspaceHistory(store, canonical); err != nil {
				t.Fatal(err)
			}
			if got := loadHistory(canonical); len(got) != 0 {
				t.Fatalf("unproven alias migrated history: %#v", got)
			}
			if _, err := os.Stat(historicalPath); err != nil {
				t.Fatalf("unproven historical history changed: %v", err)
			}
		})
	}
}

func TestWorkspaceHistoryMigrationRevalidatesAfterAliasRetarget(t *testing.T) {
	setHistoryHome(t, t.TempDir())
	store := newHistoryMigrationStore(t)
	canonical := t.TempDir()
	alias := scheduleWorkspaceAlias(t, canonical)
	createScheduleIdentitySession(t, store, alias)
	appendHistory(alias, "lose discovery race safely")
	canonicalPath, sources, err := workspaceHistoryMigrationSources(store, canonical)
	if err != nil || len(sources) != 1 {
		t.Fatalf("history source discovery = %#v, err=%v", sources, err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), alias); err != nil {
		t.Fatal(err)
	}

	err = migrateWorkspaceHistorySources(store, canonical, canonicalPath, sources, historyMigrationHooks{})
	if err == nil || !strings.Contains(err.Error(), "no longer has a published session proving identity") {
		t.Fatalf("retargeted migration error = %v", err)
	}
	if _, err := os.Stat(canonicalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed identity proof published canonical history: %v", err)
	}
	if _, err := os.Stat(sources[0].path); err != nil {
		t.Fatalf("failed identity proof removed historical history: %v", err)
	}
}

func TestWorkspaceHistoryMigrationUsesDurableBindingAfterAliasRemoval(t *testing.T) {
	setHistoryHome(t, t.TempDir())
	store := newHistoryMigrationStore(t)
	canonical := t.TempDir()
	alias := scheduleWorkspaceAlias(t, canonical)
	id := createScheduleIdentitySession(t, store, alias)
	appendHistory(alias, "binding survives")
	historicalPath, _ := historyPath(alias)
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

	if err := migrateWorkspaceHistory(store, canonical); err != nil {
		t.Fatal(err)
	}
	if got := loadHistory(canonical); len(got) != 1 || got[0] != "binding survives" {
		t.Fatalf("durably bound history = %#v", got)
	}
	if _, err := os.Stat(historicalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("durably bound historical history survived: %v", err)
	}
}

func TestWorkspaceHistoryMigrationConflictLeavesBothFilesUnchanged(t *testing.T) {
	setHistoryHome(t, t.TempDir())
	store := newHistoryMigrationStore(t)
	canonical := t.TempDir()
	alias := scheduleWorkspaceAlias(t, canonical)
	createScheduleIdentitySession(t, store, alias)
	appendHistory(canonical, "canonical prompt")
	appendHistory(alias, "historical prompt")
	canonicalPath, _ := historyPath(canonical)
	historicalPath, _ := historyPath(alias)
	beforeCanonical, _ := os.ReadFile(canonicalPath)
	beforeHistorical, _ := os.ReadFile(historicalPath)

	err := migrateWorkspaceHistory(store, canonical)
	if !errors.Is(err, errHistoryMigrationConflict) || !strings.Contains(err.Error(), canonicalPath) || !strings.Contains(err.Error(), historicalPath) {
		t.Fatalf("history conflict = %v", err)
	}
	if got, _ := os.ReadFile(canonicalPath); string(got) != string(beforeCanonical) {
		t.Fatal("conflict rewrote canonical history")
	}
	if got, _ := os.ReadFile(historicalPath); string(got) != string(beforeHistorical) {
		t.Fatal("conflict rewrote historical history")
	}
}

func TestWorkspaceHistoryMigrationCollapsesEquivalentLegacyEncoding(t *testing.T) {
	home := t.TempDir()
	setHistoryHome(t, home)
	store := newHistoryMigrationStore(t)
	canonical := t.TempDir()
	alias := scheduleWorkspaceAlias(t, canonical)
	createScheduleIdentitySession(t, store, alias)
	appendHistory(canonical, "same prompt")
	historicalPath, _ := historyPath(alias)
	if err := os.MkdirAll(filepath.Dir(historicalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(historicalPath, []byte("same prompt\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := migrateWorkspaceHistory(store, canonical); err != nil {
		t.Fatal(err)
	}
	if got := loadHistory(canonical); len(got) != 1 || got[0] != "same prompt" {
		t.Fatalf("equivalent migration = %#v", got)
	}
	if _, err := os.Stat(historicalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("equivalent historical encoding survived: %v", err)
	}
}

func TestWorkspaceHistoryMigrationRespectsSourceLock(t *testing.T) {
	setHistoryHome(t, t.TempDir())
	store := newHistoryMigrationStore(t)
	canonical := t.TempDir()
	alias := scheduleWorkspaceAlias(t, canonical)
	createScheduleIdentitySession(t, store, alias)
	appendHistory(alias, "one writer")
	historicalPath, _ := historyPath(alias)
	canonicalPath, _ := historyPath(canonical)

	err := withHistoryLock(historicalPath, false, func(os.FileInfo) error {
		return migrateWorkspaceHistory(store, canonical)
	})
	if !errors.Is(err, errHistoryBusy) {
		t.Fatalf("migration under source owner lock = %v, want errHistoryBusy", err)
	}
	if _, err := os.Stat(canonicalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("locked migration published canonical history: %v", err)
	}
	if _, err := os.Stat(historicalPath); err != nil {
		t.Fatalf("locked migration removed historical history: %v", err)
	}
}

func TestWorkspaceHistoryMigrationRefusesSourceReplacementAfterDurablePublish(t *testing.T) {
	setHistoryHome(t, t.TempDir())
	store := newHistoryMigrationStore(t)
	canonical := t.TempDir()
	alias := scheduleWorkspaceAlias(t, canonical)
	createScheduleIdentitySession(t, store, alias)
	appendHistory(alias, "original source")
	canonicalPath, sources, err := workspaceHistoryMigrationSources(store, canonical)
	if err != nil || len(sources) != 1 {
		t.Fatalf("history source discovery = %#v, err=%v", sources, err)
	}
	historicalPath := sources[0].path
	retired := historicalPath + ".retired"
	replaced := false
	hooks := historyMigrationHooks{beforeRemove: func(path string) {
		if path != historicalPath || replaced {
			return
		}
		replaced = true
		if ownerOnly, err := historyPathIsOwnerOnly(canonicalPath); err != nil || !ownerOnly {
			t.Errorf("canonical history was not durably private before source cleanup: ownerOnly=%v err=%v", ownerOnly, err)
		}
		if err := os.Rename(historicalPath, retired); err != nil {
			t.Errorf("retiring source: %v", err)
			return
		}
		if err := os.WriteFile(historicalPath, []byte("\"replacement\"\n"), 0o600); err != nil {
			t.Errorf("installing source replacement: %v", err)
		}
	}}
	err = migrateWorkspaceHistorySources(store, canonical, canonicalPath, sources, hooks)
	if err == nil || !strings.Contains(err.Error(), "changed before rewrite") {
		t.Fatalf("source replacement migration error = %v", err)
	}
	if !replaced {
		t.Fatal("source replacement hook did not run")
	}
	if got := loadHistory(canonical); len(got) != 1 || got[0] != "original source" {
		t.Fatalf("durably published canonical history = %#v", got)
	}
	for _, path := range []string{retired, historicalPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("replacement race removed %s: %v", path, err)
		}
	}
}

func TestHistoryRewriteFinalSeamReplacementRollsBackWithoutOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.hist")
	const original = "legacy prompt\n"
	const replacement = `"uncooperative replacement"` + "\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	_, snapshot, err := readHistoryFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	retired := path + ".external-retired"
	var hookErr error
	historyNamespaceMutationTestHook = func(operation, gotPath string) {
		historyNamespaceMutationTestHook = nil
		if operation != "rewrite" || gotPath != path {
			hookErr = fmt.Errorf("unexpected namespace hook %q %q", operation, gotPath)
			return
		}
		if hookErr = os.Rename(path, retired); hookErr == nil {
			hookErr = os.WriteFile(path, []byte(replacement), 0o600)
		}
	}
	t.Cleanup(func() { historyNamespaceMutationTestHook = nil })

	err = rewriteHistoryIfUnchanged(path, []string{"normalized"}, snapshot, nil)
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err == nil {
		t.Fatal("final-seam replacement was reported as a successful rewrite")
	}
	for file, want := range map[string]string{path: replacement, retired: original} {
		got, readErr := os.ReadFile(file)
		if readErr != nil || string(got) != want {
			t.Fatalf("%s = %q, err=%v; want %q", file, got, readErr, want)
		}
	}
	assertNoLiveHistoryTransaction(t, path)
}

func TestHistoryRewriteFinalSeamSameInodeMutationsNeverPublishOrErase(t *testing.T) {
	for _, test := range []struct {
		name       string
		mutate     func(string) error
		wantTarget string
		forbidden  string
	}{
		{
			name: "selected target appended",
			mutate: func(path string) error {
				return appendHistoryTestBytes(path, "late same-inode append\n")
			},
			wantTarget: "legacy\nlate same-inode append\n",
		},
		{
			name: "owned stage changed",
			mutate: func(path string) error {
				stage, err := findHistoryTestStage(filepath.Dir(path), ".history-rewrite-")
				if err != nil {
					return err
				}
				return appendHistoryTestBytes(stage, "stage mutation")
			},
			wantTarget: "legacy\n",
			forbidden:  "stage mutation",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "history.hist")
			if err := os.WriteFile(path, []byte("legacy\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, snapshot, err := readHistoryFile(path, nil)
			if err != nil {
				t.Fatal(err)
			}
			var hookErr error
			historyNamespaceMutationTestHook = func(operation, gotPath string) {
				if operation != "rewrite" {
					return
				}
				historyNamespaceMutationTestHook = nil
				if gotPath != path {
					hookErr = fmt.Errorf("rewrite hook path = %s, want %s", gotPath, path)
					return
				}
				hookErr = test.mutate(path)
			}
			t.Cleanup(func() { historyNamespaceMutationTestHook = nil })
			if err := rewriteHistoryIfUnchanged(path, []string{"desired"}, snapshot, nil); err == nil {
				t.Fatal("same-inode final-seam mutation was accepted")
			}
			if hookErr != nil {
				t.Fatal(hookErr)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != test.wantTarget {
				t.Fatalf("canonical after rollback = %q, err=%v; want %q", got, err, test.wantTarget)
			}
			assertNoLiveHistoryTransaction(t, path)
			if test.forbidden != "" {
				assertHistoryArtifactsDoNotContain(t, dir, test.forbidden)
			}
			assertRetiredHistoryArtifactsArePrivateAndEmpty(t, dir)
		})
	}
}

func TestHistoryCreateFinalSeamSameInodeStageMutationLeavesCanonicalAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.hist")
	var hookErr error
	historyNamespaceMutationTestHook = func(operation, gotPath string) {
		if operation != "create" {
			return
		}
		historyNamespaceMutationTestHook = nil
		if gotPath != path {
			hookErr = fmt.Errorf("create hook path = %s, want %s", gotPath, path)
			return
		}
		stage, err := findHistoryTestStage(dir, ".history-create-")
		if err != nil {
			hookErr = err
			return
		}
		hookErr = appendHistoryTestBytes(stage, "stage mutation")
	}
	t.Cleanup(func() { historyNamespaceMutationTestHook = nil })
	err := withHistoryLock(path, false, func(parent os.FileInfo) error {
		return createHistoryLocked(path, []string{"desired canonical"}, parent)
	})
	if err == nil {
		t.Fatal("same-inode create-stage mutation was accepted")
	}
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mutated create stage survived as canonical: %v", err)
	}
	assertNoLiveHistoryTransaction(t, path)
	assertHistoryArtifactsDoNotContain(t, dir, "stage mutation")
	assertRetiredHistoryArtifactsArePrivateAndEmpty(t, dir)
}

func TestHistoryRemoveFinalSeamSameInodeMutationIsRestored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.hist")
	if err := os.WriteFile(path, []byte("legacy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, snapshot, err := readHistoryFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	var hookErr error
	historyNamespaceMutationTestHook = func(operation, gotPath string) {
		if operation != "remove" {
			return
		}
		historyNamespaceMutationTestHook = nil
		if gotPath != path {
			hookErr = fmt.Errorf("remove hook path = %s, want %s", gotPath, path)
			return
		}
		hookErr = appendHistoryTestBytes(path, "late same-inode append\n")
	}
	t.Cleanup(func() { historyNamespaceMutationTestHook = nil })
	err = withHistoryLock(path, false, func(parent os.FileInfo) error {
		return removeHistoryIfUnchangedLocked(path, snapshot, parent, nil)
	})
	if err == nil {
		t.Fatal("same-inode remove mutation was scrubbed")
	}
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "legacy\nlate same-inode append\n" {
		t.Fatalf("restored modified source = %q, err=%v", got, err)
	}
	assertNoLiveHistoryTransaction(t, path)
}

func appendHistoryTestBytes(path, data string) (resultErr error) {
	// Use the production descriptor opener so the adversarial mutation shares
	// deletion on Windows and can coexist with the exact-handle publication
	// lease. os.OpenFile's Windows sharing defaults are not this contract.
	f, err := openHistoryDataDescriptor(path, true, false)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	if _, err := f.WriteString(data); err != nil {
		return err
	}
	return f.Sync()
}

func findHistoryTestStage(dir, prefix string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var found string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("multiple %s stages", prefix)
		}
		found = filepath.Join(dir, entry.Name())
	}
	if found == "" {
		return "", fmt.Errorf("no %s stage", prefix)
	}
	return found, nil
}

func TestWorkspaceHistoryMigrationFinalSeamReplacementRestoresForeignSource(t *testing.T) {
	setHistoryHome(t, t.TempDir())
	store := newHistoryMigrationStore(t)
	canonical := t.TempDir()
	alias := scheduleWorkspaceAlias(t, canonical)
	createScheduleIdentitySession(t, store, alias)
	appendHistory(alias, "original source")
	canonicalPath, sources, err := workspaceHistoryMigrationSources(store, canonical)
	if err != nil || len(sources) != 1 {
		t.Fatalf("history source discovery = %#v, err=%v", sources, err)
	}
	historicalPath := sources[0].path
	retired := historicalPath + ".external-retired"
	const replacement = `"uncooperative source"` + "\n"
	var hookErr error
	historyNamespaceMutationTestHook = func(operation, gotPath string) {
		if operation != "remove" {
			return
		}
		historyNamespaceMutationTestHook = nil
		if gotPath != historicalPath {
			hookErr = fmt.Errorf("unexpected namespace hook %q %q", operation, gotPath)
			return
		}
		if hookErr = os.Rename(historicalPath, retired); hookErr == nil {
			hookErr = os.WriteFile(historicalPath, []byte(replacement), 0o600)
		}
	}
	t.Cleanup(func() { historyNamespaceMutationTestHook = nil })

	err = migrateWorkspaceHistorySources(store, canonical, canonicalPath, sources, historyMigrationHooks{})
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err == nil {
		t.Fatal("final-seam source replacement was reported as removed")
	}
	if got, readErr := os.ReadFile(historicalPath); readErr != nil || string(got) != replacement {
		t.Fatalf("replacement source = %q, err=%v", got, readErr)
	}
	if got := loadHistory(canonical); len(got) != 1 || got[0] != "original source" {
		t.Fatalf("durably published canonical history = %#v", got)
	}
	if _, err := os.Stat(retired); err != nil {
		t.Fatalf("externally retired original was lost: %v", err)
	}
	// Loading the source reconciles the pre-move transaction record. It must
	// not change the foreign replacement while doing so.
	if _, _, err := readHistoryFile(historicalPath, nil); err != nil {
		t.Fatal(err)
	}
	assertNoLiveHistoryTransaction(t, historicalPath)
}

func TestHistoryRewriteCrashAfterExchangeRecoversAndScrubsOldImage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.hist")
	const old = "legacy secret-shaped history that must not survive\n"
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	_, snapshot, err := readHistoryFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	crash := errors.New("simulated process death")
	historyTransactionBoundaryHook = func(operation string, boundary historyTransactionBoundary) error {
		if operation == "rewrite" && boundary == historyTransactionNamespaceChanged {
			historyTransactionBoundaryHook = nil
			return crash
		}
		return nil
	}
	t.Cleanup(func() { historyTransactionBoundaryHook = nil })

	if err := rewriteHistoryIfUnchanged(path, []string{"normalized"}, snapshot, nil); !errors.Is(err, crash) {
		t.Fatalf("crash-boundary rewrite error = %v", err)
	}
	assertLiveHistoryTransaction(t, path)
	data, _, err := readHistoryFile(path, nil)
	if err != nil {
		t.Fatalf("recovering rewrite transaction: %v", err)
	}
	if string(data) != `"normalized"`+"\n" {
		t.Fatalf("recovered history = %q", data)
	}
	assertNoLiveHistoryTransaction(t, path)
	assertHistoryArtifactsDoNotContain(t, dir, old)
	assertRetiredHistoryArtifactsArePrivateAndEmpty(t, dir)
}

func TestHistoryRewriteCrashThenForeignCanonicalStillCleansExactOldImage(t *testing.T) {
	for _, boundary := range []historyTransactionBoundary{
		historyTransactionNamespaceChanged,
		historyTransactionRetired,
	} {
		t.Run(fmt.Sprintf("boundary %d", boundary), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "history.hist")
			const old = "legacy exact old image\n"
			const foreign = "foreign canonical after crash\n"
			if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
				t.Fatal(err)
			}
			_, snapshot, err := readHistoryFile(path, nil)
			if err != nil {
				t.Fatal(err)
			}
			crash := errors.New("simulated committed rewrite process death")
			historyTransactionBoundaryHook = func(operation string, got historyTransactionBoundary) error {
				if operation == "rewrite" && got == boundary {
					historyTransactionBoundaryHook = nil
					return crash
				}
				return nil
			}
			t.Cleanup(func() { historyTransactionBoundaryHook = nil })
			if err := rewriteHistoryIfUnchanged(path, []string{"desired"}, snapshot, nil); !errors.Is(err, crash) {
				t.Fatalf("rewrite crash error = %v", err)
			}
			assertLiveHistoryTransaction(t, path)
			externalDesired := filepath.Join(dir, "externally-retained-desired")
			if err := os.Rename(path, externalDesired); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(foreign), 0o600); err != nil {
				t.Fatal(err)
			}
			data, _, err := readHistoryFile(path, nil)
			if err != nil || string(data) != foreign {
				t.Fatalf("foreign canonical after recovery = %q, err=%v", data, err)
			}
			if desired, err := os.ReadFile(externalDesired); err != nil || string(desired) != `"desired"`+"\n" {
				t.Fatalf("externally retained desired image = %q, err=%v", desired, err)
			}
			assertNoLiveHistoryTransaction(t, path)
			assertHistoryArtifactsDoNotContain(t, dir, old)
			assertRetiredHistoryArtifactsArePrivateAndEmpty(t, dir)
		})
	}
}

func TestHistoryRewriteCrashAfterLedgerSyncRecoversEmptyReservedStage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.hist")
	if err := os.WriteFile(path, []byte("legacy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, snapshot, err := readHistoryFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	crash := errors.New("simulated process death after ledger sync")
	historyTransactionBoundaryHook = func(operation string, boundary historyTransactionBoundary) error {
		if operation == "rewrite" && boundary == historyTransactionRecorded {
			historyTransactionBoundaryHook = nil
			return crash
		}
		return nil
	}
	t.Cleanup(func() { historyTransactionBoundaryHook = nil })
	if err := rewriteHistoryIfUnchanged(path, []string{"desired private payload"}, snapshot, nil); !errors.Is(err, crash) {
		t.Fatalf("ledger-boundary rewrite error = %v", err)
	}
	assertLiveHistoryTransaction(t, path)
	data, _, err := readHistoryFile(path, nil)
	if err != nil {
		t.Fatalf("recovering reserved stage: %v", err)
	}
	if string(data) != "legacy\n" {
		t.Fatalf("prepublication recovery changed target to %q", data)
	}
	assertNoLiveHistoryTransaction(t, path)
	assertHistoryArtifactsDoNotContain(t, dir, "desired private payload")
	assertRetiredHistoryArtifactsArePrivateAndEmpty(t, dir)
}

func TestHistoryCreateProcessDeathAfterPartialStageRecovers(t *testing.T) {
	if runHistoryPartialStageCrashChild(t, "create") {
		return
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "history.hist")
	runHistoryPartialStageCrashProcess(t, "create", path)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial canonical history became visible after process death: %v", err)
	}
	assertLiveHistoryTransaction(t, path)
	if err := withHistoryLock(path, false, func(os.FileInfo) error { return nil }); err != nil {
		t.Fatalf("recovering partial canonical creation: %v", err)
	}
	assertNoLiveHistoryTransaction(t, path)
	assertHistoryArtifactsDoNotContain(t, dir, `"canoni`)
	assertRetiredHistoryArtifactsArePrivateAndEmpty(t, dir)
	if err := withHistoryLock(path, false, func(parent os.FileInfo) error {
		return createHistoryLocked(path, []string{"canonical after recovery"}, parent)
	}); err != nil {
		t.Fatalf("retrying canonical creation: %v", err)
	}
	data, _, err := readHistoryFile(path, nil)
	if err != nil || string(data) != `"canonical after recovery"`+"\n" {
		t.Fatalf("retried canonical history = %q, err=%v", data, err)
	}
}

func TestHistoryRewriteProcessDeathAfterPartialStageRecovers(t *testing.T) {
	if runHistoryPartialStageCrashChild(t, "rewrite") {
		return
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "history.hist")
	const original = "legacy remains canonical\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	runHistoryPartialStageCrashProcess(t, "rewrite", path)
	if got, err := os.ReadFile(path); err != nil || string(got) != original {
		t.Fatalf("canonical changed during partial rewrite: %q, err=%v", got, err)
	}
	assertLiveHistoryTransaction(t, path)
	data, _, err := readHistoryFile(path, nil)
	if err != nil || string(data) != original {
		t.Fatalf("recovering partial rewrite = %q, err=%v", data, err)
	}
	assertNoLiveHistoryTransaction(t, path)
	assertHistoryArtifactsDoNotContain(t, dir, `"replac`)
	assertRetiredHistoryArtifactsArePrivateAndEmpty(t, dir)
}

func TestHistoryTransactionRecordProcessDeathNeverPublishesTornLedger(t *testing.T) {
	if runHistoryTornRecordCrashChild(t) {
		return
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "history.hist")
	const original = "legacy remains canonical\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	runHistoryPartialStageCrashProcess(t, "record", path)
	if got, err := os.ReadFile(path); err != nil || string(got) != original {
		t.Fatalf("canonical changed during torn record publication: %q, err=%v", got, err)
	}
	assertNoLiveHistoryTransaction(t, path)
	assertHistoryArtifactsDoNotContain(t, dir, "record crash payload")
	_, snapshot, err := readHistoryFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rewriteHistoryIfUnchanged(path, []string{"recovered after record crash"}, snapshot, nil); err != nil {
		t.Fatalf("rewrite after torn unpublished record: %v", err)
	}
	data, _, err := readHistoryFile(path, nil)
	if err != nil || string(data) != `"recovered after record crash"`+"\n" {
		t.Fatalf("history after record crash recovery = %q, err=%v", data, err)
	}
}

func TestHistoryTransactionRecordFinalSeamMutationIsUnpublished(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.hist")
	const original = "legacy remains canonical\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	_, snapshot, err := readHistoryFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	historyTransactionRecordPublishTestHook = func(record *os.File) error {
		historyTransactionRecordPublishTestHook = nil
		if _, err := record.WriteAt([]byte{'X'}, 0); err != nil {
			return err
		}
		return record.Sync()
	}
	t.Cleanup(func() { historyTransactionRecordPublishTestHook = nil })
	if err := rewriteHistoryIfUnchanged(path, []string{"desired payload"}, snapshot, nil); err == nil {
		t.Fatal("mutated transaction preparation was published")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != original {
		t.Fatalf("canonical changed under mutated transaction record: %q, err=%v", got, err)
	}
	assertNoLiveHistoryTransaction(t, path)
	assertHistoryArtifactsDoNotContain(t, dir, "desired payload")
	assertRetiredHistoryArtifactsArePrivateAndEmpty(t, dir)
}

const (
	historyCrashChildOperationEnv = "SWITCHBOARD_HISTORY_CRASH_CHILD_OPERATION"
	historyCrashChildPathEnv      = "SWITCHBOARD_HISTORY_CRASH_CHILD_PATH"
)

func runHistoryPartialStageCrashChild(t *testing.T, operation string) bool {
	t.Helper()
	if os.Getenv(historyCrashChildOperationEnv) != operation {
		return false
	}
	path := os.Getenv(historyCrashChildPathEnv)
	if path == "" {
		fmt.Fprintln(os.Stderr, "history crash child has no target path")
		os.Exit(90)
	}
	historyStageWriteTestHook = func(gotOperation string, stage *os.File) error {
		if gotOperation != operation {
			return fmt.Errorf("partial-stage hook operation = %q, want %q", gotOperation, operation)
		}
		if err := stage.Truncate(8); err != nil {
			return err
		}
		if err := stage.Sync(); err != nil {
			return err
		}
		return killHistoryTestProcess()
	}
	var err error
	switch operation {
	case "create":
		err = withHistoryLock(path, false, func(parent os.FileInfo) error {
			return createHistoryLocked(path, []string{"canonical partial payload"}, parent)
		})
	case "rewrite":
		var snapshot historySnapshot
		_, snapshot, err = readHistoryFile(path, nil)
		if err == nil {
			err = rewriteHistoryIfUnchanged(path, []string{"replacement partial payload"}, snapshot, nil)
		}
	default:
		err = fmt.Errorf("unknown history crash operation %q", operation)
	}
	fmt.Fprintf(os.Stderr, "history crash child returned without dying: %v\n", err)
	os.Exit(91)
	return true
}

func runHistoryTornRecordCrashChild(t *testing.T) bool {
	t.Helper()
	if os.Getenv(historyCrashChildOperationEnv) != "record" {
		return false
	}
	path := os.Getenv(historyCrashChildPathEnv)
	if path == "" {
		fmt.Fprintln(os.Stderr, "history record crash child has no target path")
		os.Exit(92)
	}
	historyTransactionRecordWriteTestHook = func(record *os.File) error {
		if err := record.Truncate(8); err != nil {
			return err
		}
		if err := record.Sync(); err != nil {
			return err
		}
		return killHistoryTestProcess()
	}
	_, snapshot, err := readHistoryFile(path, nil)
	if err == nil {
		err = rewriteHistoryIfUnchanged(path, []string{"record crash payload"}, snapshot, nil)
	}
	fmt.Fprintf(os.Stderr, "history record crash child returned without dying: %v\n", err)
	os.Exit(93)
	return true
}

func killHistoryTestProcess() error {
	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		return err
	}
	if err := process.Kill(); err != nil {
		return err
	}
	return errors.New("process remained alive after self-kill")
}

func runHistoryPartialStageCrashProcess(t *testing.T, operation, path string) {
	t.Helper()
	testName := map[string]string{
		"create":  "TestHistoryCreateProcessDeathAfterPartialStageRecovers",
		"record":  "TestHistoryTransactionRecordProcessDeathNeverPublishesTornLedger",
		"rewrite": "TestHistoryRewriteProcessDeathAfterPartialStageRecovers",
	}[operation]
	if testName == "" {
		t.Fatalf("unknown partial-stage crash operation %q", operation)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+testName+"$")
	cmd.Env = append(os.Environ(),
		historyCrashChildOperationEnv+"="+operation,
		historyCrashChildPathEnv+"="+path,
	)
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("partial-stage child exited normally; output=%s", output)
	}
}

func TestWorkspaceHistoryMigrationCrashAfterQuarantineRecoversCleanup(t *testing.T) {
	setHistoryHome(t, t.TempDir())
	store := newHistoryMigrationStore(t)
	canonical := t.TempDir()
	alias := scheduleWorkspaceAlias(t, canonical)
	createScheduleIdentitySession(t, store, alias)
	appendHistory(alias, "historical crash image")
	historicalPath, _ := historyPath(alias)
	crash := errors.New("simulated process death")
	historyTransactionBoundaryHook = func(operation string, boundary historyTransactionBoundary) error {
		if operation == "remove" && boundary == historyTransactionNamespaceChanged {
			historyTransactionBoundaryHook = nil
			return crash
		}
		return nil
	}
	t.Cleanup(func() { historyTransactionBoundaryHook = nil })

	if err := migrateWorkspaceHistory(store, canonical); !errors.Is(err, crash) {
		t.Fatalf("crash-boundary migration error = %v", err)
	}
	assertLiveHistoryTransaction(t, historicalPath)
	if err := migrateWorkspaceHistory(store, canonical); err != nil {
		t.Fatalf("recovering migration cleanup: %v", err)
	}
	if _, err := os.Stat(historicalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered historical path exists: %v", err)
	}
	assertNoLiveHistoryTransaction(t, historicalPath)
	assertHistoryArtifactsDoNotContain(t, filepath.Dir(historicalPath), `"historical crash image"`+"\n")
	assertRetiredHistoryArtifactsArePrivateAndEmpty(t, filepath.Dir(historicalPath))
}

func TestHistoryRemoveCrashThenForeignTargetStillCleansExactRecordedSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.hist")
	const original = "legacy exact source\n"
	const foreign = "foreign post-crash source\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	_, snapshot, err := readHistoryFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	crash := errors.New("simulated remove process death")
	historyTransactionBoundaryHook = func(operation string, boundary historyTransactionBoundary) error {
		if operation == "remove" && boundary == historyTransactionNamespaceChanged {
			historyTransactionBoundaryHook = nil
			return crash
		}
		return nil
	}
	t.Cleanup(func() { historyTransactionBoundaryHook = nil })
	err = withHistoryLock(path, false, func(parent os.FileInfo) error {
		return removeHistoryIfUnchangedLocked(path, snapshot, parent, nil)
	})
	if !errors.Is(err, crash) {
		t.Fatalf("remove crash error = %v", err)
	}
	if err := os.WriteFile(path, []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}
	data, _, err := readHistoryFile(path, nil)
	if err != nil || string(data) != foreign {
		t.Fatalf("foreign target after cleanup = %q, err=%v", data, err)
	}
	assertNoLiveHistoryTransaction(t, path)
	assertHistoryArtifactsDoNotContain(t, dir, original)
	assertRetiredHistoryArtifactsArePrivateAndEmpty(t, dir)
}

func TestHistoryRewriteParentMoveUsesBoundDirectoryOnly(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "history")
	moved := filepath.Join(base, "history-moved")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{parent, outside} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(parent, "workspace.hist")
	outsidePath := filepath.Join(outside, "workspace.hist")
	if err := os.WriteFile(path, []byte("legacy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsidePath, []byte("outside sentinel\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, snapshot, err := readHistoryFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	var hookErr error
	err = rewriteHistoryIfUnchanged(path, []string{"bound result"}, snapshot, func() {
		if hookErr = os.Rename(parent, moved); hookErr == nil {
			hookErr = os.Symlink(outside, parent)
		}
	})
	if hookErr != nil {
		if !historyParentMoveBlockedByRetainedHandle(hookErr) {
			t.Fatal(hookErr)
		}
		// Windows refuses to rename a directory while Switchboard retains the
		// root handle that makes this transaction capability-bound. That kernel
		// refusal is stronger than the namespace-race outcome exercised on Unix:
		// prove the write stayed in the retained directory and the proposed
		// replacement namespace was never reached.
		if err != nil {
			t.Fatalf("rewrite after kernel-blocked parent move: %v", err)
		}
		if got, readErr := os.ReadFile(outsidePath); readErr != nil || string(got) != "outside sentinel\n" {
			t.Fatalf("outside history = %q, err=%v", got, readErr)
		}
		if got, readErr := os.ReadFile(path); readErr != nil || string(got) != `"bound result"`+"\n" {
			t.Fatalf("bound history = %q, err=%v", got, readErr)
		}
		if _, statErr := os.Lstat(moved); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("blocked parent move unexpectedly published %s: %v", moved, statErr)
		}
		return
	}
	if err == nil {
		t.Fatal("parent namespace move was not reported")
	}
	if got, readErr := os.ReadFile(outsidePath); readErr != nil || string(got) != "outside sentinel\n" {
		t.Fatalf("outside history = %q, err=%v", got, readErr)
	}
	if got, readErr := os.ReadFile(filepath.Join(moved, "workspace.hist")); readErr != nil || string(got) != `"bound result"`+"\n" {
		t.Fatalf("bound history = %q, err=%v", got, readErr)
	}
}

func TestHistoryRewriteRollbackFailureRetainsEveryImageAndRecoveryEvidence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.hist")
	const original = "legacy original\n"
	const foreign = `"foreign replacement"` + "\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	_, snapshot, err := readHistoryFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	retired := path + ".external-retired"
	historyNamespaceMutationTestHook = func(operation, gotPath string) {
		historyNamespaceMutationTestHook = nil
		if operation != "rewrite" || gotPath != path {
			t.Errorf("unexpected namespace hook %q %q", operation, gotPath)
			return
		}
		if err := os.Rename(path, retired); err != nil {
			t.Errorf("retiring original: %v", err)
			return
		}
		if err := os.WriteFile(path, []byte(foreign), 0o600); err != nil {
			t.Errorf("installing foreign source: %v", err)
		}
	}
	rollbackFailure := errors.New("simulated rollback failure")
	historyRollbackTestHook = func() error { return rollbackFailure }
	t.Cleanup(func() {
		historyNamespaceMutationTestHook = nil
		historyRollbackTestHook = nil
	})

	err = rewriteHistoryIfUnchanged(path, []string{"desired"}, snapshot, nil)
	if historySubstitutionRefusesBeforePublication() {
		if !errors.Is(err, checkpoint.ErrStale) || errors.Is(err, rollbackFailure) {
			t.Fatalf("exact-handle substitution refusal = %v, want checkpoint.ErrStale without rollback", err)
		}
		if got, readErr := os.ReadFile(retired); readErr != nil || string(got) != original {
			t.Fatalf("externally retired original = %q, err=%v", got, readErr)
		}
		if got, readErr := os.ReadFile(path); readErr != nil || string(got) != foreign {
			t.Fatalf("foreign canonical = %q, err=%v", got, readErr)
		}
		assertNoLiveHistoryTransaction(t, path)
		assertHistoryArtifactsDoNotContain(t, dir, `"desired"`)
		assertRetiredHistoryArtifactsArePrivateAndEmpty(t, dir)
		return
	}
	if !errors.Is(err, errHistoryRecoveryRequired) || !errors.Is(err, rollbackFailure) {
		t.Fatalf("rollback failure = %v", err)
	}
	if got, readErr := os.ReadFile(retired); readErr != nil || string(got) != original {
		t.Fatalf("externally retired original = %q, err=%v", got, readErr)
	}
	assertLiveHistoryTransaction(t, path)
	assertHistoryDirectoryContains(t, dir, foreign)
	if _, _, recoveryErr := readHistoryFile(path, nil); !errors.Is(recoveryErr, errHistoryRecoveryRequired) {
		t.Fatalf("ambiguous rollback recovery = %v", recoveryErr)
	}
}

func TestHistoryRewriteRecoversEveryWindowsExchangeCrashState(t *testing.T) {
	for _, test := range []struct {
		name      string
		step      int
		want      string
		wantStage bool
	}{
		{name: "source staged while target remains", step: 1, want: "legacy\n"},
		{name: "source staged and displaced moved while target absent", step: 2, want: `"desired"` + "\n"},
		{name: "publication complete and displaced retirement moved", step: 3, want: `"desired"` + "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "history.hist")
			if err := os.WriteFile(path, []byte("legacy\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, snapshot, err := readHistoryFile(path, nil)
			if err != nil {
				t.Fatal(err)
			}
			stage, internal := arrangeHistoryExchangeCrashState(t, path, snapshot, test.step)
			assertLiveHistoryTransaction(t, path)
			data, _, err := readHistoryFile(path, nil)
			if err != nil {
				t.Fatalf("recovering exchange step %d: %v", test.step, err)
			}
			if string(data) != test.want {
				t.Fatalf("recovered exchange step %d = %q, want %q", test.step, data, test.want)
			}
			for _, name := range []string{stage, internal} {
				if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("active exchange stage %s survived recovery: %v", name, err)
				}
			}
			assertNoLiveHistoryTransaction(t, path)
		})
	}
}

func TestHistoryRewriteRecoversEveryWindowsRollbackCrashState(t *testing.T) {
	for _, step := range []int{1, 2} {
		t.Run(fmt.Sprintf("rollback step %d", step), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "history.hist")
			if err := os.WriteFile(path, []byte("legacy\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, snapshot, err := readHistoryFile(path, nil)
			if err != nil {
				t.Fatal(err)
			}
			stage, rollback := arrangeHistoryRollbackCrashState(t, path, snapshot, step)
			assertLiveHistoryTransaction(t, path)
			data, _, err := readHistoryFile(path, nil)
			if err != nil {
				t.Fatalf("recovering rollback step %d: %v", step, err)
			}
			if string(data) != "legacy\n" {
				t.Fatalf("recovered rollback step %d = %q", step, data)
			}
			for _, name := range []string{stage, rollback} {
				if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("active rollback stage %s survived recovery: %v", name, err)
				}
			}
			assertNoLiveHistoryTransaction(t, path)
			assertHistoryArtifactsDoNotContain(t, dir, `"desired"`)
			assertRetiredHistoryArtifactsArePrivateAndEmpty(t, dir)
		})
	}
}

func TestHistoryRewriteInternalCrashStateToleratesForeignTargetAndStage(t *testing.T) {
	for _, test := range []struct {
		name    string
		arrange func(*testing.T, string, historySnapshot) (string, string)
	}{
		{
			name: "forward step one",
			arrange: func(t *testing.T, path string, snapshot historySnapshot) (string, string) {
				return arrangeHistoryExchangeCrashState(t, path, snapshot, 1)
			},
		},
		{
			name: "rollback step two",
			arrange: func(t *testing.T, path string, snapshot historySnapshot) (string, string) {
				return arrangeHistoryRollbackCrashState(t, path, snapshot, 2)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "history.hist")
			const original = "legacy exact source\n"
			const foreignTarget = "foreign canonical\n"
			const foreignStage = "foreign occupied stage\n"
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			_, snapshot, err := readHistoryFile(path, nil)
			if err != nil {
				t.Fatal(err)
			}
			stage, internal := test.arrange(t, path, snapshot)
			externalOriginal := filepath.Join(dir, "externally-retained-original")
			if err := os.Rename(path, externalOriginal); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(foreignTarget), 0o600); err != nil {
				t.Fatal(err)
			}
			stagePath := filepath.Join(dir, stage)
			if err := os.WriteFile(stagePath, []byte(foreignStage), 0o600); err != nil {
				t.Fatal(err)
			}
			data, _, err := readHistoryFile(path, nil)
			if err != nil || string(data) != foreignTarget {
				t.Fatalf("foreign canonical after recovery = %q, err=%v", data, err)
			}
			if got, err := os.ReadFile(stagePath); err != nil || string(got) != foreignStage {
				t.Fatalf("foreign stage after recovery = %q, err=%v", got, err)
			}
			if got, err := os.ReadFile(externalOriginal); err != nil || string(got) != original {
				t.Fatalf("external original after recovery = %q, err=%v", got, err)
			}
			if _, err := os.Stat(filepath.Join(dir, internal)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("owned internal replacement survived recovery: %v", err)
			}
			assertNoLiveHistoryTransaction(t, path)
			assertRetiredHistoryArtifactsArePrivateAndEmpty(t, dir)
		})
	}
}

func arrangeHistoryRollbackCrashState(t *testing.T, path string, snapshot historySnapshot, step int) (stageName, rollbackName string) {
	t.Helper()
	err := withHistoryLock(path, false, func(lockedParent os.FileInfo) error {
		parent, err := openHistoryBoundParent(path, lockedParent)
		if err != nil {
			return err
		}
		defer parent.close()
		current, err := openHistorySnapshotInRoot(parent, snapshot, true)
		if err != nil {
			return err
		}
		defer current.Close()
		stage, name, err := createHistoryBoundTemp(parent, ".history-rewrite-")
		if err != nil {
			return err
		}
		defer stage.Close()
		raw, err := encodeHistory([]string{"desired"})
		if err != nil {
			return err
		}
		if _, err := stage.Write(raw); err != nil {
			return err
		}
		if err := stage.Sync(); err != nil {
			return err
		}
		replacement, err := historyTransactionImageFor(stage, raw)
		if err != nil {
			return err
		}
		identity, err := historyFileIdentity(current)
		if err != nil {
			return err
		}
		rollback := historyExchangeStagingName(parent.name)
		txn := historyTransaction{
			Version:       historyTransactionVersion,
			Operation:     "rewrite",
			Target:        parent.name,
			Stage:         name,
			RetiredStage:  historyClearedName(name),
			InternalStage: historyExchangeStagingName(name),
			RollbackStage: rollback,
			Expected: historyTransactionImage{
				Identity: identity,
				Size:     snapshot.size,
				Digest:   fmt.Sprintf("%x", snapshot.digest),
			},
			Replacement: replacement,
		}
		record, _, err := createHistoryTransactionRecord(parent, txn)
		if err != nil {
			return err
		}
		if err := record.Close(); err != nil {
			return err
		}
		if err := parent.root.Rename(name, rollback); err != nil {
			return err
		}
		if step == 1 {
			if err := parent.root.Rename(parent.name, name); err != nil {
				return err
			}
		} else if step != 2 {
			return fmt.Errorf("unknown rollback crash step %d", step)
		}
		if err := syncHistoryBoundDirectory(parent.root); err != nil {
			return err
		}
		stageName, rollbackName = name, rollback
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return stageName, rollbackName
}

func arrangeHistoryExchangeCrashState(t *testing.T, path string, snapshot historySnapshot, step int) (stageName, internalName string) {
	t.Helper()
	err := withHistoryLock(path, false, func(lockedParent os.FileInfo) error {
		parent, err := openHistoryBoundParent(path, lockedParent)
		if err != nil {
			return err
		}
		defer parent.close()
		current, err := openHistorySnapshotInRoot(parent, snapshot, true)
		if err != nil {
			return err
		}
		defer current.Close()
		stage, name, err := createHistoryBoundTemp(parent, ".history-rewrite-")
		if err != nil {
			return err
		}
		defer stage.Close()
		raw, err := encodeHistory([]string{"desired"})
		if err != nil {
			return err
		}
		if _, err := stage.Write(raw); err != nil {
			return err
		}
		if err := stage.Sync(); err != nil {
			return err
		}
		replacement, err := historyTransactionImageFor(stage, raw)
		if err != nil {
			return err
		}
		identity, err := historyFileIdentity(current)
		if err != nil {
			return err
		}
		internal := historyExchangeStagingName(name)
		txn := historyTransaction{
			Version:       historyTransactionVersion,
			Operation:     "rewrite",
			Target:        parent.name,
			Stage:         name,
			RetiredStage:  historyClearedName(name),
			InternalStage: internal,
			RollbackStage: historyExchangeStagingName(parent.name),
			Expected: historyTransactionImage{
				Identity: identity,
				Size:     snapshot.size,
				Digest:   fmt.Sprintf("%x", snapshot.digest),
			},
			Replacement: replacement,
		}
		record, _, err := createHistoryTransactionRecord(parent, txn)
		if err != nil {
			return err
		}
		if err := record.Close(); err != nil {
			return err
		}
		if err := parent.root.Rename(name, internal); err != nil {
			return err
		}
		if step == 2 {
			if err := parent.root.Rename(parent.name, name); err != nil {
				return err
			}
		} else if step == 3 {
			if err := parent.root.Rename(parent.name, name); err != nil {
				return err
			}
			if err := parent.root.Rename(internal, parent.name); err != nil {
				return err
			}
			if err := parent.root.Rename(name, historyClearedName(name)); err != nil {
				return err
			}
		}
		if err := syncHistoryBoundDirectory(parent.root); err != nil {
			return err
		}
		stageName, internalName = name, internal
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return stageName, internalName
}

func assertLiveHistoryTransaction(t *testing.T, path string) {
	t.Helper()
	name := historyTransactionRecordName(filepath.Base(path))
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), name)); err != nil {
		t.Fatalf("live prompt history transaction missing: %v", err)
	}
}

func assertNoLiveHistoryTransaction(t *testing.T, path string) {
	t.Helper()
	name := historyTransactionRecordName(filepath.Base(path))
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prompt history transaction survived: %v", err)
	}
}

func assertHistoryArtifactsDoNotContain(t *testing.T, dir, forbidden string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || (!strings.HasPrefix(entry.Name(), ".history-") && !strings.HasPrefix(entry.Name(), ".switchboard-")) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("retired prompt history bytes survived in %s", entry.Name())
		}
	}
}

func assertHistoryDirectoryContains(t *testing.T, dir, want string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err == nil && string(raw) == want {
			return
		}
	}
	t.Fatalf("history directory did not retain %q", want)
}

func assertRetiredHistoryArtifactsArePrivateAndEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), ".history-cleared-") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
			t.Fatalf("retired history artifact %s info=%v err=%v", entry.Name(), info, err)
		}
		ownerOnly, err := historyPathIsOwnerOnly(path)
		if err != nil || !ownerOnly {
			t.Fatalf("retired history artifact %s owner-only=%v err=%v", entry.Name(), ownerOnly, err)
		}
	}
}
