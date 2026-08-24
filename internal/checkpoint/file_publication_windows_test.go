//go:build windows

package checkpoint

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
)

func TestPublishFileCASPreparedInstallsProtectedDACLBeforePublication(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	journalDir := filepath.Join(base, "sessions")
	for _, dir := range []string{workspace, journalDir} {
		if err := fileprivacy.EnsurePrivateDir(dir); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(workspace, "state.json")
	before, err := fileprivacy.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := before.WriteString("before"); err != nil {
		_ = before.Close()
		t.Fatal(err)
	}
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}
	parent, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	recorder := NewRecorder()
	if err := recorder.ConfigureRestoreCleanup(journalDir, workspace); err != nil {
		t.Fatal(err)
	}
	recorder.Begin("private state")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	recorder.RecordState(path, true, info.Mode(), []byte("before"))
	published, err := recorder.PublishFileCASPrepared(
		context.Background(), path, parent, filepath.Base(path),
		true, info.Mode(), []byte("before"), 0o600, []byte("after"),
		fileprivacy.Secure, nil,
	)
	if err != nil || !published {
		t.Fatalf("PublishFileCASPrepared() = published %v, %v", published, err)
	}
	recorder.Commit(path, true, 0o600, sha256.Sum256([]byte("after")))
	file, err := fileprivacy.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ownerOnly, ownerErr := fileprivacy.IsOwnerOnly(file)
	closeErr := file.Close()
	if ownerErr != nil || closeErr != nil || !ownerOnly {
		t.Fatalf("published owner-only=%v ownerErr=%v closeErr=%v", ownerOnly, ownerErr, closeErr)
	}
}

