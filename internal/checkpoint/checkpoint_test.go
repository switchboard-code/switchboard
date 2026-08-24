package checkpoint

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readBack(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestUndoRestoresEditedAndRemovesCreated(t *testing.T) {
	dir := t.TempDir()
	edited := filepath.Join(dir, "edited.go")
	created := filepath.Join(dir, "created.go")
	write(t, edited, "original\n")

	r := NewRecorder()
	r.Begin("fix the parser")
	r.Record(edited) // captured before mutation
	write(t, edited, "mutated\n")
	r.Record(created) // did not exist
	write(t, created, "brand new\n")

	restored, removed, skipped, failed, label, err := r.Undo()
	if err != nil {
		t.Fatal(err)
	}
	if label != "fix the parser" {
		t.Errorf("label = %q", label)
	}
	if len(restored) != 1 || restored[0] != edited {
		t.Errorf("restored = %v", restored)
	}
	if len(removed) != 1 || removed[0] != created {
		t.Errorf("removed = %v", removed)
	}
	if len(skipped) != 0 || len(failed) != 0 {
		t.Errorf("skipped=%v failed=%v", skipped, failed)
	}
	if got := readBack(t, edited); got != "original\n" {
		t.Errorf("edited file = %q, want the pre-turn content", got)
	}
	if _, statErr := os.Stat(created); !os.IsNotExist(statErr) {
		t.Error("a file the turn created must be removed by undo")
	}
}

func TestFirstCaptureWinsWithinATurn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	write(t, path, "v1\n")

	r := NewRecorder()
	r.Begin("churn")
	r.Record(path)
	write(t, path, "v2\n")
	r.Record(path) // second mutation in the same turn
	write(t, path, "v3\n")

	if _, _, _, _, _, err := r.Undo(); err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, path); got != "v1\n" {
		t.Errorf("file = %q, want the pre-turn state, not intra-turn churn", got)
	}
}

func TestEmptyTurnsAreNotStacked(t *testing.T) {
	r := NewRecorder()
	r.Begin("asked a question")
	r.Begin("asked another")

	if _, _, _, _, _, err := r.Undo(); err == nil {
		t.Fatal("undo with no captured turns must say there is nothing to undo")
	}
	if turns := r.Turns(); len(turns) != 0 {
		t.Errorf("turns = %v, want none", turns)
	}
}

func TestUndoCurrentDoesNotFallThroughAnEmptyTurnToOlderSameLabelMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "same-label.txt")
	write(t, path, "before")

	r := NewRecorder()
	first := TurnIdentity{SessionID: "session", OpeningMessage: 0}
	second := TurnIdentity{SessionID: "session", OpeningMessage: 2}
	r.BeginTurn(first.SessionID, first.OpeningMessage, "continue")
	r.Record(path)
	write(t, path, "after first turn")
	r.BeginTurn(second.SessionID, second.OpeningMessage, "continue")

	if current, ok := r.CurrentTurn(second); !ok || current.Files != 0 {
		t.Fatalf("empty second turn is not authoritative: current=%+v ok=%v", current, ok)
	}
	if _, _, _, _, _, err := r.UndoCurrent(first); err == nil {
		t.Fatal("UndoCurrent accepted an older turn instead of the empty current turn")
	}
	if got := readBack(t, path); got != "after first turn" {
		t.Fatalf("refused UndoCurrent still changed the file: %q", got)
	}
	if _, _, _, _, label, err := r.Undo(); err != nil || label != "continue" {
		t.Fatalf("older checkpoint was consumed by the refusal: label=%q err=%v", label, err)
	}
	if got := readBack(t, path); got != "before" {
		t.Fatalf("ordinary undo did not retain the older checkpoint: %q", got)
	}
}

func TestUndoCurrentRestoresTheExactIdentifiedTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identified.txt")
	write(t, path, "before")

	r := NewRecorder()
	identity := TurnIdentity{SessionID: "session", OpeningMessage: 4}
	r.BeginTurn(identity.SessionID, identity.OpeningMessage, "edit")
	r.Record(path)
	write(t, path, "after")

	if _, _, _, _, label, err := r.UndoCurrent(identity); err != nil || label != "edit" {
		t.Fatalf("UndoCurrent: label=%q err=%v", label, err)
	}
	if got := readBack(t, path); got != "before" {
		t.Fatalf("identified turn was not restored: %q", got)
	}
}

func TestUndoCurrentKeepsTheExactTurnsCapturesAfterUndoFile(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.txt")
	second := filepath.Join(dir, "second.txt")
	write(t, first, "first before")
	write(t, second, "second before")

	r := NewRecorder()
	identity := TurnIdentity{SessionID: "session", OpeningMessage: 6}
	r.BeginTurn(identity.SessionID, identity.OpeningMessage, "edit both")
	r.Record(first)
	r.Record(second)
	write(t, first, "first after")
	write(t, second, "second after")

	if _, _, err := r.UndoFile(first); err != nil {
		t.Fatal(err)
	}
	if current, ok := r.CurrentTurn(identity); !ok || current.Files != 1 {
		t.Fatalf("remaining capture lost its turn identity: current=%+v ok=%v", current, ok)
	}
	if _, _, _, _, _, err := r.UndoCurrent(identity); err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, first); got != "first before" {
		t.Fatalf("UndoFile result changed: %q", got)
	}
	if got := readBack(t, second); got != "second before" {
		t.Fatalf("UndoCurrent did not restore the remaining exact-turn capture: %q", got)
	}
}

func TestPreparedUndoCurrentRestoresOnlyOnApply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prepared.txt")
	write(t, path, "before")
	r := NewRecorder()
	identity := TurnIdentity{SessionID: "session", OpeningMessage: 2}
	r.BeginTurn(identity.SessionID, identity.OpeningMessage, "edit")
	r.Record(path)
	write(t, path, "after")

	prepared, err := r.PrepareUndoCurrent(identity)
	if err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, path); got != "after" {
		t.Fatalf("preparation changed the workspace: %q", got)
	}
	result, err := prepared.Apply()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Restored) != 1 || result.Restored[0] != path || len(result.Removed) != 0 {
		t.Fatalf("prepared restore result = %+v", result)
	}
	if got := readBack(t, path); got != "before" {
		t.Fatalf("Apply did not publish the pre-image: %q", got)
	}
	if current, ok := r.CurrentTurn(identity); !ok || current.Files != 0 || current.Partial {
		t.Fatalf("successful Apply did not consume its exact evidence: current=%+v ok=%v", current, ok)
	}
	if _, err := prepared.Apply(); !errors.Is(err, ErrStale) {
		t.Fatalf("prepared restore was reusable: %v", err)
	}
}

