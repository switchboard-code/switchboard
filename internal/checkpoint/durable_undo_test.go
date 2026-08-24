package checkpoint

import (
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type durableUndoFixture struct {
	workspace  string
	journalDir string
	child      string
	target     string
	recorder   *Recorder
	prepared   *PreparedUndo
}

func newDurableUndoFixture(t *testing.T) durableUndoFixture {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	journalDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(journalDir, "child.log")
	write(t, child, "staged child")
	target := filepath.Join(workspace, "target.txt")
	write(t, target, "before")

	recorder := NewRecorder()
	identity := TurnIdentity{SessionID: "source", OpeningMessage: 2}
	recorder.BeginTurn(identity.SessionID, identity.OpeningMessage, "retry this turn")
	recorder.Record(target)
	write(t, target, "after")
	prepared, err := recorder.PrepareDurableUndoCurrent(identity, journalDir, workspace, child, "test-child-identity")
	if err != nil {
		t.Fatal(err)
	}
	return durableUndoFixture{
		workspace: workspace, journalDir: journalDir, child: child,
		target: target, recorder: recorder, prepared: prepared,
	}
}

func TestConfiguredUndoLeavesNoWorkspaceOrSensitiveControlArtifacts(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	journalDir := filepath.Join(base, "sessions")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(workspace, "existing.txt")
	created := filepath.Join(workspace, "created.txt")
	write(t, existing, "before")
	recorder := NewRecorder()
	if err := recorder.ConfigureRestoreCleanup(journalDir, workspace); err != nil {
		t.Fatal(err)
	}
	recorder.Begin("normal configured undo")
	recorder.RecordState(existing, true, 0o644, []byte("before"))
	recorder.RecordState(created, false, 0, nil)
	write(t, existing, "after")
	write(t, created, "created")
	recorder.Commit(existing, true, 0o644, sha256.Sum256([]byte("after")))
	recorder.Commit(created, true, 0o644, sha256.Sum256([]byte("created")))
	restored, removed, _, failed, _, err := recorder.Undo()
	if err != nil || len(failed) != 0 || len(restored) != 1 || len(removed) != 1 {
		t.Fatalf("undo = restored %v removed %v failed %v, %v", restored, removed, failed, err)
	}
	if got := readBack(t, existing); got != "before" {
		t.Fatalf("restored file = %q", got)
	}
	if _, err := os.Lstat(created); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created file remains after undo: %v", err)
	}
	workspaceEntries, err := os.ReadDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range workspaceEntries {
		if strings.HasPrefix(entry.Name(), ".switchboard-") {
			t.Fatalf("successful undo left workspace artifact %s", entry.Name())
		}
	}
	controlEntries, err := os.ReadDir(journalDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range controlEntries {
		if strings.HasPrefix(entry.Name(), ".switchboard-quarantine-") {
			info, err := entry.Info()
			if err != nil {
				t.Fatal(err)
			}
			if !info.Mode().IsRegular() || info.Size() != 0 {
				t.Fatalf("control quarantine %s retained sensitive bytes or changed type", entry.Name())
			}
			continue
		}
		if strings.HasPrefix(entry.Name(), ".switchboard-") {
			t.Fatalf("successful undo left active control artifact %s", entry.Name())
		}
	}
}

func releaseDurableUndoFixture(t *testing.T, fixture durableUndoFixture) {
	t.Helper()
	if fixture.prepared.journal == nil || fixture.prepared.journal.file == nil || fixture.prepared.journal.lock == nil {
		t.Fatal("fixture has no live durable journal")
	}
	if err := fixture.prepared.journal.close(); err != nil {
		t.Fatal(err)
	}
}

func TestDurableUndoJournalExcludesConcurrentRecoveryAndCanAbort(t *testing.T) {
	fixture := newDurableUndoFixture(t)
	if _, err := RecoverDurableUndo(fixture.journalDir, fixture.workspace, func(string, string) (bool, error) {
		return false, nil
	}); !errors.Is(err, ErrDurableUndoLocked) {
		t.Fatalf("live journal recovery error = %v, want ErrDurableUndoLocked", err)
	}
	if got := readBack(t, fixture.target); got != "after" {
		t.Fatalf("locked recovery changed workspace to %q", got)
	}
	if err := fixture.prepared.AbortDurable(); err != nil {
		t.Fatal(err)
	}
	recovery, err := RecoverDurableUndo(fixture.journalDir, fixture.workspace, func(string, string) (bool, error) {
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Found {
		t.Fatalf("aborted journal remained discoverable: %+v", recovery)
	}
}

func TestDurableUndoRecoveryRollsUnpublishedWorkspaceForward(t *testing.T) {
	fixture := newDurableUndoFixture(t)
	releaseDurableUndoFixture(t, fixture)
	write(t, fixture.target, "before")

	recovery, err := RecoverDurableUndo(fixture.journalDir, fixture.workspace, func(path, identity string) (bool, error) {
		want, resolveErr := filepath.EvalSymlinks(fixture.child)
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		if path != want {
			t.Fatalf("publication path = %q, want %q", path, want)
		}
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !recovery.Found || recovery.Published || recovery.RolledForward != 1 || recovery.AlreadyPost != 0 {
		t.Fatalf("recovery = %+v", recovery)
	}
	if got := readBack(t, fixture.target); got != "after" {
		t.Fatalf("unpublished retry recovered %q, want post-image", got)
	}
}

func TestDurableUndoRecoveryRefusesRecreatedWorkspaceRoot(t *testing.T) {
	fixture := newDurableUndoFixture(t)
	releaseDurableUndoFixture(t, fixture)
	moved := fixture.workspace + ".original"
	if err := os.Rename(fixture.workspace, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(fixture.workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, fixture.target, "replacement workspace")

	called := false
	_, err := RecoverDurableUndo(fixture.journalDir, fixture.workspace, func(string, string) (bool, error) {
		called = true
		return false, nil
	})
	if !errors.Is(err, ErrStale) {
		t.Fatalf("recreated workspace recovery error = %v, want ErrStale", err)
	}
	if called {
		t.Fatal("publication was consulted after the workspace root changed identity")
	}
	if got := readBack(t, fixture.target); got != "replacement workspace" {
		t.Fatalf("recovery mutated replacement workspace to %q", got)
	}
	if _, statErr := os.Stat(filepath.Join(fixture.journalDir, durableUndoJournalName)); statErr != nil {
		t.Fatalf("identity refusal did not retain the recovery journal: %v", statErr)
	}
}

func TestDurableUndoRecoveryRevalidatesRootAfterPublicationCallback(t *testing.T) {
	fixture := newDurableUndoFixture(t)
	releaseDurableUndoFixture(t, fixture)
	moved := fixture.workspace + ".during-publication"
	boundBefore, err := os.Stat(fixture.workspace)
	if err != nil {
		t.Fatal(err)
	}
	retargetPrevented := false
	recovery, err := RecoverDurableUndo(fixture.journalDir, fixture.workspace, func(string, string) (bool, error) {
		if renameErr := os.Rename(fixture.workspace, moved); renameErr != nil {
			if !openDirectoryRetargetPrevented(renameErr) {
				return false, renameErr
			}
			boundAfter, statErr := os.Stat(fixture.workspace)
			_, movedErr := os.Lstat(moved)
			if statErr != nil || !errors.Is(movedErr, fs.ErrNotExist) || !os.SameFile(boundBefore, boundAfter) {
				return false, errors.Join(statErr, movedErr,
					errors.New("Windows retry-workspace retarget refusal did not retain the bound identity"))
			}
			retargetPrevented = true
			return true, nil
		}
		if err := os.Mkdir(fixture.workspace, 0o755); err != nil {
			return false, err
		}
		write(t, fixture.target, "replacement workspace")
		return true, nil
	})
	if retargetPrevented {
		if err != nil || !recovery.Found || !recovery.Published {
			t.Fatalf("Windows retry-workspace retarget refusal recovery = %+v, %v", recovery, err)
		}
		if got := readBack(t, fixture.target); got != "after" {
			t.Fatalf("Windows retry-workspace retarget refusal changed target to %q", got)
		}
		if _, statErr := os.Lstat(filepath.Join(fixture.journalDir, durableUndoJournalName)); !errors.Is(statErr, fs.ErrNotExist) {
			t.Fatalf("published retry journal remains after Windows retarget refusal: %v", statErr)
		}
		return
	}
	if !errors.Is(err, ErrStale) {
		t.Fatalf("post-callback workspace swap error = %v, want ErrStale", err)
	}
	if !recovery.Found || recovery.Published {
		t.Fatalf("post-callback workspace swap recovery = %+v", recovery)
	}
	if got := readBack(t, fixture.target); got != "replacement workspace" {
		t.Fatalf("recovery mutated replacement workspace to %q", got)
	}
	if _, statErr := os.Stat(filepath.Join(fixture.journalDir, durableUndoJournalName)); statErr != nil {
		t.Fatalf("root ambiguity did not retain the recovery journal: %v", statErr)
	}
}

func TestDurableUndoJournalRefusesWorkspaceSwapAfterPreparation(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	journalDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(journalDir, "child.log")
	target := filepath.Join(workspace, "target.txt")
	write(t, child, "staged child")
	write(t, target, "before")
	recorder := NewRecorder()
	identity := TurnIdentity{SessionID: "source", OpeningMessage: 7}
	recorder.BeginTurn(identity.SessionID, identity.OpeningMessage, "root swap after prepare")
	recorder.Record(target)
	write(t, target, "after")
	moved := workspace + ".original"
	recorder.beforeDurableJournalHook = func() error {
		if err := os.Rename(workspace, moved); err != nil {
			return err
		}
		if err := os.Mkdir(workspace, 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, []byte("after"), 0o644)
	}
	if _, err := recorder.PrepareDurableUndoCurrent(identity, journalDir, workspace, child, "test-child-identity"); !errors.Is(err, ErrStale) {
		t.Fatalf("workspace swap preparation error = %v, want ErrStale", err)
	}
	called := false
	recovery, err := RecoverDurableUndo(journalDir, workspace, func(string, string) (bool, error) {
		called = true
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if called || recovery.Found {
		t.Fatalf("workspace swap published a recoverable journal: called=%v recovery=%+v", called, recovery)
	}
	if got := readBack(t, target); got != "after" {
		t.Fatalf("replacement workspace changed to %q", got)
	}
}

func TestRecoveryCleansPreparingJournalButPreservesUnrecordedWorkspaceLookalike(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	journalDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(filepath.Join(workspace, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	checkpointTemp := filepath.Join(workspace, "nested", ".switchboard-undo-0123456789abcdef0123456789abcdef")
	journalTemp := filepath.Join(journalDir, durableUndoJournalName+".preparing-0123456789abcdef0123456789abcdef")
	journalLookalike := filepath.Join(journalDir, durableUndoJournalName+".preparing-user-not-a-switchboard-temp")
	exactUnowned := filepath.Join(journalDir, durableUndoJournalName+".preparing-fedcba9876543210fedcba9876543210")
	write(t, checkpointTemp, "sensitive pre-image")
	write(t, journalTemp, durableUndoJournalMagic+"partial journal with post-images")
	write(t, journalLookalike, "user-owned")
	write(t, exactUnowned, "no switchboard ownership marker")

	recovery, err := RecoverDurableUndo(journalDir, workspace, func(string, string) (bool, error) {
		t.Fatal("publication callback ran without a fixed journal")
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Found {
		t.Fatalf("temporary cleanup reported a pending transaction: %+v", recovery)
	}
	if got := readBack(t, checkpointTemp); got != "sensitive pre-image" {
		t.Fatalf("prefix-only workspace file was changed to %q", got)
	}
	if _, err := os.Lstat(journalTemp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned journal preparation remains: %v", err)
	}
	if got := readBack(t, journalLookalike); got != "user-owned" {
		t.Fatalf("journal lookalike was changed to %q", got)
	}
	if got := readBack(t, exactUnowned); got != "no switchboard ownership marker" {
		t.Fatalf("unowned exact-grammar journal file was changed to %q", got)
	}
}

func TestRecoveryCleansOnlyExactLedgerOwnedCheckpointTemporary(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	journalDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(workspace, "target.txt")
	write(t, target, "after")
	recorder := NewRecorder()
	if err := recorder.ConfigureRestoreCleanup(journalDir, workspace); err != nil {
		t.Fatal(err)
	}
	state := &fileState{
		existed: true, mode: 0o644, content: []byte("before"),
		after: fingerprintBytes(true, 0o644, []byte("after")),
	}
	lease, err := beginRestoreTempLease(recorder.restoreCleanup, target, state, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	tempPath := filepath.Join(workspace, lease.record.TempName)
	temp, err := os.OpenFile(tempPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := temp.WriteString("before"); err != nil {
		t.Fatal(err)
	}
	if err := temp.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := lease.bindTemp(temp, true); err != nil {
		t.Fatal(err)
	}
	if err := temp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.retain(); err != nil {
		t.Fatal(err)
	}
	lookalike := filepath.Join(workspace, ".switchboard-undo-fedcba9876543210fedcba9876543210")
	write(t, lookalike, "repo-owned")

	if _, err := RecoverDurableUndo(journalDir, workspace, func(string, string) (bool, error) {
		t.Fatal("publication callback ran without a retry journal")
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ledger-owned temporary remains: %v", err)
	}
	if got := readBack(t, lookalike); got != "repo-owned" {
		t.Fatalf("unrecorded lookalike was changed to %q", got)
	}
	entries, err := os.ReadDir(journalDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if isRestoreTempLedgerName(entry.Name()) {
			t.Fatalf("cleanup ledger %s remains", entry.Name())
		}
	}
}

func TestRecoveryNeverDeletesReservedNameWithoutRecordedInodeIdentity(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	journalDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(workspace, "target.txt")
	write(t, target, "after")
	recorder := NewRecorder()
	if err := recorder.ConfigureRestoreCleanup(journalDir, workspace); err != nil {
		t.Fatal(err)
	}
	state := &fileState{
		existed: true, mode: 0o644, content: []byte("before"),
		after: fingerprintBytes(true, 0o644, []byte("after")),
	}
	lease, err := beginRestoreTempLease(recorder.restoreCleanup, target, state, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	reserved := filepath.Join(workspace, lease.record.TempName)
	if _, err := lease.handle.file.Seek(0, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if _, err := lease.handle.file.WriteString(restoreTempLedgerMagic[:len(restoreTempLedgerMagic)/2]); err != nil {
		t.Fatal(err)
	}
	if err := lease.handle.file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := lease.retain(); err != nil {
		t.Fatal(err)
	}
	write(t, reserved, "repo-created after the owner crashed")

	if _, err := RecoverDurableUndo(journalDir, workspace, func(string, string) (bool, error) {
		t.Fatal("publication callback ran without a retry journal")
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, reserved); got != "repo-created after the owner crashed" {
		t.Fatalf("identity-less reserved name was changed to %q", got)
	}
}

func TestDurableUndoRecoveryCommitsPublishedWorkspace(t *testing.T) {
	fixture := newDurableUndoFixture(t)
	releaseDurableUndoFixture(t, fixture)
	write(t, fixture.target, "before")

	recovery, err := RecoverDurableUndo(fixture.journalDir, fixture.workspace, func(string, string) (bool, error) {
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !recovery.Found || !recovery.Published || recovery.RolledForward != 0 {
		t.Fatalf("recovery = %+v", recovery)
	}
	if got := readBack(t, fixture.target); got != "before" {
		t.Fatalf("published retry was rolled forward to %q", got)
	}
}

func TestPublishedDurableUndoCleanupFailureIsOnlyAWarning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("path replacement seam uses Unix rename behavior")
	}
	fixture := newDurableUndoFixture(t)
	releaseDurableUndoFixture(t, fixture)
	write(t, fixture.target, "before")
	var hookErr error
	durableUndoRecoveryHandleHook = func(handle *durableUndoHandle) {
		handle.afterRetire = func(path string) {
			original := path + ".original"
			if err := os.Rename(path, original); err != nil {
				hookErr = err
				return
			}
			hookErr = os.WriteFile(path, []byte("replacement"), 0o600)
		}
	}
	defer func() { durableUndoRecoveryHandleHook = nil }()
	recovery, err := RecoverDurableUndo(fixture.journalDir, fixture.workspace, func(string, string) (bool, error) {
		return true, nil
	})
	if err != nil {
		t.Fatalf("published cleanup warning blocked recovery: %v", err)
	}
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if !recovery.Found || !recovery.Published || !errors.Is(recovery.CleanupWarning, ErrStale) {
		t.Fatalf("published cleanup recovery = %+v", recovery)
	}
	if got := readBack(t, fixture.target); got != "before" {
		t.Fatalf("published cleanup warning changed committed workspace to %q", got)
	}
}

func TestUnpublishedDurableUndoCleanupFailureBlocksRecovery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("path replacement seam uses Unix rename behavior")
	}
	fixture := newDurableUndoFixture(t)
	releaseDurableUndoFixture(t, fixture)
	write(t, fixture.target, "before")
	var hookErr error
	durableUndoRecoveryHandleHook = func(handle *durableUndoHandle) {
		handle.afterRetire = func(path string) {
			original := path + ".original"
			if err := os.Rename(path, original); err != nil {
				hookErr = err
				return
			}
			hookErr = os.WriteFile(path, []byte("replacement"), 0o600)
		}
	}
	defer func() { durableUndoRecoveryHandleHook = nil }()
	recovery, err := RecoverDurableUndo(fixture.journalDir, fixture.workspace, func(string, string) (bool, error) {
		return false, nil
	})
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if !errors.Is(err, ErrDurableUndoRecoveryRequired) || !errors.Is(err, ErrStale) {
		t.Fatalf("unpublished cleanup error = %v, want recovery-required stale refusal", err)
	}
	if !recovery.Found || recovery.Published || recovery.RolledForward != 1 {
		t.Fatalf("unpublished cleanup recovery = %+v", recovery)
	}
	if got := readBack(t, fixture.target); got != "after" {
		t.Fatalf("unpublished cleanup failure left workspace at %q, want post-image", got)
	}
	lock, lockErr := openDurableUndoLock(fixture.journalDir)
	if lockErr != nil {
		t.Fatalf("failed recovery leaked its workspace lock: %v", lockErr)
	}
	if closeErr := lock.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
}

func TestJournalPublicationReopenRefusesAPathReplacementWithoutDeletingIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("deterministic replacement seam uses Unix rename behavior")
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	journalDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(journalDir, "child.log")
	target := filepath.Join(workspace, "target.txt")
	write(t, child, "staged child")
	write(t, target, "before")
	recorder := NewRecorder()
	identity := TurnIdentity{SessionID: "source", OpeningMessage: 0}
	recorder.BeginTurn(identity.SessionID, identity.OpeningMessage, "replace journal path")
	recorder.Record(target)
	write(t, target, "after")
	journalPath := filepath.Join(journalDir, durableUndoJournalName)
	originalPath := journalPath + ".original"
	recorder.durableUndoHook = func(boundary durableUndoBoundary, _ int) {
		if boundary != durableUndoJournalAfterInstall {
			return
		}
		if err := os.Rename(journalPath, originalPath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(journalPath, []byte("foreign replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := recorder.PrepareDurableUndoCurrent(identity, journalDir, workspace, child, "test-child-identity")
	if !errors.Is(err, ErrDurableUndoRecoveryRequired) || !errors.Is(err, ErrStale) {
		t.Fatalf("reopen replacement error = %v", err)
	}
	data, readErr := os.ReadFile(journalPath)
	if readErr != nil || string(data) != "foreign replacement" {
		t.Fatalf("foreign replacement was altered or removed: %q, %v", data, readErr)
	}
	if _, statErr := os.Stat(originalPath); statErr != nil {
		t.Fatalf("published original was not retained for diagnosis: %v", statErr)
	}
}

func TestDurableUndoRecoveryRefusesThirdStateWithoutWriting(t *testing.T) {
	fixture := newDurableUndoFixture(t)
	releaseDurableUndoFixture(t, fixture)
	write(t, fixture.target, "external edit")

	_, err := RecoverDurableUndo(fixture.journalDir, fixture.workspace, func(string, string) (bool, error) {
		return false, nil
	})
	if !errors.Is(err, ErrStale) {
		t.Fatalf("third-state recovery error = %v, want ErrStale", err)
	}
	if got := readBack(t, fixture.target); got != "external edit" {
		t.Fatalf("third-state refusal changed workspace to %q", got)
	}
	if _, statErr := os.Stat(filepath.Join(fixture.journalDir, durableUndoJournalName)); statErr != nil {
		t.Fatalf("third-state refusal removed journal: %v", statErr)
	}
}

func TestDurableUndoRecoveryPreflightsEveryPathBeforeWriting(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	journalDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(journalDir, "child.log")
	write(t, child, "staged child")
	first := filepath.Join(workspace, "a-first.txt")
	third := filepath.Join(workspace, "z-third.txt")
	write(t, first, "first before")
	write(t, third, "third before")
	recorder := NewRecorder()
	identity := TurnIdentity{SessionID: "source", OpeningMessage: 2}
	recorder.BeginTurn(identity.SessionID, identity.OpeningMessage, "two paths")
	recorder.Record(first)
	recorder.Record(third)
	write(t, first, "first after")
	write(t, third, "third after")
	prepared, err := recorder.PrepareDurableUndoCurrent(identity, journalDir, workspace, child, "test-child-identity")
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.journal.close(); err != nil {
		t.Fatal(err)
	}
	write(t, first, "first before")
	write(t, third, "unrecognised external edit")

	_, err = RecoverDurableUndo(journalDir, workspace, func(string, string) (bool, error) { return false, nil })
	if !errors.Is(err, ErrStale) {
		t.Fatalf("multi-path third-state error = %v, want ErrStale", err)
	}
	if got := readBack(t, first); got != "first before" {
		t.Fatalf("recovery wrote an earlier path before finding the third state: %q", got)
	}
	if got := readBack(t, third); got != "unrecognised external edit" {
		t.Fatalf("recovery overwrote the third state: %q", got)
	}
}

func TestDurableUndoRecoveryBoundsThirdStateFingerprint(t *testing.T) {
	fixture := newDurableUndoFixture(t)
	releaseDurableUndoFixture(t, fixture)
	if err := os.WriteFile(fixture.target, []byte(strings.Repeat("x", maxFileBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := RecoverDurableUndo(fixture.journalDir, fixture.workspace, func(string, string) (bool, error) {
		return false, nil
	})
	if !errors.Is(err, ErrSnapshotTooLarge) {
		t.Fatalf("oversized third-state recovery error = %v, want ErrSnapshotTooLarge", err)
	}
	if _, statErr := os.Stat(filepath.Join(fixture.journalDir, durableUndoJournalName)); statErr != nil {
		t.Fatalf("oversized refusal removed its journal: %v", statErr)
	}
}

func TestDurableUndoRecoveryRejectsCorruptJournal(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	journalDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(journalDir, durableUndoJournalName)
	if err := os.WriteFile(journalPath, []byte("not a retry journal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverDurableUndo(journalDir, workspace, func(string, string) (bool, error) {
		return false, nil
	}); err == nil || !strings.Contains(err.Error(), "invalid header") {
		t.Fatalf("corrupt journal recovery error = %v", err)
	}
	if _, err := os.Stat(journalPath); err != nil {
		t.Fatalf("corrupt journal was not retained: %v", err)
	}
}

func TestDurableUndoRecoveryReleasesLockForRejectedJournal(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	journalDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(journalDir, durableUndoJournalName), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverDurableUndo(journalDir, workspace, func(string, string) (bool, error) {
		return false, nil
	}); err == nil || !strings.Contains(err.Error(), "not a bounded regular file") {
		t.Fatalf("non-regular journal recovery error = %v", err)
	}
	lock, err := openDurableUndoLock(journalDir)
	if err != nil {
		t.Fatalf("rejected journal leaked its workspace lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDurableUndoCleanupDoesNotDeleteRetiredReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("path replacement seam uses Unix rename-over-open behavior")
	}
	fixture := newDurableUndoFixture(t)
	var retired, original string
	var hookErr error
	fixture.prepared.journal.afterRetire = func(path string) {
		retired = path
		original = path + ".original"
		if err := os.Rename(path, original); err != nil {
			hookErr = err
			return
		}
		hookErr = os.WriteFile(path, []byte("replacement must survive"), 0o600)
	}
	err := fixture.prepared.AbortDurable()
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if !errors.Is(err, ErrStale) {
		t.Fatalf("retired replacement cleanup error = %v, want ErrStale", err)
	}
	if data, readErr := os.ReadFile(retired); readErr != nil || string(data) != "replacement must survive" {
		t.Fatalf("retired replacement was deleted: content=%q err=%v", data, readErr)
	}
	if data, readErr := os.ReadFile(original); readErr != nil || !strings.HasPrefix(string(data), durableUndoJournalMagic) {
		t.Fatalf("exact journal evidence was changed: content=%q err=%v", data, readErr)
	}
}

func TestDurableUndoRequiresCommitAndTreatsPublishedCleanupAsWarning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not permit replacing an open journal path")
	}
	fixture := newDurableUndoFixture(t)
	if _, err := fixture.prepared.Apply(); err == nil || !strings.Contains(err.Error(), "requires a publication commit") {
		t.Fatalf("durable Apply error = %v", err)
	}

	result, err := fixture.prepared.ApplyAndCommit(func() error {
		// Replace the linked name after the commit point. The live descriptor
		// still identifies the original journal, so cleanup must refuse to
		// unlink the replacement without undoing the committed workspace.
		journalPath := filepath.Join(fixture.journalDir, durableUndoJournalName)
		if err := os.Remove(journalPath); err != nil {
			return err
		}
		return os.WriteFile(journalPath, []byte("replacement"), 0o600)
	})
	if err != nil {
		t.Fatalf("published cleanup failure became adoption failure: %v", err)
	}
	if result.CleanupWarning == nil || !errors.Is(result.CleanupWarning, ErrStale) {
		t.Fatalf("cleanup warning = %v, want ErrStale", result.CleanupWarning)
	}
	if got := readBack(t, fixture.target); got != "before" {
		t.Fatalf("cleanup warning rolled committed workspace to %q", got)
	}
}

func TestRestoreTempLedgerFallsBackOnlyFromTornFinalFrame(t *testing.T) {
	first := restoreTempLedgerRecord{Version: 1, TempIdentity: "first"}
	second := restoreTempLedgerRecord{Version: 1, TempIdentity: "second"}
	third := restoreTempLedgerRecord{Version: 1, TempIdentity: "third"}
	frame1, err := encodeRestoreTempLedger(first)
	if err != nil {
		t.Fatal(err)
	}
	frame2, err := encodeRestoreTempLedger(second)
	if err != nil {
		t.Fatal(err)
	}
	frame3, err := encodeRestoreTempLedger(third)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), frame3...)
	corrupt[len(restoreTempLedgerMagic)+1] = 'z'

	tests := []struct {
		name    string
		data    []byte
		want    string
		wantErr bool
	}{
		{name: "torn second frame", data: append(append([]byte(nil), frame1...), corrupt...), want: "first"},
		{name: "torn third frame", data: append(append(append([]byte(nil), frame1...), frame2...), corrupt...), want: "second"},
		{name: "partial third frame", data: append(append(append([]byte(nil), frame1...), frame2...), []byte("switchboard-undo")...), want: "second"},
		{name: "invalid first frame", data: corrupt, wantErr: true},
		{name: "invalid interior frame", data: append(append(append([]byte(nil), frame1...), corrupt...), frame3...), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "ledger-")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if _, err := file.Write(test.data); err != nil {
				t.Fatal(err)
			}
			got, err := decodeRestoreTempLedger(file)
			if test.wantErr {
				if err == nil {
					t.Fatalf("decode = %+v, nil; want corruption error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.TempIdentity != test.want {
				t.Fatalf("decoded identity = %q, want %q", got.TempIdentity, test.want)
			}
		})
	}
}