func TestPublishFileCASWindowsPhaseOutcomes(t *testing.T) {
	injected := errors.New("injected Windows publication phase failure")
	for _, test := range []struct {
		name          string
		phase         windowsHandlePhase
		wantPublished bool
		wantTarget    string
	}{
		{name: "stage one before flush rolls back", phase: windowsBeforeSourceStagingFlush, wantTarget: "before"},
		{name: "stage one after flush rolls back", phase: windowsAfterSourceStagingFlush, wantTarget: "before"},
		{name: "stage two before flush rolls back", phase: windowsBeforeDisplacedStagingFlush, wantTarget: "before"},
		{name: "stage two after flush rolls back", phase: windowsAfterDisplacedStagingFlush, wantTarget: "before"},
		{name: "stage three before flush is published", phase: windowsBeforeSourcePublicationFlush, wantPublished: true, wantTarget: "after"},
		{name: "stage three after flush is published", phase: windowsAfterSourcePublicationFlush, wantPublished: true, wantTarget: "after"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace, journalDir, path, parent, recorder := newWindowsFilePublicationFixture(t)
			windowsHandlePhaseTestHook = func(phase windowsHandlePhase) error {
				if phase == test.phase {
					return injected
				}
				return nil
			}
			t.Cleanup(func() { windowsHandlePhaseTestHook = nil })

			published, err := recorder.PublishFileCAS(
				context.Background(), path, parent, filepath.Base(path),
				true, 0o644, []byte("before"), 0o644, []byte("after"), nil,
			)
			windowsHandlePhaseTestHook = nil
			if published != test.wantPublished || !errors.Is(err, injected) {
				t.Fatalf("PublishFileCAS() = published %v, %v; want published %v with injected error", published, err, test.wantPublished)
			}
			if got := readBack(t, path); got != test.wantTarget {
				t.Fatalf("target = %q, want %q", got, test.wantTarget)
			}
			if published {
				recorder.Commit(path, true, 0o644, sha256.Sum256([]byte("after")))
				if !hasPublicationTempLedger(t, journalDir) {
					t.Fatal("published phase error did not retain recovery ledger")
				}
				if _, err := RecoverDurableUndo(journalDir, workspace, func(string, string) (bool, error) {
					return false, nil
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				recorder.Abort(path)
			}
			assertNoPublicationArtifacts(t, workspace, journalDir)
		})
	}
}

func TestPublishFileCASWindowsRollbackFailureStaysPublishedAndRecoverable(t *testing.T) {
	injected := errors.New("injected Windows exchange failure")
	workspace, journalDir, path, parent, recorder := newWindowsFilePublicationFixture(t)
	var blocker string
	windowsHandlePhaseTestHook = func(phase windowsHandlePhase) error {
		if phase != windowsBeforeSourceStagingFlush {
			return nil
		}
		windowsHandlePhaseTestHook = nil
		blocker = filepath.Join(workspace, currentPublicationTempName(t, journalDir))
		if err := os.WriteFile(blocker, []byte("foreign blocker"), 0o644); err != nil {
			t.Fatalf("installing rollback blocker: %v", err)
		}
		return injected
	}
	t.Cleanup(func() { windowsHandlePhaseTestHook = nil })

	published, err := recorder.PublishFileCAS(
		context.Background(), path, parent, filepath.Base(path),
		true, 0o644, []byte("before"), 0o644, []byte("after"), nil,
	)
	windowsHandlePhaseTestHook = nil
	if !published || !errors.Is(err, injected) || !strings.Contains(err.Error(), "rolling back") {
		t.Fatalf("PublishFileCAS() = published %v, %v; want ambiguous published rollback failure", published, err)
	}
	recorder.Commit(path, true, 0o644, sha256.Sum256([]byte("after")))
	if got := readBack(t, path); got != "before" {
		t.Fatalf("currently linked target = %q, want conservatively ambiguous expected state", got)
	}
	if got := readBack(t, blocker); got != "foreign blocker" {
		t.Fatalf("rollback blocker = %q", got)
	}
	if !hasPublicationTempLedger(t, journalDir) {
		t.Fatal("rollback failure did not retain recovery ledger")
	}
	if _, recoveryErr := RecoverDurableUndo(journalDir, workspace, func(string, string) (bool, error) {
		return false, nil
	}); !errors.Is(recoveryErr, ErrStale) {
		t.Fatalf("recovery with foreign blocker = %v, want stale refusal", recoveryErr)
	}
	if got := readBack(t, blocker); got != "foreign blocker" {
		t.Fatalf("failed recovery changed rollback blocker to %q", got)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverDurableUndo(journalDir, workspace, func(string, string) (bool, error) {
		return false, nil
	}); err != nil {
		t.Fatalf("recovery after blocker removal: %v", err)
	}
	if got := readBack(t, path); got != "before" {
		t.Fatalf("recovered target = %q, want before", got)
	}
	assertNoPublicationArtifacts(t, workspace, journalDir)
}

func newWindowsFilePublicationFixture(t *testing.T) (workspace, journalDir, path string, parent *os.Root, recorder *Recorder) {
	t.Helper()
	base := t.TempDir()
	workspace = filepath.Join(base, "workspace")
	journalDir = filepath.Join(base, "sessions")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(workspace, "target.txt")
	write(t, path, "before")
	var err error
	parent, err = os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parent.Close() })
	retirement, err := os.OpenRoot(journalDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureRetirementCompatible(parent, retirement); err != nil {
		_ = retirement.Close()
		t.Fatal(err)
	}
	if err := retirement.Close(); err != nil {
		t.Fatal(err)
	}
	recorder = NewRecorder()
	if err := recorder.ConfigureRestoreCleanup(journalDir, workspace); err != nil {
		t.Fatal(err)
	}
	recorder.Begin("Windows file publication")
	recorder.RecordState(path, true, 0o644, []byte("before"))
	return workspace, journalDir, path, parent, recorder
}

func currentPublicationTempName(t *testing.T, journalDir string) string {
	t.Helper()
	entries, err := os.ReadDir(journalDir)
	if err != nil {
		t.Fatal(err)
	}
	var tempName string
	for _, entry := range entries {
		if !isRestoreTempLedgerName(entry.Name()) {
			continue
		}
		if tempName != "" {
			t.Fatal("more than one publication cleanup ledger is active")
		}
		// Windows byte-range locks are mandatory, so the active recovery
		// ledger cannot be read through a second handle at this test seam. Its
		// validated random suffix is deliberately shared with the temporary.
		tempName = ".switchboard-undo-" + strings.TrimPrefix(entry.Name(), restoreTempLedgerPrefix)
	}
	if tempName == "" {
		t.Fatal("publication cleanup ledger not found")
	}
	return tempName
}