func TestPreparedUndoCurrentPreflightsEveryPathBeforePublishing(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.txt")
	second := filepath.Join(dir, "b.txt")
	write(t, first, "first before")
	write(t, second, "second before")
	r := NewRecorder()
	identity := TurnIdentity{SessionID: "session", OpeningMessage: 0}
	r.BeginTurn(identity.SessionID, identity.OpeningMessage, "edit both")
	for _, path := range []string{first, second} {
		r.Record(path)
	}
	write(t, first, "first after")
	write(t, second, "second after")

	prepared, err := r.PrepareUndoCurrent(identity)
	if err != nil {
		t.Fatal(err)
	}
	write(t, second, "newer external edit")
	if _, err := prepared.Apply(); !errors.Is(err, ErrStale) {
		t.Fatalf("stale Apply error = %v, want ErrStale", err)
	}
	if got := readBack(t, first); got != "first after" {
		t.Fatalf("Apply restored a prefix before finding staleness: %q", got)
	}
	if got := readBack(t, second); got != "newer external edit" {
		t.Fatalf("Apply overwrote the newer edit: %q", got)
	}
	if current, ok := r.CurrentTurn(identity); !ok || current.Files != 2 {
		t.Fatalf("stale Apply consumed evidence: current=%+v ok=%v", current, ok)
	}
}

