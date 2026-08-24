//go:build unix

package checkpoint

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const cleanupRaceSentinel = "foreign sentinel must survive"

func TestRestoreCleanupPreservesSwappedTemporaryName(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := NewRecorder()
	recorder.Begin("temporary cleanup substitution")
	recorder.RecordState(target, true, 0o600, []byte("before"))
	if err := os.WriteFile(target, []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder.Commit(target, true, 0o600, sha256.Sum256([]byte("after")))

	var tempPath, captured string
	recorder.beforeReplaceHook = func() error {
		entries, err := os.ReadDir(root)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !isRestoreTempName(entry.Name()) {
				continue
			}
			tempPath = filepath.Join(root, entry.Name())
			captured = tempPath + ".captured"
			if err := os.Rename(tempPath, captured); err != nil {
				return err
			}
			return os.WriteFile(tempPath, []byte(cleanupRaceSentinel), 0o600)
		}
		return errors.New("restore temporary was not visible at the publication seam")
	}

	outcome, _, err := recorder.UndoFile(target)
	if outcome.Published || !errors.Is(err, ErrStale) {
		t.Fatalf("temporary substitution outcome=%+v error=%v, want unpublished stale refusal", outcome, err)
	}
	if data, readErr := os.ReadFile(target); readErr != nil || string(data) != "after" {
		t.Fatalf("temporary substitution changed target: content=%q err=%v", data, readErr)
	}
	assertCleanupSentinel(t, tempPath)
	assertRetainedCheckpointFile(t, captured, "before")
}

func TestDurableUndoCleanupPreservesPreRetireReplacement(t *testing.T) {
	fixture := newDurableUndoFixture(t)
	journal := filepath.Join(fixture.journalDir, durableUndoJournalName)
	captured := journal + ".captured"
	var hookErr error
	fixture.prepared.journal.beforeRetire = func(path string) {
		if err := os.Rename(path, captured); err != nil {
			hookErr = err
			return
		}
		hookErr = os.WriteFile(path, []byte(cleanupRaceSentinel), 0o600)
	}

	err := fixture.prepared.AbortDurable()
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if !errors.Is(err, ErrDurableUndoRecoveryRequired) || !errors.Is(err, ErrStale) {
		t.Fatalf("pre-retire substitution error = %v, want recovery-required stale refusal", err)
	}
	assertCleanupSentinel(t, journal)
	data, readErr := os.ReadFile(captured)
	if readErr != nil || !strings.HasPrefix(string(data), durableUndoJournalMagic) {
		t.Fatalf("exact journal evidence was changed: content=%q err=%v", data, readErr)
	}
}

