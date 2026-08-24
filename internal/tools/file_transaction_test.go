package tools

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/execution"
)

func TestFailedAndNoopEditsCreateNoCheckpoint(t *testing.T) {
	r, root := newRegistry(t)
	rec := newCheckpointRecorder(t, root)
	r.SetCheckpoints(rec)
	path := filepath.Join(root, "target.txt")
	writeFile(t, path, "same\nsame\n")
	run(t, r, "read", map[string]any{"path": "target.txt"})

	tests := []struct {
		name  string
		input map[string]any
	}{
		{"missing match", map[string]any{"path": "target.txt", "old_string": "absent", "new_string": "new"}},
		{"ambiguous match", map[string]any{"path": "target.txt", "old_string": "same", "new_string": "new"}},
		{"missing file", map[string]any{"path": "missing.txt", "old_string": "old", "new_string": "new"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec.Begin(tt.name)
			res := run(t, r, "edit", tt.input)
			if !res.IsError {
				t.Fatalf("edit unexpectedly succeeded: %+v", res)
			}
			if turns := rec.Turns(); len(turns) != 0 {
				t.Fatalf("failed edit created checkpoint: %+v", turns)
			}
		})
	}

	rec.Begin("plan-time no-op")
	if _, err := tryRun(r, "edit", map[string]any{
		"path": "target.txt", "old_string": "same", "new_string": "same",
	}); err == nil {
		t.Fatal("identical edit must fail validation")
	}
	if turns := rec.Turns(); len(turns) != 0 {
		t.Fatalf("no-op edit created checkpoint: %+v", turns)
	}
}