func TestPreparedUndoCurrentRollsPublishedPathsForwardOnFailure(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.txt")
	second := filepath.Join(dir, "b.txt")
	write(t, first, "first before")
	write(t, second, "second before")
	r := NewRecorder()
	identity := TurnIdentity{SessionID: "session", OpeningMessage: 0}
	r.BeginTurn(identity.SessionID, identity.OpeningMessage, "edit both")
	for _, path := range []string{first, second} {
		r.Record(path)
	}
	write(t, first, "first after")
	write(t, second, "second after")
	prepared, err := r.PrepareUndoCurrent(identity)
	if err != nil {
		t.Fatal(err)
	}

	injected := errors.New("injected post-publication failure")
	publications := 0
	r.afterReplaceHook = func() error {
		publications++
		if publications == 2 {
			return injected
		}
		return nil
	}
	if _, err := prepared.Apply(); !errors.Is(err, injected) {
		t.Fatalf("Apply error = %v, want injected failure", err)
	}
	if got := readBack(t, first); got != "first after" {
		t.Fatalf("first published path was not rolled forward: %q", got)
	}
	if got := readBack(t, second); got != "second after" {
		t.Fatalf("failing published path was not rolled forward: %q", got)
	}
	if current, ok := r.CurrentTurn(identity); !ok || current.Files != 2 {
		t.Fatalf("rolled-back Apply consumed evidence: current=%+v ok=%v", current, ok)
	}

	r.afterReplaceHook = nil
	prepared, err = r.PrepareUndoCurrent(identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.Apply(); err != nil {
		t.Fatalf("retained evidence could not be applied: %v", err)
	}
	if got := readBack(t, first); got != "first before" {
		t.Fatalf("retry did not restore first pre-image: %q", got)
	}
	if got := readBack(t, second); got != "second before" {
		t.Fatalf("retry did not restore second pre-image: %q", got)
	}
}

func TestPreparedUndoCurrentCommitRunsOnPreImagesAndRollsBackOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "publish.txt")
	write(t, path, "before")
	r := NewRecorder()
	identity := TurnIdentity{SessionID: "session", OpeningMessage: 0}
	r.BeginTurn(identity.SessionID, identity.OpeningMessage, "edit")
	r.Record(path)
	write(t, path, "after")
	prepared, err := r.PrepareUndoCurrent(identity)
	if err != nil {
		t.Fatal(err)
	}

	injected := errors.New("injected adoption failure")
	commits := 0
	_, err = prepared.ApplyAndCommit(func() error {
		commits++
		if got := readBack(t, path); got != "before" {
			t.Fatalf("commit observed %q, want installed pre-image", got)
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("ApplyAndCommit error = %v, want injected failure", err)
	}
	if commits != 1 {
		t.Fatalf("commit ran %d times, want once", commits)
	}
	if got := readBack(t, path); got != "after" {
		t.Fatalf("failed commit did not roll the post-image forward: %q", got)
	}
	if current, ok := r.CurrentTurn(identity); !ok || current.Files != 1 {
		t.Fatalf("failed commit consumed evidence: current=%+v ok=%v", current, ok)
	}

	prepared, err = r.PrepareUndoCurrent(identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.ApplyAndCommit(func() error {
		commits++
		if got := readBack(t, path); got != "before" {
			t.Fatalf("successful commit observed %q, want installed pre-image", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if commits != 2 {
		t.Fatalf("successful commit count = %d, want 2 total", commits)
	}
	if got := readBack(t, path); got != "before" {
		t.Fatalf("successful commit did not retain pre-image: %q", got)
	}
	if current, ok := r.CurrentTurn(identity); !ok || current.Files != 0 {
		t.Fatalf("successful commit did not consume evidence: current=%+v ok=%v", current, ok)
	}
}

func TestPreparedUndoCurrentRestoresCreatedAndDeletedPaths(t *testing.T) {
	dir := t.TempDir()
	created := filepath.Join(dir, "created.txt")
	deleted := filepath.Join(dir, "deleted.txt")
	write(t, deleted, "deleted before")
	r := NewRecorder()
	identity := TurnIdentity{SessionID: "session", OpeningMessage: 0}
	r.BeginTurn(identity.SessionID, identity.OpeningMessage, "create and delete")
	r.Record(created)
	write(t, created, "created after")
	r.Record(deleted)
	if err := os.Remove(deleted); err != nil {
		t.Fatal(err)
	}

	prepared, err := r.PrepareUndoCurrent(identity)
	if err != nil {
		t.Fatal(err)
	}
	result, err := prepared.Apply()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != 1 || result.Removed[0] != created ||
		len(result.Restored) != 1 || result.Restored[0] != deleted {
		t.Fatalf("created/deleted result = %+v", result)
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Fatalf("created path survived Apply: %v", err)
	}
	if got := readBack(t, deleted); got != "deleted before" {
		t.Fatalf("deleted path was not restored: %q", got)
	}
}

func TestPreparedUndoCurrentRollsCreatedAndDeletedPathsForward(t *testing.T) {
	injected := errors.New("injected post-publication failure")
	t.Run("created", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "created.txt")
		r := NewRecorder()
		identity := TurnIdentity{SessionID: "session", OpeningMessage: 0}
		r.BeginTurn(identity.SessionID, identity.OpeningMessage, "create")
		r.Record(path)
		write(t, path, "created after")
		prepared, err := r.PrepareUndoCurrent(identity)
		if err != nil {
			t.Fatal(err)
		}
		r.afterRemoveHook = func() error { return injected }
		if _, err := prepared.Apply(); !errors.Is(err, injected) {
			t.Fatalf("Apply error = %v, want injected failure", err)
		}
		if got := readBack(t, path); got != "created after" {
			t.Fatalf("created post-image was not rolled forward: %q", got)
		}
		if current, ok := r.CurrentTurn(identity); !ok || current.Files != 1 {
			t.Fatalf("created-path rollback consumed evidence: current=%+v ok=%v", current, ok)
		}
	})

	t.Run("deleted", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "deleted.txt")
		write(t, path, "deleted before")
		r := NewRecorder()
		identity := TurnIdentity{SessionID: "session", OpeningMessage: 0}
		r.BeginTurn(identity.SessionID, identity.OpeningMessage, "delete")
		r.Record(path)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		prepared, err := r.PrepareUndoCurrent(identity)
		if err != nil {
			t.Fatal(err)
		}
		r.afterReplaceHook = func() error { return injected }
		if _, err := prepared.Apply(); !errors.Is(err, injected) {
			t.Fatalf("Apply error = %v, want injected failure", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("deleted post-image was not rolled forward: %v", err)
		}
		if current, ok := r.CurrentTurn(identity); !ok || current.Files != 1 {
			t.Fatalf("deleted-path rollback consumed evidence: current=%+v ok=%v", current, ok)
		}
	})
}

func TestPreparedUndoCurrentRejectsAnonymousIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anonymous.txt")
	write(t, path, "before")
	r := NewRecorder()
	r.Begin("anonymous")
	r.Record(path)
	write(t, path, "after")

	if _, err := r.PrepareUndoCurrent(TurnIdentity{}); !errors.Is(err, ErrStale) {
		t.Fatalf("anonymous scope was prepared as a durable retry: %v", err)
	}
	if got := readBack(t, path); got != "after" {
		t.Fatalf("anonymous refusal changed the file: %q", got)
	}
}

func TestPreparedUndoCurrentBoundsRetainedFileCount(t *testing.T) {
	r := NewRecorder()
	identity := TurnIdentity{SessionID: "session", OpeningMessage: 0}
	r.BeginTurn(identity.SessionID, identity.OpeningMessage, "generated files")
	r.mu.Lock()
	for i := 0; i <= maxPreparedUndoFiles; i++ {
		// Synthetic committed captures exercise the pre-I/O count gate without
		// creating a thousand filesystem entries just to test arithmetic.
		r.cur.files["/synthetic/generated-"+strconv.Itoa(i)] = &fileState{committed: true}
	}
	r.mu.Unlock()

	if _, err := r.PrepareUndoCurrent(identity); !errors.Is(err, ErrPreparedUndoTooLarge) {
		t.Fatalf("file-count bound error = %v", err)
	}
	if current, ok := r.CurrentTurn(identity); !ok || current.Files != maxPreparedUndoFiles+1 {
		t.Fatalf("bounded preparation consumed evidence: current=%+v ok=%v", current, ok)
	}
}

func TestPreparedUndoCurrentBoundsAggregatePostImages(t *testing.T) {
	r := NewRecorder()
	identity := TurnIdentity{SessionID: "session", OpeningMessage: 0}
	r.BeginTurn(identity.SessionID, identity.OpeningMessage, "large generated output")

	// Build committed fingerprints directly so the bound is tested without
	// allocating its full 32 MiB threshold in the test process. Prepare checks
	// these sizes before any post-image file I/O.
	r.mu.Lock()
	for i := 0; i <= maxPreparedUndoBytes/maxFileBytes; i++ {
		path := "/synthetic/post-image-" + strconv.Itoa(i)
		r.cur.files[path] = &fileState{
			committed: true,
			after: fingerprint{
				existed: true,
				mode:    0o644,
				size:    maxFileBytes,
			},
		}
	}
	r.mu.Unlock()

	if _, err := r.PrepareUndoCurrent(identity); !errors.Is(err, ErrPreparedUndoTooLarge) {
		t.Fatalf("aggregate byte bound error = %v", err)
	}
	if current, ok := r.CurrentTurn(identity); !ok || current.Files != maxPreparedUndoBytes/maxFileBytes+1 {
		t.Fatalf("bounded preparation consumed evidence: current=%+v ok=%v", current, ok)
	}
}

func TestPreparedUndoCurrentInvalidatesCursorMintedDuringApply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor.txt")
	write(t, path, "before")
	r := NewRecorder()
	identity := TurnIdentity{SessionID: "session", OpeningMessage: 0}
	r.BeginTurn(identity.SessionID, identity.OpeningMessage, "edit")
	r.Record(path)
	write(t, path, "after")
	prepared, err := r.PrepareUndoCurrent(identity)
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	r.restoreHook = func() {
		close(started)
		<-release
	}
	done := make(chan error, 1)
	go func() {
		_, err := prepared.Apply()
		done <- err
	}()
	<-started
	cursor, _, _, ok := r.CurrentReviewCursor()
	if !ok {
		t.Fatal("open turn disappeared while Apply was paused")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r.ReviewCursorValid(cursor) {
		t.Fatal("cursor minted during Apply became valid after its evidence was consumed")
	}
}

func TestPreparedUndoCurrentRefusesConcurrentPostImageAdvance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.txt")
	write(t, path, "before")
	r := NewRecorder()
	identity := TurnIdentity{SessionID: "session", OpeningMessage: 0}
	r.BeginTurn(identity.SessionID, identity.OpeningMessage, "edit")
	r.RecordState(path, true, 0o644, []byte("before"))
	write(t, path, "after one")
	r.Commit(path, true, 0o644, sha256.Sum256([]byte("after one")))

	opened := make(chan struct{})
	release := make(chan struct{})
	r.snapshotAfterOpenHook = func() {
		close(opened)
		<-release
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.PrepareUndoCurrent(identity)
		done <- err
	}()
	<-opened
	r.RecordState(path, true, 0o644, []byte("after one"))
	write(t, path, "after two")
	r.Commit(path, true, 0o644, sha256.Sum256([]byte("after two")))
	close(release)
	if err := <-done; !errors.Is(err, ErrStale) {
		t.Fatalf("concurrent post-image advance error = %v, want ErrStale", err)
	}
	r.snapshotAfterOpenHook = nil
	if current, ok := r.CurrentTurn(identity); !ok || current.Files != 1 {
		t.Fatalf("concurrent preparation consumed evidence: current=%+v ok=%v", current, ok)
	}
}

func TestUndoWalksBackTurnByTurn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	write(t, path, "v1\n")

	r := NewRecorder()
	r.Begin("first")
	r.Record(path)
	write(t, path, "v2\n")

	r.Begin("second")
	r.Record(path)
	write(t, path, "v3\n")
	r.Begin("") // commit the open scope

	if _, _, _, _, label, err := r.Undo(); err != nil || label != "second" {
		t.Fatalf("first undo: label=%q err=%v", label, err)
	}
	if got := readBack(t, path); got != "v2\n" {
		t.Errorf("after one undo, file = %q, want v2", got)
	}
	if _, _, _, _, label, err := r.Undo(); err != nil || label != "first" {
		t.Fatalf("second undo: label=%q err=%v", label, err)
	}
	if got := readBack(t, path); got != "v1\n" {
		t.Errorf("after two undos, file = %q, want v1", got)
	}
}