func TestCleanupExactRestoreTempPreservesPreRetireReplacement(t *testing.T) {
	workspace := t.TempDir()
	target := filepath.Join(workspace, "target.txt")
	if err := os.WriteFile(target, []byte("post-image"), 0o600); err != nil {
		t.Fatal(err)
	}
	name := ".switchboard-undo-0123456789abcdef0123456789abcdef"
	tempPath := filepath.Join(workspace, name)
	temp, err := os.OpenFile(tempPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := temp.WriteString("sensitive pre-image"); err != nil {
		t.Fatal(err)
	}
	identity, err := boundOpenFileIdentity(temp)
	if err != nil {
		t.Fatal(err)
	}
	if err := temp.Close(); err != nil {
		t.Fatal(err)
	}

	captured := tempPath + ".captured"
	var hookErr error
	cleanupExactRestoreTempBeforeRetire = func(gotTarget, gotName string) {
		if gotTarget != target || gotName != name {
			hookErr = errors.New("cleanup hook received the wrong target")
			return
		}
		if err := os.Rename(tempPath, captured); err != nil {
			hookErr = err
			return
		}
		hookErr = os.WriteFile(tempPath, []byte(cleanupRaceSentinel), 0o600)
	}
	t.Cleanup(func() { cleanupExactRestoreTempBeforeRetire = nil })

	scope, err := openRestoreScope(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer scope.close()
	err = cleanupExactRestoreTemp(scope, nil, target, name, restoreCleanupIdentity{value: identity, owned: true})
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if !errors.Is(err, ErrStale) {
		t.Fatalf("exact temporary substitution error = %v, want ErrStale", err)
	}
	assertCleanupSentinel(t, tempPath)
	assertRetainedCheckpointFile(t, captured, "sensitive pre-image")
}

func TestCleanupPreparingJournalPreservesPreRetireReplacement(t *testing.T) {
	dir := t.TempDir()
	name := durableUndoJournalName + ".preparing-0123456789abcdef0123456789abcdef"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(durableUndoJournalMagic+"sensitive journal payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	captured := path + ".captured"
	var hookErr error
	cleanupPreparingBeforeRetire = func(gotPath string) {
		if gotPath != path {
			hookErr = errors.New("preparing cleanup hook received the wrong path")
			return
		}
		if err := os.Rename(path, captured); err != nil {
			hookErr = err
			return
		}
		hookErr = os.WriteFile(path, []byte(cleanupRaceSentinel), 0o600)
	}
	t.Cleanup(func() { cleanupPreparingBeforeRetire = nil })

	err := cleanupPreparingJournals(dir)
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if !errors.Is(err, ErrStale) {
		t.Fatalf("preparing journal substitution error = %v, want ErrStale", err)
	}
	assertCleanupSentinel(t, path)
	assertRetainedCheckpointFile(t, captured, durableUndoJournalMagic+"sensitive journal payload")
}

func TestRetireBoundOpenFilePreservesPostRenameReplacement(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	const name = "owned-checkpoint"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("sensitive checkpoint bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := root.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	var sentinelPath, captured string
	var hookErr error
	err = retireBoundOpenFile(root, name, file, true, nil, func(quarantine string) {
		sentinelPath = filepath.Join(dir, quarantine)
		captured = sentinelPath + ".captured"
		if err := os.Rename(sentinelPath, captured); err != nil {
			hookErr = err
			return
		}
		hookErr = os.WriteFile(sentinelPath, []byte(cleanupRaceSentinel), 0o600)
	})
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if !errors.Is(err, ErrStale) {
		t.Fatalf("post-rename substitution error = %v, want ErrStale", err)
	}
	assertCleanupSentinel(t, sentinelPath)
	assertRetainedCheckpointFile(t, captured, "sensitive checkpoint bytes")
}

func TestCrossFilesystemRetirementRefusesBeforePublication(t *testing.T) {
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
	write(t, target, "before")
	recorder := NewRecorder()
	if err := recorder.ConfigureRestoreCleanup(journalDir, workspace); err != nil {
		t.Fatal(err)
	}
	recorder.Begin("cross-filesystem retirement")
	recorder.RecordState(target, true, 0o644, []byte("before"))
	write(t, target, "after")
	recorder.Commit(target, true, 0o644, sha256.Sum256([]byte("after")))
	retirementCompatibilityTestHook = func() error { return syscall.EXDEV }
	outcome, _, err := recorder.UndoFile(target)
	retirementCompatibilityTestHook = nil
	if outcome.Published || err == nil {
		t.Fatalf("cross-filesystem undo = %+v, %v; want unpublished refusal", outcome, err)
	}
	if got := readBack(t, target); got != "after" {
		t.Fatalf("cross-filesystem refusal changed target to %q", got)
	}
	recovery, err := RecoverDurableUndo(journalDir, workspace, func(string, string) (bool, error) {
		t.Fatal("publication callback ran without a retry journal")
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Found {
		t.Fatalf("ordinary undo cleanup reported a retry journal: %+v", recovery)
	}
	entries, err := os.ReadDir(journalDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if isRestoreTempLedgerName(entry.Name()) {
			t.Fatalf("cross-filesystem cleanup retained active ledger %s", entry.Name())
		}
	}
	workspaceEntries, err := os.ReadDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range workspaceEntries {
		if strings.HasPrefix(entry.Name(), ".switchboard-") {
			t.Fatalf("cross-filesystem refusal left workspace artifact %s", entry.Name())
		}
	}
}

func TestOwnedTemporaryHardlinkIsNeverScrubbed(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	sinkDir := filepath.Join(base, "sessions")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sinkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const name = ".switchboard-undo-0123456789abcdef0123456789abcdef"
	path := filepath.Join(workspace, name)
	outside := filepath.Join(base, "outside-hardlink")
	write(t, path, "sensitive checkpoint bytes")
	if err := os.Link(path, outside); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	sink, err := os.OpenRoot(sinkDir)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	file, err := root.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := retireBoundOpenFileTo(root, sink, name, file, true, nil, nil); !errors.Is(err, ErrStale) {
		t.Fatalf("hardlinked owned temp retirement error = %v, want ErrStale", err)
	}
	if got := readBack(t, outside); got != "sensitive checkpoint bytes" {
		t.Fatalf("outside hardlink was scrubbed to %q", got)
	}
	retired := filepath.Join(sinkDir, retiredSinkName(name))
	if got := readBack(t, retired); got != "sensitive checkpoint bytes" {
		t.Fatalf("exact retirement evidence was not retained: %q", got)
	}
	outsideInfo, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	retiredInfo, err := os.Stat(retired)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(outsideInfo, retiredInfo) {
		t.Fatal("trusted retirement name no longer identifies the hardlinked checkpoint inode")
	}
}

func TestOwnedTemporaryHardlinkRetainsLedgerAndSinkEvidence(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	sinkDir := filepath.Join(base, "sessions")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sinkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(workspace, "target.txt")
	write(t, target, "before")
	recorder := NewRecorder()
	if err := recorder.ConfigureRestoreCleanup(sinkDir, workspace); err != nil {
		t.Fatal(err)
	}
	recorder.Begin("hardlinked owned temporary")
	recorder.RecordState(target, true, 0o644, []byte("before"))
	write(t, target, "after")
	recorder.Commit(target, true, 0o644, sha256.Sum256([]byte("after")))

	outside := filepath.Join(base, "outside-hardlink")
	var tempName string
	recorder.publicationSeamHook = func() error {
		entries, err := os.ReadDir(workspace)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if isRestoreTempName(entry.Name()) {
				tempName = entry.Name()
				if err := os.Link(filepath.Join(workspace, tempName), outside); err != nil {
					return err
				}
				return errors.New("stop before publication")
			}
		}
		return errors.New("checkpoint temporary was not visible")
	}
	outcome, _, err := recorder.UndoFile(target)
	if outcome.Published || !errors.Is(err, ErrStale) {
		t.Fatalf("hardlinked temporary undo = %+v, %v; want unpublished recovery-required error", outcome, err)
	}
	if got := readBack(t, target); got != "after" {
		t.Fatalf("failed undo changed target to %q", got)
	}
	retired := filepath.Join(sinkDir, retiredSinkName(tempName))
	if got := readBack(t, retired); got != "before" {
		t.Fatalf("trusted retirement evidence = %q, want pre-image", got)
	}
	if got := readBack(t, outside); got != "before" {
		t.Fatalf("outside hardlink was scrubbed to %q", got)
	}
	ledgerFound := false
	entries, err := os.ReadDir(sinkDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		ledgerFound = ledgerFound || isRestoreTempLedgerName(entry.Name())
	}
	if !ledgerFound {
		t.Fatal("failed cleanup did not retain its exact cleanup ledger")
	}
	if _, err := RecoverDurableUndo(sinkDir, workspace, func(string, string) (bool, error) {
		t.Fatal("retry publication callback ran while cleanup required recovery")
		return false, nil
	}); !errors.Is(err, ErrStale) {
		t.Fatalf("startup cleanup error = %v, want retained hardlink evidence", err)
	}
	if got := readBack(t, retired); got != "before" {
		t.Fatalf("startup cleanup removed trusted evidence: %q", got)
	}
}

func assertCleanupSentinel(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != cleanupRaceSentinel {
		t.Fatalf("foreign sentinel was changed or unlinked: content=%q err=%v", data, err)
	}
}

func assertRetainedCheckpointFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatalf("exact checkpoint evidence was changed: content=%q err=%v", data, err)
	}
}
