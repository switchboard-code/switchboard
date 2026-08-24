package checkpoint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishFileCASReplacesAndCreatesWithoutArtifacts(t *testing.T) {
	for _, test := range []struct {
		name     string
		existed  bool
		expected string
		desired  string
	}{
		{name: "replace", existed: true, expected: "before", desired: "after"},
		{name: "create", desired: "created"},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			workspace := filepath.Join(base, "workspace")
			journalDir := filepath.Join(base, "sessions")
			if err := os.Mkdir(workspace, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(journalDir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(workspace, "target.txt")
			if test.existed {
				write(t, path, test.expected)
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
			recorder.Begin(test.name)
			recorder.RecordState(path, test.existed, 0o644, []byte(test.expected))
			published, err := recorder.PublishFileCAS(
				context.Background(), path, parent, filepath.Base(path),
				test.existed, 0o644, []byte(test.expected), 0o644, []byte(test.desired), nil,
			)
			if err != nil || !published {
				t.Fatalf("PublishFileCAS() = published %v, %v", published, err)
			}
			recorder.Commit(path, true, 0o644, sha256.Sum256([]byte(test.desired)))
			if got := readBack(t, path); got != test.desired {
				t.Fatalf("target = %q, want %q", got, test.desired)
			}
			assertNoPublicationArtifacts(t, workspace, journalDir)
		})
	}
}

func TestRecoverFilePublicationCleanupReconcilesPublishedReplacement(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	journalDir := filepath.Join(base, "sessions")
	for _, dir := range []string{workspace, journalDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(workspace, "target.txt")
	write(t, path, "before")
	parent, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()

	recorder := NewRecorder()
	if err := recorder.ConfigureRestoreCleanup(journalDir, workspace); err != nil {
		t.Fatal(err)
	}
	recorder.Begin("standalone publication recovery")
	recorder.RecordState(path, true, 0o644, []byte("before"))
	injected := errors.New("injected post-publication verification failure")
	recorder.afterReplaceHook = func() error { return injected }
	published, err := recorder.PublishFileCAS(
		context.Background(), path, parent, filepath.Base(path),
		true, 0o644, []byte("before"), 0o644, []byte("after"), nil,
	)
	if !published || !errors.Is(err, injected) {
		t.Fatalf("PublishFileCAS() = published %v, %v; want published injected failure", published, err)
	}
	recorder.Commit(path, true, 0o644, sha256.Sum256([]byte("after")))
	if got := readBack(t, path); got != "after" {
		t.Fatalf("published target = %q, want after", got)
	}

	if err := RecoverFilePublicationCleanup(journalDir, workspace); err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, path); got != "after" {
		t.Fatalf("recovery changed published target to %q", got)
	}
	assertNoPublicationArtifacts(t, workspace, journalDir)
}

func TestRecoverFilePublicationCleanupSupportsEightMiBStandaloneState(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	journalDir := filepath.Join(base, "sessions")
	for _, dir := range []string{workspace, journalDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(workspace, "state.json")
	write(t, path, "before")
	parent, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	desired := bytes.Repeat([]byte("x"), maxStandaloneFilePublicationBytes)
	recorder := NewRecorder()
	if err := recorder.ConfigureRestoreCleanup(journalDir, workspace); err != nil {
		t.Fatal(err)
	}
	recorder.Begin("large standalone publication recovery")
	if err := recorder.recordStateForFilePublication(
		path, true, 0o644, []byte("before"), maxStandaloneFilePublicationBytes,
	); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected post-publication cleanup retention")
	recorder.afterReplaceHook = func() error { return injected }
	published, err := recorder.publishFileCASPrepared(
		context.Background(), path, parent, filepath.Base(path),
		true, 0o644, []byte("before"), 0o600, desired,
		maxStandaloneFilePublicationBytes, nil, nil,
	)
	if !published || !errors.Is(err, injected) {
		t.Fatalf("large standalone publication = published %v, %v", published, err)
	}
	recorder.Commit(path, true, 0o600, sha256.Sum256(desired))
	if err := RecoverFilePublicationCleanup(journalDir, workspace); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != int64(len(desired)) {
		t.Fatalf("recovered large state size = %v, %v", info, err)
	}
	assertNoPublicationArtifacts(t, workspace, journalDir)
}

func TestPublishFileCASPreparedFailureLeavesNoPublicationArtifacts(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	journalDir := filepath.Join(base, "sessions")
	for _, dir := range []string{workspace, journalDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(workspace, "target.txt")
	write(t, path, "before")
	parent, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	recorder := NewRecorder()
	if err := recorder.ConfigureRestoreCleanup(journalDir, workspace); err != nil {
		t.Fatal(err)
	}
	recorder.Begin("prepared temporary failure")
	recorder.RecordState(path, true, 0o644, []byte("before"))
	injected := errors.New("injected temporary preparation failure")
	called := false
	published, err := recorder.PublishFileCASPrepared(
		context.Background(), path, parent, filepath.Base(path),
		true, 0o644, []byte("before"), 0o600, []byte("private replacement"),
		func(file *os.File) error {
			called = true
			info, statErr := file.Stat()
			if statErr != nil {
				return statErr
			}
			if info.Size() != 0 {
				return fmt.Errorf("temporary already contains %d bytes", info.Size())
			}
			return injected
		},
		nil,
	)
	if !called || published || !errors.Is(err, injected) {
		t.Fatalf("PublishFileCASPrepared() called=%v published=%v err=%v", called, published, err)
	}
	recorder.Abort(path)
	if got := readBack(t, path); got != "before" {
		t.Fatalf("failed preparation changed target to %q", got)
	}
	assertNoPublicationArtifacts(t, workspace, journalDir)
}

func TestPublishStandaloneFileCASAllowsEightMiBWithoutUndoArtifacts(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	journalDir := filepath.Join(base, "sessions")
	for _, dir := range []string{workspace, journalDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(workspace, "state.json")
	parent, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	desired := bytes.Repeat([]byte("x"), maxStandaloneFilePublicationBytes)
	published, err := PublishStandaloneFileCAS(
		context.Background(), journalDir, workspace, path, parent, filepath.Base(path),
		false, 0, nil, 0o600, desired, maxStandaloneFilePublicationBytes, nil, nil,
	)
	if err != nil || !published {
		t.Fatalf("PublishStandaloneFileCAS() = published %v, %v", published, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(desired)) {
		t.Fatalf("published size=%d, want %d", info.Size(), len(desired))
	}
	assertNoPublicationArtifacts(t, workspace, journalDir)
}

func TestRestoreStandaloneFileCASBoundSupportsDeletionAndRecreation(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	journalDir := filepath.Join(base, "sessions")
	for _, dir := range []string{workspace, journalDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(workspace, "target.txt")
	write(t, path, "before")
	workspaceRoot, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer workspaceRoot.Close()
	journalRoot, err := os.OpenRoot(journalDir)
	if err != nil {
		t.Fatal(err)
	}
	defer journalRoot.Close()

	published, err := RestoreStandaloneFileCASBound(
		context.Background(), journalDir, workspace, journalRoot, workspaceRoot,
		path, workspaceRoot, filepath.Base(path),
		FileState{Existed: true, Mode: 0o644, Content: []byte("before")}, FileState{},
		1024, nil, nil,
	)
	if err != nil || !published {
		t.Fatalf("delete = published %v, %v", published, err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("deleted target remains: %v", err)
	}
	assertNoPublicationArtifacts(t, workspace, journalDir)

	published, err = RestoreStandaloneFileCASBound(
		context.Background(), journalDir, workspace, journalRoot, workspaceRoot,
		path, workspaceRoot, filepath.Base(path),
		FileState{}, FileState{Existed: true, Mode: 0o644, Content: []byte("again")},
		1024, nil, nil,
	)
	if err != nil || !published {
		t.Fatalf("recreate = published %v, %v", published, err)
	}
	if got := readBack(t, path); got != "again" {
		t.Fatalf("recreated target = %q", got)
	}
	assertNoPublicationArtifacts(t, workspace, journalDir)
}

func TestRestoreStandaloneFileCASBoundDeletionRunsFinalCASSeam(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	journalDir := filepath.Join(base, "sessions")
	for _, dir := range []string{workspace, journalDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(workspace, "target.txt")
	write(t, path, "before")
	workspaceRoot, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer workspaceRoot.Close()
	journalRoot, err := os.OpenRoot(journalDir)
	if err != nil {
		t.Fatal(err)
	}
	defer journalRoot.Close()
	called := false
	published, err := RestoreStandaloneFileCASBound(
		context.Background(), journalDir, workspace, journalRoot, workspaceRoot,
		path, workspaceRoot, filepath.Base(path),
		FileState{Existed: true, Mode: 0o644, Content: []byte("before")}, FileState{},
		1024, nil, func() {
			called = true
			write(t, path, "external")
		},
	)
	if published || !errors.Is(err, ErrStale) || !called {
		t.Fatalf("delete race = called %v, published %v, %v", called, published, err)
	}
	if got := readBack(t, path); got != "external" {
		t.Fatalf("external target = %q", got)
	}
	assertNoPublicationArtifacts(t, workspace, journalDir)
}

func TestPublishStandaloneFileCASRejectsPlusOneBeforeFilesystemSetup(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "missing")
	desired := make([]byte, maxStandaloneFilePublicationBytes+1)
	published, err := PublishStandaloneFileCAS(
		context.Background(), missing, missing, filepath.Join(missing, "state.json"), nil, "state.json",
		false, 0, nil, 0o600, desired, maxStandaloneFilePublicationBytes, nil, nil,
	)
	if published || err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize standalone publication = published %v, %v", published, err)
	}
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversize publication touched filesystem: %v", err)
	}
}

func TestBoundStandalonePublicationAndRecoveryRefuseRetargetedJournal(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	journalDir := filepath.Join(base, "journal")
	movedJournal := filepath.Join(base, "journal-moved")
	for _, dir := range []string{workspace, journalDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	workspaceRoot, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer workspaceRoot.Close()
	journalRoot, err := os.OpenRoot(journalDir)
	if err != nil {
		t.Fatal(err)
	}
	defer journalRoot.Close()
	if err := os.Rename(journalDir, movedJournal); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(journalDir, "replacement-sentinel")
	write(t, sentinel, "outside")
	path := filepath.Join(workspace, "state.json")

	published, err := PublishStandaloneFileCASBound(
		context.Background(), journalDir, workspace, journalRoot, workspaceRoot,
		path, workspaceRoot, filepath.Base(path),
		false, 0, nil, 0o600, []byte("state"), 1024, nil, nil,
	)
	if published || !errors.Is(err, ErrStale) {
		t.Fatalf("bound publication = published %v, %v", published, err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("retargeted bound publication created state: %v", err)
	}
	if err := RecoverFilePublicationCleanupBound(journalDir, workspace, journalRoot, workspaceRoot); !errors.Is(err, ErrStale) {
		t.Fatalf("bound recovery after journal retarget = %v", err)
	}
	if got := readBack(t, sentinel); got != "outside" {
		t.Fatalf("replacement journal sentinel = %q", got)
	}
}

func TestBoundStandalonePublicationRetainsJournalCapabilityAfterPathRetarget(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	journalDir := filepath.Join(base, "journal")
	movedJournal := filepath.Join(base, "journal-moved")
	for _, dir := range []string{workspace, journalDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	workspaceRoot, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer workspaceRoot.Close()
	journalRoot, err := os.OpenRoot(journalDir)
	if err != nil {
		t.Fatal(err)
	}
	defer journalRoot.Close()
	sentinel := filepath.Join(journalDir, "replacement-sentinel")
	var hookErr error
	standaloneFilePublicationAfterConfigTestHook = func() {
		standaloneFilePublicationAfterConfigTestHook = nil
		hookErr = os.Rename(journalDir, movedJournal)
		if hookErr == nil {
			hookErr = os.Mkdir(journalDir, 0o700)
		}
		if hookErr == nil {
			write(t, sentinel, "outside")
		}
	}
	t.Cleanup(func() { standaloneFilePublicationAfterConfigTestHook = nil })
	path := filepath.Join(workspace, "state.json")
	published, err := PublishStandaloneFileCASBound(
		context.Background(), journalDir, workspace, journalRoot, workspaceRoot,
		path, workspaceRoot, filepath.Base(path),
		false, 0, nil, 0o600, []byte("state"), 1024, nil, nil,
	)
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err != nil || !published {
		t.Fatalf("bound publication after retarget = published %v, %v", published, err)
	}
	if got := readBack(t, path); got != "state" {
		t.Fatalf("published state = %q", got)
	}
	if got := readBack(t, sentinel); got != "outside" {
		t.Fatalf("replacement journal sentinel = %q", got)
	}
	entries, err := os.ReadDir(movedJournal)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), restoreTempLedgerPrefix) {
			t.Fatalf("retained journal left cleanup ledger: %s", entry.Name())
		}
	}
}

func TestPublishFileCASRefusesChangedSourceAndAbortsCleanly(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	journalDir := filepath.Join(base, "sessions")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace, "target.txt")
	write(t, path, "before")
	parent, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	recorder := NewRecorder()
	if err := recorder.ConfigureRestoreCleanup(journalDir, workspace); err != nil {
		t.Fatal(err)
	}
	recorder.Begin("source race")
	recorder.RecordState(path, true, 0o644, []byte("before"))
	published, err := recorder.PublishFileCAS(
		context.Background(), path, parent, filepath.Base(path),
		true, 0o644, []byte("before"), 0o644, []byte("agent"),
		func() { write(t, path, "external") },
	)
	if published || !errors.Is(err, ErrStale) {
		t.Fatalf("PublishFileCAS() = published %v, %v; want unpublished stale refusal", published, err)
	}
	recorder.Abort(path)
	if got := readBack(t, path); got != "external" {
		t.Fatalf("external source = %q, want external", got)
	}
	assertNoPublicationArtifacts(t, workspace, journalDir)
}

func TestPublishFileCASRefusesSameInodeChangeAtFinalSeam(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	journalDir := filepath.Join(base, "sessions")
	for _, dir := range []string{workspace, journalDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(workspace, "target.txt")
	write(t, path, "before")
	parent, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()

	recorder := NewRecorder()
	if err := recorder.ConfigureRestoreCleanup(journalDir, workspace); err != nil {
		t.Fatal(err)
	}
	recorder.Begin("same-inode final-seam race")
	recorder.RecordState(path, true, 0o644, []byte("before"))
	recorder.publicationSeamHook = func() error {
		return os.WriteFile(path, []byte("external"), 0o644)
	}
	published, err := recorder.PublishFileCAS(
		context.Background(), path, parent, filepath.Base(path),
		true, 0o644, []byte("before"), 0o644, []byte("agent"), nil,
	)
	if published || !errors.Is(err, ErrStale) {
		t.Fatalf("PublishFileCAS() = published %v, %v; want unpublished stale refusal", published, err)
	}
	recorder.Abort(path)
	if got := readBack(t, path); got != "external" {
		t.Fatalf("external source = %q, want external", got)
	}
	assertNoPublicationArtifacts(t, workspace, journalDir)
}

func TestPublishFileCASRollsBackSameInodeChangeAfterExchange(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	journalDir := filepath.Join(base, "sessions")
	for _, dir := range []string{workspace, journalDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(workspace, "target.txt")
	write(t, path, "before")
	parent, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()

	recorder := NewRecorder()
	if err := recorder.ConfigureRestoreCleanup(journalDir, workspace); err != nil {
		t.Fatal(err)
	}
	recorder.Begin("same-inode post-exchange race")
	recorder.RecordState(path, true, 0o644, []byte("before"))
	boundAfterNamespaceTestHook = func() error {
		boundAfterNamespaceTestHook = nil
		entries, err := os.ReadDir(workspace)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if isRestoreTempName(entry.Name()) {
				return os.WriteFile(filepath.Join(workspace, entry.Name()), []byte("external"), 0o644)
			}
		}
		return errors.New("displaced pre-image was not present at the post-exchange seam")
	}
	t.Cleanup(func() { boundAfterNamespaceTestHook = nil })
	published, err := recorder.PublishFileCAS(
		context.Background(), path, parent, filepath.Base(path),
		true, 0o644, []byte("before"), 0o644, []byte("agent"), nil,
	)
	boundAfterNamespaceTestHook = nil
	if published || !errors.Is(err, ErrStale) {
		t.Fatalf("PublishFileCAS() = published %v, %v; want rolled-back stale refusal", published, err)
	}
	recorder.Abort(path)
	if got := readBack(t, path); got != "external" {
		t.Fatalf("rolled-back external source = %q, want external", got)
	}
	assertNoPublicationArtifacts(t, workspace, journalDir)
}

func TestPublishFileCASLateParentMoveNeverFollowsReplacement(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	journalDir := filepath.Join(base, "sessions")
	parentPath := filepath.Join(workspace, "nested")
	movedParent := filepath.Join(workspace, "nested-moved")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{parentPath, journalDir, outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(parentPath, "target.txt")
	outsidePath := filepath.Join(outside, "target.txt")
	write(t, path, "before")
	write(t, outsidePath, "outside")
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	recorder := NewRecorder()
	if err := recorder.ConfigureRestoreCleanup(journalDir, workspace); err != nil {
		t.Fatal(err)
	}
	recorder.Begin("parent move")
	recorder.RecordState(path, true, 0o644, []byte("before"))
	boundAfterNamespaceTestHook = func() error {
		boundAfterNamespaceTestHook = nil
		if renameErr := os.Rename(parentPath, movedParent); renameErr != nil {
			return renameErr
		}
		return os.Symlink(outside, parentPath)
	}
	t.Cleanup(func() { boundAfterNamespaceTestHook = nil })
	published, err := recorder.PublishFileCAS(
		context.Background(), path, parent, filepath.Base(path),
		true, 0o644, []byte("before"), 0o644, []byte("agent"),
		nil,
	)
	boundAfterNamespaceTestHook = nil
	if !published || !errors.Is(err, ErrStale) {
		t.Fatalf("PublishFileCAS() = published %v, %v; want published recovery-required namespace result", published, err)
	}
	recorder.Commit(path, true, 0o644, sha256.Sum256([]byte("agent")))
	if got := readBack(t, outsidePath); got != "outside" {
		t.Fatalf("outside target = %q, want outside", got)
	}
	if got := readBack(t, filepath.Join(movedParent, "target.txt")); got != "agent" {
		t.Fatalf("already-published moved target = %q, want agent", got)
	}
	if !hasPublicationTempLedger(t, journalDir) {
		t.Fatal("post-publication parent move did not retain recovery evidence")
	}

	if err := os.Remove(parentPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(movedParent, parentPath); err != nil {
		t.Fatal(err)
	}
	if err := RecoverFilePublicationCleanup(journalDir, workspace); err != nil {
		t.Fatal(err)
	}
	assertNoPublicationArtifacts(t, workspace, journalDir)
}

func TestPublishFileCASRollsBackCreationAfterLateParentMove(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	journalDir := filepath.Join(base, "sessions")
	parentPath := filepath.Join(workspace, "nested")
	movedParent := filepath.Join(workspace, "nested-moved")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{parentPath, journalDir, outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(parentPath, "created.txt")
	outsidePath := filepath.Join(outside, "created.txt")
	write(t, outsidePath, "outside")
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	recorder := NewRecorder()
	if err := recorder.ConfigureRestoreCleanup(journalDir, workspace); err != nil {
		t.Fatal(err)
	}
	recorder.Begin("create parent move")
	recorder.RecordState(path, false, 0, nil)
	boundAfterNamespaceTestHook = func() error {
		boundAfterNamespaceTestHook = nil
		if renameErr := os.Rename(parentPath, movedParent); renameErr != nil {
			return renameErr
		}
		return os.Symlink(outside, parentPath)
	}
	t.Cleanup(func() { boundAfterNamespaceTestHook = nil })
	published, err := recorder.PublishFileCAS(
		context.Background(), path, parent, filepath.Base(path),
		false, 0, nil, 0o644, []byte("agent"), nil,
	)
	boundAfterNamespaceTestHook = nil
	if !published || !errors.Is(err, ErrStale) {
		t.Fatalf("PublishFileCAS() = published %v, %v; want published recovery-required creation", published, err)
	}
	recorder.Commit(path, true, 0o644, sha256.Sum256([]byte("agent")))
	if got := readBack(t, outsidePath); got != "outside" {
		t.Fatalf("outside target = %q, want outside", got)
	}
	if got := readBack(t, filepath.Join(movedParent, "created.txt")); got != "agent" {
		t.Fatalf("already-published moved creation = %q, want agent", got)
	}
	if !hasPublicationTempLedger(t, journalDir) {
		t.Fatal("post-publication parent move did not retain recovery evidence")
	}
	if err := os.Remove(parentPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(movedParent, parentPath); err != nil {
		t.Fatal(err)
	}
	if err := RecoverFilePublicationCleanup(journalDir, workspace); err != nil {
		t.Fatal(err)
	}
	assertNoPublicationArtifacts(t, workspace, journalDir)
}

func TestPublishFileCASParentRollbackFailureRetainsRecoveryEvidence(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	journalDir := filepath.Join(base, "sessions")
	parentPath := filepath.Join(workspace, "nested")
	movedParent := filepath.Join(workspace, "nested-moved")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{parentPath, journalDir, outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(parentPath, "target.txt")
	outsidePath := filepath.Join(outside, "target.txt")
	write(t, path, "before")
	write(t, outsidePath, "outside")
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	recorder := NewRecorder()
	if err := recorder.ConfigureRestoreCleanup(journalDir, workspace); err != nil {
		t.Fatal(err)
	}
	recorder.Begin("failed parent rollback")
	recorder.RecordState(path, true, 0o644, []byte("before"))
	boundAfterNamespaceTestHook = func() error {
		boundAfterNamespaceTestHook = nil
		if renameErr := os.Rename(parentPath, movedParent); renameErr != nil {
			return renameErr
		}
		return os.Symlink(outside, parentPath)
	}
	injected := errors.New("injected descriptor-bound rollback failure")
	boundRollbackTestHook = func() error { return injected }
	t.Cleanup(func() {
		boundAfterNamespaceTestHook = nil
		boundRollbackTestHook = nil
	})
	published, err := recorder.PublishFileCAS(
		context.Background(), path, parent, filepath.Base(path),
		true, 0o644, []byte("before"), 0o644, []byte("agent"), nil,
	)
	boundAfterNamespaceTestHook = nil
	boundRollbackTestHook = nil
	if !published || !errors.Is(err, injected) || !errors.Is(err, ErrStale) ||
		!strings.Contains(err.Error(), "rolling back stale checkpoint publication") {
		t.Fatalf("PublishFileCAS() = published %v, %v; want published rollback failure before stale cause", published, err)
	}
	recorder.Commit(path, true, 0o644, sha256.Sum256([]byte("agent")))
	if got := readBack(t, outsidePath); got != "outside" {
		t.Fatalf("outside target = %q, want outside", got)
	}
	if got := readBack(t, filepath.Join(movedParent, "target.txt")); got != "agent" {
		t.Fatalf("ambiguous bound target = %q, want agent", got)
	}
	if !hasPublicationTempLedger(t, journalDir) {
		t.Fatal("rollback failure did not retain recovery ledger")
	}
	if err := os.Remove(parentPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(movedParent, parentPath); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverDurableUndo(journalDir, workspace, func(string, string) (bool, error) {
		return false, nil
	}); err != nil {
		t.Fatalf("recovering retained rollback evidence: %v", err)
	}
	if got := readBack(t, path); got != "agent" {
		t.Fatalf("recovered published target = %q, want agent", got)
	}
	assertNoPublicationArtifacts(t, workspace, journalDir)
}

func hasPublicationTempLedger(t *testing.T, dir string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if isRestoreTempLedgerName(entry.Name()) {
			return true
		}
	}
	return false
}

func TestPublishFileCASRequiresPreparedDurableRecorder(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "target.txt")
	parent, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	recorder := NewRecorder()
	if published, err := recorder.PublishFileCAS(
		context.Background(), path, parent, filepath.Base(path),
		false, 0, nil, 0o644, []byte("desired"), nil,
	); published || err == nil || !strings.Contains(err.Error(), "crash recovery") {
		t.Fatalf("unconfigured PublishFileCAS() = published %v, %v", published, err)
	}
}

func assertNoPublicationArtifacts(t *testing.T, dirs ...string) {
	t.Helper()
	for index, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := entry.Name()
			if index > 0 && strings.HasPrefix(name, ".switchboard-quarantine-") {
				info, infoErr := entry.Info()
				if infoErr != nil {
					return infoErr
				}
				if !info.Mode().IsRegular() || info.Size() != 0 {
					t.Errorf("control quarantine retained publication bytes: %s", path)
				}
				return nil
			}
			if isRestoreTempName(name) || isRestoreTempLedgerName(name) ||
				strings.HasPrefix(name, ".switchboard-staging-") ||
				strings.HasPrefix(name, ".switchboard-retired-") ||
				strings.HasPrefix(name, ".switchboard-quarantine-") {
				t.Errorf("publication artifact remains: %s", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