func TestOversizeFilesAreNamedNotHalfCovered(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.bin")
	small := filepath.Join(dir, "small.txt")
	write(t, big, strings.Repeat("x", maxFileBytes+1))
	write(t, small, "small\n")

	r := NewRecorder()
	r.Begin("bulk change")
	r.Record(big)
	r.Record(small)
	write(t, small, "changed\n")

	turns := r.Turns()
	if len(turns) != 1 || !turns[0].Partial {
		t.Fatalf("turns = %+v, want one partial turn", turns)
	}

	_, _, skipped, _, _, err := r.Undo()
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0] != big {
		t.Errorf("skipped = %v, want the oversize file named", skipped)
	}
	if got := readBack(t, small); got != "small\n" {
		t.Errorf("small = %q, want restored despite the oversize sibling", got)
	}
}

func TestRecordOutsideATurnIsIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	write(t, path, "v1\n")

	r := NewRecorder()
	r.Record(path)
	if turns := r.Turns(); len(turns) != 0 {
		t.Errorf("a capture with no turn scope must not invent a checkpoint: %v", turns)
	}
}

func TestPendingFilesCountsTheOpenScope(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	write(t, a, "one")
	write(t, b, "two")

	r := NewRecorder()
	if r.PendingFiles() != 0 {
		t.Error("counted files with no scope open")
	}

	r.Begin("first turn")
	if r.PendingFiles() != 0 {
		t.Error("counted files before any capture")
	}
	r.Record(a)
	r.Record(a) // the same file twice is one capture
	if got := r.PendingFiles(); got != 1 {
		t.Errorf("want 1 pending after one capture, got %d", got)
	}
	r.Record(b)
	if got := r.PendingFiles(); got != 2 {
		t.Errorf("want 2 pending, got %d", got)
	}

	// A new turn opens a fresh scope; the committed turn no longer counts.
	r.Begin("second turn")
	if got := r.PendingFiles(); got != 0 {
		t.Errorf("the previous turn's captures leaked into the new scope: %d", got)
	}
}

// Details is Turns with the paths attached: the same evidence Undo restores
// from, shaped for a surface that says what a session touched.
func TestDetailsNamesThePathsPerTurn(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	b := filepath.Join(dir, "b.go")
	os.WriteFile(a, []byte("a"), 0o644)

	r := NewRecorder()
	r.Begin("first turn")
	r.Record(a)
	r.Begin("second turn")
	r.Record(a)
	r.Record(b)

	details := r.Details()
	if len(details) != 2 {
		t.Fatalf("got %d turns, want 2", len(details))
	}
	if details[0].Label != "first turn" || len(details[0].Paths) != 1 {
		t.Fatalf("first turn: %+v", details[0])
	}
	if details[1].Label != "second turn" || len(details[1].Paths) != 2 {
		t.Fatalf("second turn: %+v", details[1])
	}
	if details[1].Paths[0] != a {
		t.Fatalf("paths are not sorted: %v", details[1].Paths)
	}
}

// UndoFile takes back one file, not the turn: the newest capture of that
// file restores, the capture is consumed only on success, the turn's other
// files stay, and a turn left with nothing disappears from the stack.
func TestUndoFileRestoresOneAndConsumesTheCapture(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	b := filepath.Join(dir, "b.go")
	os.WriteFile(a, []byte("a before"), 0o644)

	r := NewRecorder()
	r.Begin("the turn")
	r.Record(a) // existed: restore rewrites it
	r.Record(b) // absent: restore removes it
	os.WriteFile(a, []byte("a after"), 0o644)
	os.WriteFile(b, []byte("b created"), 0o644)

	outcome, label, err := r.UndoFile(a)
	if err != nil || !outcome.Published || outcome.Removed || label != "the turn" {
		t.Fatalf("UndoFile(a) = %+v %q %v", outcome, label, err)
	}
	if got, _ := os.ReadFile(a); string(got) != "a before" {
		t.Fatalf("a holds %q, want its pre-turn content", got)
	}
	if _, err := os.Stat(b); err != nil {
		t.Fatal("taking back a took b with it")
	}
	if details := r.Details(); len(details) != 1 || len(details[0].Paths) != 1 {
		t.Fatalf("the turn should still hold b alone: %+v", details)
	}

	if _, _, err := r.UndoFile(a); err == nil {
		t.Fatal("a consumed capture restored twice")
	}

	outcome, _, err = r.UndoFile(b)
	if err != nil || !outcome.Published || !outcome.Removed {
		t.Fatalf("UndoFile(b) = %+v %v, want removed", outcome, err)
	}
	if _, err := os.Stat(b); !os.IsNotExist(err) {
		t.Fatal("the created file survived its undo")
	}
	if details := r.Details(); len(details) != 0 {
		t.Fatalf("an emptied turn stayed on the stack: %+v", details)
	}
}

func TestStateBeforeTakesTheOldestPreimageInRange(t *testing.T) {
	dir := t.TempDir()
	churned := filepath.Join(dir, "churned.go")
	late := filepath.Join(dir, "late.go")
	write(t, churned, "v0\n")

	r := NewRecorder()
	r.Begin("turn 0")
	r.Record(churned)
	write(t, churned, "v1\n")

	r.Begin("turn 1")
	r.Record(churned)
	write(t, churned, "v2\n")

	r.Begin("turn 2") // the open scope counts too
	r.Record(late)
	write(t, late, "created in turn 2\n")

	before0 := r.StateBefore(0)
	if got := string(before0[churned].Content); got != "v0\n" {
		t.Errorf("before turn 0, churned = %q, want the oldest pre-image", got)
	}
	if before0[late].Existed {
		t.Error("before turn 0, late.go had not been created")
	}

	before1 := r.StateBefore(1)
	if got := string(before1[churned].Content); got != "v1\n" {
		t.Errorf("before turn 1, churned = %q, want turn 1's own pre-image", got)
	}

	before2 := r.StateBefore(2)
	if _, ok := before2[churned]; ok {
		t.Error("no turn from 2 onward captured churned; its state then is whatever it holds now")
	}
	if before2[late].Existed {
		t.Error("before turn 2, late.go had not been created")
	}
}