func TestTransactionalEditPreservesCRLFAndNoFinalNewline(t *testing.T) {
	r, root := newRegistry(t)
	path := filepath.Join(root, "target.txt")
	writeFile(t, path, "one\r\ntwo\r\nlast")
	run(t, r, "read", map[string]any{"path": "target.txt"})

	res := run(t, r, "edit", map[string]any{
		"path": "target.txt", "old_string": "two", "new_string": "second",
	})
	if res.IsError {
		t.Fatalf("edit failed: %s", res.Content)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "one\r\nsecond\r\nlast" {
		t.Fatalf("content=%q", got)
	}
}

func TestTransactionalWritePreservesExistingMode(t *testing.T) {
	r, root := newRegistry(t)
	path := filepath.Join(root, "script")
	writeFile(t, path, "old")
	if err := os.Chmod(path, 0o751); err != nil {
		t.Fatal(err)
	}
	run(t, r, "read", map[string]any{"path": "script"})
	if res := run(t, r, "write", map[string]any{"path": "script", "content": "new"}); res.IsError {
		t.Fatalf("write failed: %s", res.Content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := restorableFileMode(0o751); info.Mode().Perm() != want.Perm() {
		t.Fatalf("mode=%o, want %o", info.Mode().Perm(), want.Perm())
	}
}

func TestTransactionalCreationPublishesExactBytesWithoutTempLeak(t *testing.T) {
	r, root := newRegistry(t)
	content := "first\r\nlast-without-newline"
	if res := run(t, r, "write", map[string]any{
		"path": "nested/deeper/new.txt", "content": content,
	}); res.IsError {
		t.Fatalf("creation failed: %s", res.Content)
	}
	path := filepath.Join(root, "nested", "deeper", "new.txt")
	got, err := os.ReadFile(path)
	if err != nil || string(got) != content {
		t.Fatalf("content=%q err=%v", got, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".switchboard-write-") {
			t.Fatalf("temporary file leaked: %s", entry.Name())
		}
	}
}

func TestMutationWithoutAtomicRecorderFailsBeforeFilesystemChange(t *testing.T) {
	root := t.TempDir()
	r, err := NewRegistry(root, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	result := run(t, r, "write", map[string]any{
		"path": "nested/new.txt", "content": "must not publish",
	})
	if !result.IsError || !strings.Contains(result.Content, "checkpoint recorder") {
		t.Fatalf("unprotected mutation = %+v, want atomic-recorder refusal", result)
	}
	if _, err := os.Lstat(filepath.Join(root, "nested")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused mutation created its parent or target: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("refused mutation left filesystem artifacts: %v", entries)
	}
}

func TestTransactionalMutationPostImageBound(t *testing.T) {
	assertNoMutationArtifacts := func(t *testing.T, dirs ...string) {
		t.Helper()
		for _, dir := range dirs {
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".switchboard-write-") {
					t.Fatalf("mutation temporary leaked in %s: %s", dir, entry.Name())
				}
			}
		}
	}
	assertOutsideUnchanged := func(t *testing.T, path string) {
		t.Helper()
		got, err := os.ReadFile(path)
		if err != nil || string(got) != "outside sentinel" {
			t.Fatalf("outside sentinel changed: bytes=%q err=%v", got, err)
		}
	}

	for _, tc := range []struct {
		name  string
		tool  string
		size  int
		exact bool
	}{
		{name: "write exact limit", tool: "write", size: int(maxWorkspaceFileBytes), exact: true},
		{name: "write one over", tool: "write", size: int(maxWorkspaceFileBytes) + 1},
		{name: "edit exact limit", tool: "edit", size: int(maxWorkspaceFileBytes), exact: true},
		{name: "edit one over", tool: "edit", size: int(maxWorkspaceFileBytes) + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, root := newRegistry(t)
			recorder := newCheckpointRecorder(t, root)
			r.SetCheckpoints(recorder)
			outside := t.TempDir()
			outsideSentinel := filepath.Join(outside, "sentinel")
			writeFile(t, outsideSentinel, "outside sentinel")

			target := filepath.Join(root, "target.txt")
			input := map[string]any{
				"path":    "target.txt",
				"content": strings.Repeat("w", tc.size),
			}
			if tc.tool == "edit" {
				writeFile(t, target, "x")
				if result := run(t, r, "read", map[string]any{"path": "target.txt"}); result.IsError {
					t.Fatalf("arming edit read: %s", result.Content)
				}
				input = map[string]any{
					"path": "target.txt", "old_string": "x",
					"new_string": strings.Repeat("e", tc.size),
				}
			}

			recorder.Begin(tc.name)
			result := run(t, r, tc.tool, input)
			if tc.exact {
				if result.IsError {
					t.Fatalf("exact-limit %s failed: %s", tc.tool, result.Content)
				}
				info, err := os.Stat(target)
				if err != nil || info.Size() != maxWorkspaceFileBytes {
					t.Fatalf("exact-limit post-image size=%v err=%v, want %d", info, err, maxWorkspaceFileBytes)
				}
				if turns := recorder.Turns(); len(turns) != 1 || turns[0].Files != 1 || turns[0].Partial {
					t.Fatalf("exact-limit checkpoint = %+v", turns)
				}
			} else {
				if !result.IsError || !strings.Contains(result.Content, "mutation file limit") {
					t.Fatalf("oversized %s result = %+v", tc.tool, result)
				}
				if tc.tool == "write" {
					if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("oversized write created a partial target: %v", err)
					}
				} else if got, err := os.ReadFile(target); err != nil || string(got) != "x" {
					t.Fatalf("oversized edit changed its source: bytes=%q err=%v", got, err)
				}
				if turns := recorder.Turns(); len(turns) != 0 {
					t.Fatalf("oversized %s prepared a checkpoint: %+v", tc.tool, turns)
				}
			}
			assertNoMutationArtifacts(t, root, outside)
			assertOutsideUnchanged(t, outsideSentinel)
		})
	}
}

