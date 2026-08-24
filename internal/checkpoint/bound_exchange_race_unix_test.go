//go:build unix

package checkpoint

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestExchangeRollsBackFinalSourceSubstitution(t *testing.T) {
	workspace, journalDir, target, recorder := newExchangeRaceFixture(t)
	var substituted string
	boundNamespaceMutationTestHook = func() {
		boundNamespaceMutationTestHook = nil
		entries, err := os.ReadDir(workspace)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if !isRestoreTempName(entry.Name()) {
				continue
			}
			temp := filepath.Join(workspace, entry.Name())
			substituted = temp
			if err := os.Rename(temp, temp+".owned"); err != nil {
				t.Fatal(err)
			}
			write(t, temp, "foreign source sentinel")
			return
		}
		t.Fatal("checkpoint temp was not visible at exchange seam")
	}
	outcome, _, err := recorder.UndoFile(target)
	boundNamespaceMutationTestHook = nil
	if outcome.Published || !errors.Is(err, ErrStale) {
		t.Fatalf("source substitution = %+v, %v; want rolled-back stale refusal", outcome, err)
	}
	if got := readBack(t, target); got != "after" {
		t.Fatalf("source substitution changed target to %q", got)
	}
	if got := readBack(t, substituted); got != "foreign source sentinel" {
		t.Fatalf("source sentinel was changed to %q", got)
	}
	if got := readBack(t, substituted+".owned"); got != "before" {
		t.Fatalf("exact owned pre-image evidence was changed to %q", got)
	}
	if !hasPublicationTempLedger(t, journalDir) {
		t.Fatal("ambiguous source substitution did not retain its cleanup ledger")
	}
}

func TestExchangeRollsBackFinalTargetSubstitution(t *testing.T) {
	_, journalDir, target, recorder := newExchangeRaceFixture(t)
	captured := target + ".captured"
	boundNamespaceMutationTestHook = func() {
		boundNamespaceMutationTestHook = nil
		if err := os.Rename(target, captured); err != nil {
			t.Fatal(err)
		}
		write(t, target, "foreign target sentinel")
	}
	outcome, _, err := recorder.UndoFile(target)
	boundNamespaceMutationTestHook = nil
	if outcome.Published || !errors.Is(err, ErrStale) {
		t.Fatalf("target substitution = %+v, %v; want rolled-back stale refusal", outcome, err)
	}
	if got := readBack(t, target); got != "foreign target sentinel" {
		t.Fatalf("target sentinel was changed to %q", got)
	}
	if got := readBack(t, captured); got != "after" {
		t.Fatalf("pre-exchange target evidence changed to %q", got)
	}
	if hasPublicationTempLedger(t, journalDir) {
		t.Fatal("positively retired rollback temporary left a false recovery ledger")
	}
}

func TestRemovalFinalSubstitutionRetainsForeignInodeAndRecoveryEvidence(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	journalDir := filepath.Join(base, "sessions")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(workspace, "created.txt")
	recorder := NewRecorder()
	if err := recorder.ConfigureRestoreCleanup(journalDir, workspace); err != nil {
		t.Fatal(err)
	}
	recorder.Begin("removal race")
	recorder.RecordState(target, false, 0, nil)
	write(t, target, "after")
	recorder.Commit(target, true, 0o644, sha256.Sum256([]byte("after")))
	captured := target + ".captured"
	boundNamespaceMutationTestHook = func() {
		boundNamespaceMutationTestHook = nil
		if err := os.Rename(target, captured); err != nil {
			t.Fatal(err)
		}
		write(t, target, "foreign removal sentinel")
	}
	outcome, _, err := recorder.UndoFile(target)
	boundNamespaceMutationTestHook = nil
	if !outcome.Published || !errors.Is(err, ErrStale) {
		t.Fatalf("removal substitution = %+v, %v; want published recovery-required refusal", outcome, err)
	}
	if got := readBack(t, captured); got != "after" {
		t.Fatalf("opened removal target evidence changed to %q", got)
	}
	if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("target name unexpectedly remains after ambiguous move: %v", statErr)
	}
	sentinelFound := false
	entries, err := os.ReadDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !isRestoreTempName(entry.Name()) {
			continue
		}
		if got := readBack(t, filepath.Join(workspace, entry.Name())); got == "foreign removal sentinel" {
			sentinelFound = true
		}
	}
	if !sentinelFound {
		t.Fatal("foreign removal sentinel was unlinked instead of retained as recovery evidence")
	}
	if !hasPublicationTempLedger(t, journalDir) {
		t.Fatal("ambiguous removal did not retain its cleanup ledger")
	}
}

func newExchangeRaceFixture(t *testing.T) (workspace, journalDir, target string, recorder *Recorder) {
	t.Helper()
	root := t.TempDir()
	workspace = filepath.Join(root, "workspace")
	journalDir = filepath.Join(root, "sessions")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target = filepath.Join(workspace, "target.txt")
	write(t, target, "before")
	recorder = NewRecorder()
	if err := recorder.ConfigureRestoreCleanup(journalDir, workspace); err != nil {
		t.Fatal(err)
	}
	recorder.Begin("exchange race")
	recorder.RecordState(target, true, 0o644, []byte("before"))
	write(t, target, "after")
	recorder.Commit(target, true, 0o644, sha256.Sum256([]byte("after")))
	return workspace, journalDir, target, recorder
}