func TestAbortPreparedCaptureLeavesNoCheckpoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.txt")
	write(t, path, "before\n")

	r := NewRecorder()
	r.Begin("failed edit")
	r.RecordState(path, true, 0o644, []byte("before\n"))
	r.Abort(path)

	if turns := r.Turns(); len(turns) != 0 {
		t.Fatalf("aborted mutation created a checkpoint: %+v", turns)
	}
	if snapshots := r.Snapshots(); len(snapshots) != 0 {
		t.Fatalf("aborted mutation appeared in review evidence: %+v", snapshots)
	}
}

func TestCommittedEditsKeepFirstPreimageAndAdvancePostimage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.txt")
	write(t, path, "v1")
	r := NewRecorder()
	r.Begin("two edits")

	r.RecordState(path, true, 0o644, []byte("v1"))
	write(t, path, "v2")
	r.Commit(path, true, 0o644, sha256.Sum256([]byte("v2")))
	r.RecordState(path, true, 0o644, []byte("v2"))
	write(t, path, "v3")
	r.Commit(path, true, 0o644, sha256.Sum256([]byte("v3")))

	snapshots := r.Snapshots()
	if len(snapshots) != 1 || len(snapshots[0].Files) != 1 {
		t.Fatalf("snapshots=%+v", snapshots)
	}
	if got := string(snapshots[0].Files[0].Before.Content); got != "v1" {
		t.Fatalf("first preimage was replaced by intra-turn churn: %q", got)
	}
	if got := snapshots[0].Files[0].After.Digest; got != sha256.Sum256([]byte("v3")) {
		t.Fatalf("postimage did not advance to v3: %x", got)
	}
	if _, _, _, failed, _, err := r.Undo(); err != nil || len(failed) != 0 {
		t.Fatalf("undo: failed=%v err=%v", failed, err)
	}
	if got := readBack(t, path); got != "v1" {
		t.Fatalf("undo restored %q, want v1", got)
	}
}

func TestAbortLaterEditKeepsEarlierCommittedCapture(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.txt")
	write(t, path, "v1")
	r := NewRecorder()
	r.Begin("one success, one failure")
	r.RecordState(path, true, 0o644, []byte("v1"))
	write(t, path, "v2")
	r.Commit(path, true, 0o644, sha256.Sum256([]byte("v2")))
	r.RecordState(path, true, 0o644, []byte("v2"))
	r.Abort(path)

	if _, _, _, failed, _, err := r.Undo(); err != nil || len(failed) != 0 {
		t.Fatalf("undo: failed=%v err=%v", failed, err)
	}
	if got := readBack(t, path); got != "v1" {
		t.Fatalf("undo restored %q, want v1", got)
	}
}

func waitForTransitionWaiter(t *testing.T, r *Recorder) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		waiters := r.transitionWaiters
		r.mu.Unlock()
		if waiters > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("transition never reached the in-flight transaction barrier")
}

func TestUndoWaitsForTwoPhaseCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.txt")
	write(t, path, "before")
	r := NewRecorder()
	r.Begin("in flight")
	r.RecordState(path, true, 0o644, []byte("before"))

	type undoResult struct {
		restored []string
		failed   []string
		err      error
	}
	done := make(chan undoResult, 1)
	go func() {
		restored, _, _, failed, _, err := r.Undo()
		done <- undoResult{restored: restored, failed: failed, err: err}
	}()
	waitForTransitionWaiter(t, r)
	select {
	case got := <-done:
		t.Fatalf("undo crossed an active RecordState before Commit: %+v", got)
	default:
	}

	write(t, path, "after")
	r.Commit(path, true, 0o644, sha256.Sum256([]byte("after")))
	select {
	case got := <-done:
		if got.err != nil || len(got.failed) != 0 || len(got.restored) != 1 || got.restored[0] != path {
			t.Fatalf("undo after commit: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("undo did not resume after Commit")
	}
	if got := readBack(t, path); got != "before" {
		t.Fatalf("file=%q, want committed mutation restored", got)
	}
	if snapshots := r.Snapshots(); len(snapshots) != 0 {
		t.Fatalf("successful undo failed to consume the capture: %+v", snapshots)
	}
}

func TestBeginWaitsForTwoPhaseCommitAndKeepsEvidenceAttached(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.txt")
	write(t, path, "before")
	r := NewRecorder()
	r.Begin("first")
	r.RecordState(path, true, 0o644, []byte("before"))

	done := make(chan struct{})
	go func() {
		r.Begin("second")
		close(done)
	}()
	waitForTransitionWaiter(t, r)
	select {
	case <-done:
		t.Fatal("Begin crossed an active RecordState before Commit")
	default:
	}

	write(t, path, "after")
	r.Commit(path, true, 0o644, sha256.Sum256([]byte("after")))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Begin did not resume after Commit")
	}
	snapshots := r.Snapshots()
	if len(snapshots) != 1 || snapshots[0].Label != "first" || snapshots[0].Open || len(snapshots[0].Files) != 1 {
		t.Fatalf("commit detached from its original turn: %+v", snapshots)
	}
	if snapshots[0].Files[0].After.Digest != sha256.Sum256([]byte("after")) {
		t.Fatalf("committed post-image was lost: %+v", snapshots[0].Files[0].After)
	}
	if _, _, _, failed, label, err := r.Undo(); err != nil || len(failed) != 0 || label != "first" {
		t.Fatalf("undo after Begin: label=%q failed=%v err=%v", label, failed, err)
	}
	if got := readBack(t, path); got != "before" {
		t.Fatalf("file=%q, want before", got)
	}
}

func TestBeginWaitsForEveryOverlappingRecordState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.txt")
	write(t, path, "before")
	r := NewRecorder()
	r.Begin("overlapping")
	r.RecordState(path, true, 0o644, []byte("before"))
	r.RecordState(path, true, 0o644, []byte("before"))

	done := make(chan struct{})
	go func() {
		r.Begin("next")
		close(done)
	}()
	waitForTransitionWaiter(t, r)

	write(t, path, "after")
	r.Commit(path, true, 0o644, sha256.Sum256([]byte("after")))
	select {
	case <-done:
		t.Fatal("Begin crossed the second active RecordState after only one Commit")
	default:
	}

	r.Abort(path)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Begin did not resume after both overlapping transactions finished")
	}
	snapshots := r.Snapshots()
	if len(snapshots) != 1 || snapshots[0].Label != "overlapping" || snapshots[0].Open || len(snapshots[0].Files) != 1 {
		t.Fatalf("overlapping transaction evidence=%+v", snapshots)
	}
	if snapshots[0].Files[0].After.Digest != sha256.Sum256([]byte("after")) {
		t.Fatalf("successful overlapping transaction post-image was lost: %+v", snapshots[0].Files[0].After)
	}
}