func TestInjectedPrecommitRaceRefusesAndAbortsCheckpoint(t *testing.T) {
	r, root := newRegistry(t)
	rec := newCheckpointRecorder(t, root)
	r.SetCheckpoints(rec)
	path := filepath.Join(root, "target.txt")
	writeFile(t, path, "source")
	run(t, r, "read", map[string]any{"path": "target.txt"})
	rec.Begin("raced write")

	tx, res, ok := r.prepareFileMutation(path, false)
	if !ok {
		t.Fatalf("prepare failed: %s", res.Content)
	}
	defer tx.close()
	err := tx.publish(context.Background(), []byte("agent"), tx.before.mode, func() {
		if writeErr := os.WriteFile(path, []byte("external"), 0o644); writeErr != nil {
			t.Fatalf("injecting race: %v", writeErr)
		}
	})
	if err == nil || !errors.Is(err, checkpoint.ErrStale) {
		t.Fatalf("publish error=%v, want source CAS refusal", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "external" {
		t.Fatalf("external bytes were overwritten: %q", got)
	}
	if turns := rec.Turns(); len(turns) != 0 {
		t.Fatalf("unpublished write created a checkpoint: %+v", turns)
	}
}

func TestConcurrentSamePathWritesSerialize(t *testing.T) {
	r, root := newRegistry(t)
	path := filepath.Join(root, "target.txt")
	writeFile(t, path, "seed")
	run(t, r, "read", map[string]any{"path": "target.txt"})

	const writers = 24
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	successes := make(chan struct{}, writers)
	contents := make(map[string]bool, writers)
	for i := range writers {
		content := fmt.Sprintf("writer-%02d-%s", i, strings.Repeat("x", 1024+i))
		contents[content] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := tryRun(r, "write", map[string]any{"path": "target.txt", "content": content})
			if err != nil {
				errs <- err
				return
			}
			if res.IsError {
				if !strings.Contains(res.Content, "changed since it was read") {
					errs <- fmt.Errorf("unexpected refusal: %s", res.Content)
				}
				return
			}
			successes <- struct{}{}
		}()
	}
	wg.Wait()
	close(errs)
	close(successes)
	for err := range errs {
		t.Error(err)
	}
	if len(successes) == 0 {
		t.Fatal("every serialized writer was refused")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !contents[string(got)] {
		t.Fatalf("final file is torn or unexpected (%d bytes)", len(got))
	}
}

func TestConcurrentDelegateRegistriesSerializeAndFailStale(t *testing.T) {
	first, root := newRegistry(t)
	second, err := NewRegistry(root, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	recorder := newCheckpointRecorder(t, root)
	first.SetCheckpoints(recorder)
	second.SetCheckpoints(recorder)
	path := filepath.Join(root, "shared.txt")
	writeFile(t, path, "seed")
	run(t, first, "read", map[string]any{"path": "shared.txt"})
	run(t, second, "read", map[string]any{"path": "shared.txt"})
	recorder.Begin("parallel delegates")

	type outcome struct {
		content string
		res     Result
		err     error
	}
	outcomes := make(chan outcome, 2)
	for _, item := range []string{"delegate-one", "delegate-two"} {
		registry := first
		if item == "delegate-two" {
			registry = second
		}
		go func(registry *Registry, content string) {
			res, err := tryRun(registry, "write", map[string]any{"path": "shared.txt", "content": content})
			outcomes <- outcome{content: content, res: res, err: err}
		}(registry, item)
	}
	var winner string
	stale := 0
	for range 2 {
		got := <-outcomes
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.res.IsError {
			if !strings.Contains(got.res.Content, "changed since it was read") {
				t.Fatalf("unexpected delegate write refusal: %s", got.res.Content)
			}
			stale++
			continue
		}
		if winner != "" {
			t.Fatalf("both stale-source delegates wrote: %s and %s", winner, got.content)
		}
		winner = got.content
	}
	if winner == "" || stale != 1 {
		t.Fatalf("winner=%q stale=%d, want exactly one of each", winner, stale)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != winner {
		t.Fatalf("published file=%q err=%v, want whole winner %q", got, err, winner)
	}
	turns := recorder.Turns()
	if len(turns) != 1 || turns[0].Files != 1 || turns[0].Partial {
		t.Fatalf("shared checkpoint = %+v, want one exact pre-image", turns)
	}
}

func TestCaseOnlyAliasesShareMutationLeaseAcrossRegistries(t *testing.T) {
	first, root := newRegistry(t)
	second, err := NewRegistry(root, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	recorder := newCheckpointRecorder(t, root)
	first.SetCheckpoints(recorder)
	second.SetCheckpoints(recorder)

	const (
		firstName  = "CaseAlias.txt"
		secondName = "casealias.txt"
		seed       = "seed"
		winner     = "first registry"
		loser      = "second registry"
	)
	firstPath := filepath.Join(root, firstName)
	secondPath := filepath.Join(root, secondName)
	writeFile(t, firstPath, seed)
	firstInfo, err := os.Lstat(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Lstat(secondPath)
	if os.IsNotExist(err) {
		t.Skip("temporary volume is case-sensitive")
	}
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(firstInfo, secondInfo) {
		t.Fatalf("case-only alias resolved to a different file: %s and %s", firstPath, secondPath)
	}

	// Populate each registry's independent stale-check ledger using the
	// spelling its caller supplied. On a case-insensitive volume both reads
	// observe the same inode and source bytes.
	if res := run(t, first, "read", map[string]any{"path": firstName}); res.IsError {
		t.Fatalf("first read failed: %s", res.Content)
	}
	if res := run(t, second, "read", map[string]any{"path": secondName}); res.IsError {
		t.Fatalf("second read failed: %s", res.Content)
	}
	firstResolved, err := first.resolve(firstName)
	if err != nil {
		t.Fatal(err)
	}
	secondResolved, err := second.resolve(secondName)
	if err != nil {
		t.Fatal(err)
	}
	if mutationLockKey(firstResolved) != mutationLockKey(secondResolved) {
		t.Fatalf("case-only aliases received different mutation keys: %q and %q", firstResolved, secondResolved)
	}
	if got := first.display(firstResolved); got != firstName {
		t.Fatalf("first caller spelling was rewritten for display: got %q, want %q", got, firstName)
	}
	if got := second.display(secondResolved); got != secondName {
		t.Fatalf("second caller spelling was rewritten for display: got %q, want %q", got, secondName)
	}
	recorder.Begin("case aliases")

	firstAtPublish := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	defer release()
	firstDone := make(chan error, 1)
	go func() {
		tx, res, ok := first.prepareFileMutation(firstResolved, false)
		if !ok {
			firstDone <- fmt.Errorf("first prepare failed: %s", res.Content)
			return
		}
		defer tx.close()
		firstDone <- tx.publish(context.Background(), []byte(winner), tx.before.mode, func() {
			close(firstAtPublish)
			<-releaseFirst
		})
	}()
	select {
	case <-firstAtPublish:
	case err := <-firstDone:
		t.Fatalf("first publish did not reach its barrier: %v", err)
	}

	type outcome struct {
		res Result
		err error
	}
	secondStarted := make(chan struct{})
	secondDone := make(chan outcome, 1)
	go func() {
		close(secondStarted)
		res, runErr := tryRun(second, "write", map[string]any{"path": secondName, "content": loser})
		secondDone <- outcome{res: res, err: runErr}
	}()
	<-secondStarted

	// Refs are registered before a caller waits on the per-key mutex. Waiting
	// for two refs is a deterministic barrier: the second registry has reached
	// the same lease and cannot merely be an unscheduled goroutine.
	key := mutationLockKey(firstResolved)
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		pathLocks.Lock()
		lock := pathLocks.locks[key]
		refs := 0
		if lock != nil {
			refs = lock.refs
		}
		pathLocks.Unlock()
		if refs == 2 {
			break
		}
		select {
		case <-deadline.C:
			t.Fatalf("case-only alias did not join the active mutation lease (refs=%d)", refs)
		default:
			runtime.Gosched()
		}
	}
	select {
	case got := <-secondDone:
		t.Fatalf("second alias crossed the held publication barrier: %+v", got)
	default:
	}

	release()
	if err := <-firstDone; err != nil {
		t.Fatalf("first publish failed: %v", err)
	}
	secondResult := <-secondDone
	if secondResult.err != nil {
		t.Fatal(secondResult.err)
	}
	if !secondResult.res.IsError || !strings.Contains(secondResult.res.Content, secondName+" changed since it was read") {
		t.Fatalf("second alias result = %+v, want a serialized stale-source refusal", secondResult.res)
	}

	disk, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(disk) != winner {
		t.Fatalf("published file = %q, want the first complete post-image", disk)
	}
	snapshots := recorder.Snapshots()
	if len(snapshots) != 1 || len(snapshots[0].Files) != 1 || snapshots[0].Partial {
		t.Fatalf("checkpoint snapshots = %+v, want one exact successful mutation", snapshots)
	}
	mutation := snapshots[0].Files[0]
	if mutation.After.Digest != sha256.Sum256(disk) {
		t.Fatalf("checkpoint post-image digest does not match disk: checkpoint=%x disk=%x", mutation.After.Digest, sha256.Sum256(disk))
	}
	current, err := recorder.ReadSnapshotCurrent(mutation)
	if err != nil {
		t.Fatalf("checkpoint post-image did not revalidate: %v", err)
	}
	if !current.Existed || string(current.Content) != winner {
		t.Fatalf("checkpoint current state = %+v, want %q", current, winner)
	}
}