func TestUndoRefusesExternalChangeAndRetainsCapture(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.txt")
	write(t, path, "before\n")

	r := NewRecorder()
	r.Begin("successful edit")
	r.RecordState(path, true, 0o644, []byte("before\n"))
	write(t, path, "after\n")
	r.Commit(path, true, 0o644, sha256.Sum256([]byte("after\n")))
	write(t, path, "external\n")

	restored, _, _, failed, _, err := r.Undo()
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 0 || len(failed) != 1 || !strings.Contains(failed[0], ErrStale.Error()) {
		t.Fatalf("restored=%v failed=%v, want a stale refusal", restored, failed)
	}
	if got := readBack(t, path); got != "external\n" {
		t.Fatalf("stale undo overwrote external content: %q", got)
	}
	if details := r.Details(); len(details) != 1 || len(details[0].Paths) != 1 {
		t.Fatalf("failed restore was consumed: %+v", details)
	}

	// Returning the path to the recorded post-image makes the retained
	// capture retryable.
	write(t, path, "after\n")
	if _, _, _, failed, _, err := r.Undo(); err != nil || len(failed) != 0 {
		t.Fatalf("retry failed: failed=%v err=%v", failed, err)
	}
	if got := readBack(t, path); got != "before\n" {
		t.Fatalf("retry restored %q, want before", got)
	}
}

func TestWholeTurnPartialFailureRemainsRetryable(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	write(t, a, "a before")
	write(t, b, "b before")

	r := NewRecorder()
	r.Begin("two files")
	for path, before := range map[string]string{a: "a before", b: "b before"} {
		r.RecordState(path, true, 0o644, []byte(before))
		after := before + " after"
		write(t, path, after)
		r.Commit(path, true, 0o644, sha256.Sum256([]byte(after)))
	}
	write(t, b, "external")

	restored, _, _, failed, _, err := r.Undo()
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 1 || restored[0] != a || len(failed) != 1 {
		t.Fatalf("restored=%v failed=%v", restored, failed)
	}
	if details := r.Details(); len(details) != 1 || len(details[0].Paths) != 1 || details[0].Paths[0] != b {
		t.Fatalf("successful and failed restores were not consumed independently: %+v", details)
	}

	write(t, b, "b before after")
	if restored, _, _, failed, _, err = r.Undo(); err != nil || len(failed) != 0 || len(restored) != 1 || restored[0] != b {
		t.Fatalf("retry: restored=%v failed=%v err=%v", restored, failed, err)
	}
	if got := readBack(t, b); got != "b before" {
		t.Fatalf("b = %q after retry", got)
	}
}

func TestUndoRestoresExactModeAndBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "script")
	if err := os.WriteFile(path, []byte("before\r\nwithout-final-newline"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := NewRecorder()
	r.Begin("rewrite")
	r.RecordState(path, true, 0o755, []byte("before\r\nwithout-final-newline"))
	if err := os.WriteFile(path, []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	r.Commit(path, true, 0o644, sha256.Sum256([]byte("after\n")))
	if _, _, _, failed, _, err := r.Undo(); err != nil || len(failed) != 0 {
		t.Fatalf("undo: failed=%v err=%v", failed, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "before\r\nwithout-final-newline" {
		t.Fatalf("content=%q err=%v", got, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := restorableMode(0o755); info.Mode().Perm() != want.Perm() {
		t.Fatalf("mode=%o, want %o", info.Mode().Perm(), want.Perm())
	}
}

func TestUndoFileReturnsStaleSentinelAndRetainsCapture(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	write(t, path, "before")
	r := NewRecorder()
	r.Begin("edit")
	r.RecordState(path, true, 0o644, []byte("before"))
	write(t, path, "after")
	r.Commit(path, true, 0o644, sha256.Sum256([]byte("after")))
	write(t, path, "other")

	outcome, _, err := r.UndoFile(path)
	if outcome.Published || !errors.Is(err, ErrStale) {
		t.Fatalf("UndoFile outcome=%+v error=%v, want unpublished ErrStale", outcome, err)
	}
	if details := r.Details(); len(details) != 1 {
		t.Fatalf("stale UndoFile consumed capture: %+v", details)
	}
}

func TestUndoReportsPublishedRemovalAndConsumesCaptureAfterLaterFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "created")
	r := NewRecorder()
	r.Begin("create")
	r.RecordState(path, false, 0, nil)
	write(t, path, "created by the turn")
	r.Commit(path, true, 0o644, sha256.Sum256([]byte("created by the turn")))

	injected := errors.New("injected failure after remove")
	r.afterRemoveHook = func() error { return injected }
	restored, removed, skipped, failed, label, err := r.Undo()
	if err != nil {
		t.Fatal(err)
	}
	if label != "create" || len(restored) != 0 || len(skipped) != 0 ||
		len(removed) != 1 || removed[0] != path || len(failed) != 1 ||
		!strings.Contains(failed[0], path) || !strings.Contains(failed[0], injected.Error()) {
		t.Fatalf("restored=%v removed=%v skipped=%v failed=%v label=%q", restored, removed, skipped, failed, label)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("published removal left target present: %v", statErr)
	}
	if details := r.Details(); len(details) != 0 {
		t.Fatalf("published removal retained stale capture: %+v", details)
	}
	if _, _, _, _, _, retryErr := r.Undo(); retryErr == nil {
		t.Fatal("published removal remained retryable")
	}
}

func TestUndoFileReportsPublishedReplaceAndConsumesCaptureAfterLaterFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	write(t, path, "before")
	r := NewRecorder()
	r.Begin("replace")
	r.RecordState(path, true, 0o644, []byte("before"))
	write(t, path, "after")
	r.Commit(path, true, 0o644, sha256.Sum256([]byte("after")))

	injected := errors.New("injected failure after replace")
	r.afterReplaceHook = func() error { return injected }
	outcome, label, err := r.UndoFile(path)
	if !outcome.Published || outcome.Removed || label != "replace" || !errors.Is(err, injected) ||
		!strings.Contains(err.Error(), path) {
		t.Fatalf("outcome=%+v label=%q err=%v", outcome, label, err)
	}
	if got := readBack(t, path); got != "before" {
		t.Fatalf("published replacement holds %q, want before", got)
	}
	if details := r.Details(); len(details) != 0 {
		t.Fatalf("published replacement retained stale capture: %+v", details)
	}
	if _, _, retryErr := r.UndoFile(path); retryErr == nil {
		t.Fatal("published replacement remained retryable")
	}
}

func TestSnapshotsCloneSuccessfulEvidence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	write(t, path, "before")
	r := NewRecorder()
	r.Begin("edit")
	r.RecordState(path, true, 0o644, []byte("before"))
	if got := r.Snapshots(); len(got) != 0 {
		t.Fatalf("prepared mutation appeared successful: %+v", got)
	}
	write(t, path, "after")
	digest := sha256.Sum256([]byte("after"))
	r.Commit(path, true, 0o644, digest)

	got := r.Snapshots()
	if len(got) != 1 || !got[0].Open || len(got[0].Files) != 1 {
		t.Fatalf("snapshots=%+v", got)
	}
	file := got[0].Files[0]
	if string(file.Before.Content) != "before" || file.After.Digest != digest {
		t.Fatalf("snapshot=%+v", file)
	}
	got[0].Files[0].Before.Content[0] = 'X'
	if again := r.Snapshots(); string(again[0].Files[0].Before.Content) != "before" {
		t.Fatal("snapshot content aliases recorder state")
	}
}

func TestCurrentSnapshotReportsEmptyOpenTurn(t *testing.T) {
	r := NewRecorder()
	r.Begin("no-op turn")
	current, index, ok := r.CurrentSnapshot()
	if !ok || index != 1 || !current.Open || current.Label != "no-op turn" || len(current.Files) != 0 || len(current.Skipped) != 0 {
		t.Fatalf("empty current snapshot: index=%d ok=%v snapshot=%+v", index, ok, current)
	}
	if snapshots := r.Snapshots(); len(snapshots) != 0 {
		t.Fatalf("historical snapshots changed empty-turn filtering: %+v", snapshots)
	}
}

func TestReviewCursorBindsExactRevisionAndBoundsSnapshotClone(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder()
	r.Begin("bounded")
	for _, name := range []string{"a", "b", "c"} {
		path := filepath.Join(dir, name)
		write(t, path, "old-"+name)
		r.RecordState(path, true, 0o644, []byte("old-"+name))
		write(t, path, "new-"+name)
		r.Commit(path, true, 0o644, sha256.Sum256([]byte("new-"+name)))
	}

	cursor, index, hasMutations, ok := r.CurrentReviewCursor()
	if !ok || !hasMutations || index != 1 {
		t.Fatalf("cursor=(index %d, mutations %v, ok %v)", index, hasMutations, ok)
	}
	snapshot, omitted, err := r.ReviewSnapshot(cursor, 2, len("old-a"))
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Files) != 1 || snapshot.Files[0].Path != filepath.Join(dir, "a") || omitted != 2 {
		t.Fatalf("bounded snapshot=%+v omitted=%d", snapshot, omitted)
	}

	r.Begin("next")
	if r.ReviewCursorValid(cursor) {
		t.Fatal("cursor remained valid across Begin")
	}
	if _, _, err := r.ReviewSnapshot(cursor, 2, 32); !errors.Is(err, ErrStale) {
		t.Fatalf("stale cursor error=%v, want ErrStale", err)
	}
}

func TestReadSnapshotCurrentRequiresExactCommittedPostimage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	write(t, path, "before")
	r := NewRecorder()
	r.Begin("edit")
	r.RecordState(path, true, 0o755, []byte("before"))
	write(t, path, "after")
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	r.Commit(path, true, 0o640, sha256.Sum256([]byte("after")))

	snapshot := r.Snapshots()[0].Files[0]
	current, err := r.ReadSnapshotCurrent(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !current.Existed || current.Mode.Perm() != restorableMode(0o640).Perm() || string(current.Content) != "after" {
		t.Fatalf("current=%+v content=%q", current, current.Content)
	}

	write(t, path, "external")
	if _, err := r.ReadSnapshotCurrent(snapshot); !errors.Is(err, ErrStale) {
		t.Fatalf("stale read error=%v, want ErrStale", err)
	}

	// Public snapshot fields are evidence, not authority a caller may rewrite.
	snapshot.Before.Content[0] = 'X'
	if _, err := r.ReadSnapshotCurrent(snapshot); !errors.Is(err, ErrStale) {
		t.Fatalf("mutated snapshot error=%v, want ErrStale", err)
	}
}

func TestReadSnapshotCurrentRepresentsCommittedDeletion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	write(t, path, "before")
	r := NewRecorder()
	r.Begin("delete")
	r.RecordState(path, true, 0o644, []byte("before"))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	r.Commit(path, false, 0, [sha256.Size]byte{})

	current, err := r.ReadSnapshotCurrent(r.Snapshots()[0].Files[0])
	if err != nil {
		t.Fatal(err)
	}
	if current.Existed || current.Mode != 0 || current.Content != nil {
		t.Fatalf("deleted current state=%+v", current)
	}
}

func TestReadSnapshotCurrentBoundsVerifiedPostimage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	write(t, path, "before")
	after := []byte(strings.Repeat("x", maxFileBytes+1))
	r := NewRecorder()
	r.Begin("large rewrite")
	r.RecordState(path, true, 0o644, []byte("before"))
	if err := os.WriteFile(path, after, 0o644); err != nil {
		t.Fatal(err)
	}
	r.Commit(path, true, 0o644, sha256.Sum256(after))

	current, err := r.ReadSnapshotCurrent(r.Snapshots()[0].Files[0])
	if !errors.Is(err, ErrSnapshotTooLarge) {
		t.Fatalf("error=%v, want ErrSnapshotTooLarge", err)
	}
	if !current.Existed || current.Mode.Perm() != restorableMode(0o644).Perm() || current.Content != nil {
		t.Fatalf("bounded state=%+v", current)
	}
}

func TestReadSnapshotCurrentHonorsCallerAggregateBound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	write(t, path, "before")
	r := NewRecorder()
	r.Begin("edit")
	r.RecordState(path, true, 0o644, []byte("before"))
	write(t, path, "after value")
	r.Commit(path, true, 0o644, sha256.Sum256([]byte("after value")))
	snapshot := r.Snapshots()[0].Files[0]

	current, err := r.ReadSnapshotCurrentBounded(snapshot, 5)
	if !errors.Is(err, ErrSnapshotTooLarge) || !current.Existed || current.Content != nil {
		t.Fatalf("bounded current=%+v err=%v", current, err)
	}
	current, err = r.ReadSnapshotCurrent(snapshot)
	if err != nil || string(current.Content) != "after value" {
		t.Fatalf("default current=%+v err=%v", current, err)
	}
}

func TestReadSnapshotCurrentRejectsHugeReplacementBySize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	write(t, path, "before")
	r := NewRecorder()
	r.Begin("edit")
	r.RecordState(path, true, 0o644, []byte("before"))
	write(t, path, "after")
	r.Commit(path, true, 0o644, sha256.Sum256([]byte("after")))
	snapshot := r.Snapshots()[0].Files[0]

	if err := os.Truncate(path, 1<<32); err != nil {
		t.Skipf("filesystem cannot create a sparse replacement: %v", err)
	}
	if _, err := r.ReadSnapshotCurrent(snapshot); !errors.Is(err, ErrStale) {
		t.Fatalf("huge replacement error=%v, want ErrStale", err)
	}
}

func TestReadSnapshotCurrentIOLeavesLifecycleUnlocked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	write(t, path, "before")
	r := NewRecorder()
	r.Begin("edit")
	r.RecordState(path, true, 0o644, []byte("before"))
	write(t, path, "after")
	r.Commit(path, true, 0o644, sha256.Sum256([]byte("after")))
	snapshot := r.Snapshots()[0].Files[0]

	opened := make(chan struct{})
	release := make(chan struct{})
	r.snapshotAfterOpenHook = func() {
		close(opened)
		<-release
	}
	readDone := make(chan error, 1)
	go func() {
		_, err := r.ReadSnapshotCurrent(snapshot)
		readDone <- err
	}()
	<-opened

	beginDone := make(chan struct{})
	go func() {
		r.Begin("next")
		close(beginDone)
	}()
	select {
	case <-beginDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Begin was blocked by snapshot file I/O")
	}
	close(release)
	if err := <-readDone; err != nil {
		t.Fatalf("stable unlocked read failed: %v", err)
	}
}

func TestReadSnapshotCurrentCapsGrowthAfterOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	write(t, path, "before")
	r := NewRecorder()
	r.Begin("edit")
	r.RecordState(path, true, 0o644, []byte("before"))
	write(t, path, "after")
	r.Commit(path, true, 0o644, sha256.Sum256([]byte("after")))
	snapshot := r.Snapshots()[0].Files[0]
	r.snapshotAfterOpenHook = func() {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := f.WriteString("x"); err != nil {
			t.Error(err)
		}
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	}
	if _, err := r.ReadSnapshotCurrent(snapshot); !errors.Is(err, ErrStale) {
		t.Fatalf("growing file error=%v, want ErrStale", err)
	}
}

func TestReadSnapshotCurrentRefusesCommittedPathWithActiveMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	write(t, path, "before")
	r := NewRecorder()
	r.Begin("edit")
	r.RecordState(path, true, 0o644, []byte("before"))
	write(t, path, "after")
	r.Commit(path, true, 0o644, sha256.Sum256([]byte("after")))
	snapshot := r.Snapshots()[0].Files[0]

	r.RecordState(path, true, 0o644, []byte("after"))
	if _, err := r.ReadSnapshotCurrent(snapshot); !errors.Is(err, ErrStale) {
		t.Fatalf("active mutation read error=%v, want ErrStale", err)
	}
	r.Abort(path)
	if _, err := r.ReadSnapshotCurrent(snapshot); err != nil {
		t.Fatalf("read after active mutation aborted: %v", err)
	}
}

func TestUndoFileBlocksNewTurnUntilRestoreFinishes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	write(t, path, "before")
	r := NewRecorder()
	r.Begin("edit")
	r.RecordState(path, true, 0o644, []byte("before"))
	write(t, path, "after")
	r.Commit(path, true, 0o644, sha256.Sum256([]byte("after")))
	restoreStarted := make(chan struct{})
	releaseRestore := make(chan struct{})
	r.restoreHook = func() {
		close(restoreStarted)
		<-releaseRestore
	}
	undoDone := make(chan error, 1)
	go func() {
		_, _, err := r.UndoFile(path)
		undoDone <- err
	}()
	<-restoreStarted

	beginDone := make(chan struct{})
	mutationDone := make(chan struct{})
	go func() {
		r.Begin("new turn")
		close(beginDone)
		r.RecordState(path, true, 0o644, []byte("before"))
		r.Abort(path)
		close(mutationDone)
	}()
	waitForTransitionWaiter(t, r)
	select {
	case <-beginDone:
		t.Fatal("a new turn crossed an in-flight restore")
	default:
	}

	close(releaseRestore)
	if err := <-undoDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-mutationDone:
	case <-time.After(2 * time.Second):
		t.Fatal("new mutation did not resume after restore")
	}
	if got := readBack(t, path); got != "before" {
		t.Fatalf("file=%q, want restored pre-image", got)
	}
}

func TestUndoFilePreservesEvidenceForWaitingRecordState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	write(t, path, "before")
	r := NewRecorder()
	r.Begin("first")
	r.RecordState(path, true, 0o644, []byte("before"))
	write(t, path, "after")
	r.Commit(path, true, 0o644, sha256.Sum256([]byte("after")))

	restoreStarted := make(chan struct{})
	releaseRestore := make(chan struct{})
	r.restoreHook = func() {
		close(restoreStarted)
		<-releaseRestore
	}
	undoDone := make(chan error, 1)
	go func() {
		_, _, err := r.UndoFile(path)
		undoDone <- err
	}()
	<-restoreStarted

	mutationDone := make(chan error, 1)
	go func() {
		r.RecordState(path, true, 0o644, []byte("before"))
		if err := os.WriteFile(path, []byte("racing mutation"), 0o644); err != nil {
			mutationDone <- err
			return
		}
		r.Commit(path, true, 0o644, sha256.Sum256([]byte("racing mutation")))
		mutationDone <- nil
	}()
	waitForTransitionWaiter(t, r)
	close(releaseRestore)
	if err := <-undoDone; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-mutationDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiting RecordState did not resume after undo")
	}

	current, _, ok := r.CurrentSnapshot()
	if !ok || len(current.Files) != 1 || string(current.Files[0].Before.Content) != "before" ||
		current.Files[0].After.Digest != sha256.Sum256([]byte("racing mutation")) {
		t.Fatalf("waiting mutation published without evidence: %+v", current)
	}
}

func TestReadSnapshotCurrentRefusesTargetSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	outside := filepath.Join(dir, "outside")
	write(t, path, "before")
	write(t, outside, "after")
	r := NewRecorder()
	r.Begin("edit")
	r.RecordState(path, true, 0o644, []byte("before"))
	write(t, path, "after")
	r.Commit(path, true, 0o644, sha256.Sum256([]byte("after")))
	snapshot := r.Snapshots()[0].Files[0]
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := r.ReadSnapshotCurrent(snapshot); !errors.Is(err, ErrStale) {
		t.Fatalf("symlink read error=%v, want ErrStale", err)
	}
}
