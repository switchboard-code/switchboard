//go:build unix

package bisect

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
)

func TestRunnerRejectsEveryOutsideWorkspacePathBeforeMutation(t *testing.T) {
	workspace := canonicalTempDir(t)
	inside := filepath.Join(workspace, "inside.txt")
	if err := os.WriteFile(inside, []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideDir := canonicalTempDir(t)
	outside := filepath.Join(outsideDir, "outside.txt")
	if err := os.WriteFile(outside, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	verified := false
	runner := configureRunner(t, workspace, &Runner{
		States: []map[string]checkpoint.FileState{{outside: state("overwritten")}},
		Verify: func(context.Context) Verdict {
			verified = true
			return Verdict{Passed: true}
		},
	})

	_, err := runner.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("outside path error = %v", err)
	}
	if verified {
		t.Fatal("verifier ran after an outside path was refused")
	}
	if got, _ := os.ReadFile(inside); string(got) != "inside" {
		t.Fatalf("inside file changed: %q", got)
	}
	if got, _ := os.ReadFile(outside); string(got) != "sentinel" {
		t.Fatalf("outside file changed: %q", got)
	}
	if _, statErr := os.Lstat(filepath.Join(runner.JournalDir, bisectJournalDirectory)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("invalid plan created cleanup state: %v", statErr)
	}
}

func TestRunnerRejectsOutsideSymlinkWithoutReadingOrHanging(t *testing.T) {
	workspace := canonicalTempDir(t)
	outsideDir := canonicalTempDir(t)
	outside := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outside, []byte("do-not-read-or-change"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(workspace, "linked.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	runner := configureRunner(t, workspace, &Runner{
		States: []map[string]checkpoint.FileState{{link: state("replacement")}},
		Verify: func(context.Context) Verdict { return Verdict{Passed: true} },
	})

	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("symlink error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bisect blocked while refusing an outside symlink")
	}
	if got, _ := os.ReadFile(outside); string(got) != "do-not-read-or-change" {
		t.Fatalf("outside symlink target changed: %q", got)
	}
}

func TestRunnerParentRenameCannotRedirectRestoreOutsideWorkspace(t *testing.T) {
	base := canonicalTempDir(t)
	workspace := filepath.Join(base, "workspace")
	outside := filepath.Join(base, "outside")
	moved := filepath.Join(base, "moved-parent")
	for _, dir := range []string{workspace, outside} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	parent := filepath.Join(workspace, "src")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parent, "state.txt")
	external := filepath.Join(outside, "state.txt")
	if err := os.WriteFile(target, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(external, []byte("external-sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := configureRunner(t, workspace, &Runner{
		States: []map[string]checkpoint.FileState{{target: state("fine")}},
		Verify: func(context.Context) Verdict { return Verdict{FirstFail: "broken"} },
	})
	var hookErr error
	runner.beforeRestore = func(path string) {
		if path != target || hookErr != nil {
			return
		}
		if err := os.Rename(parent, moved); err != nil {
			hookErr = err
			return
		}
		hookErr = os.Symlink(outside, parent)
	}

	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("parent substitution was accepted")
		}
		if hookErr != nil {
			t.Fatal(hookErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bisect blocked after a parent rename and symlink substitution")
	}
	if got, _ := os.ReadFile(external); string(got) != "external-sentinel" {
		t.Fatalf("external target changed: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(moved, "state.txt")); string(got) != "broken" {
		t.Fatalf("retained original parent was mutated: %q", got)
	}
}

func TestRunnerFinalRestoreDeletesAFileRecreatedOnlyForProbe(t *testing.T) {
	workspace := canonicalTempDir(t)
	status := filepath.Join(workspace, "status.txt")
	deleted := filepath.Join(workspace, "deleted.txt")
	if err := os.WriteFile(status, []byte("broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	probes := 0
	runner := configureRunner(t, workspace, &Runner{
		States: []map[string]checkpoint.FileState{
			{status: state("fine-0"), deleted: state("historical")},
			{status: state("fine-1"), deleted: state("historical")},
			{status: state("fine-2"), deleted: state("historical")},
		},
		Verify: func(context.Context) Verdict {
			probes++
			if probes == 2 {
				if got, err := os.ReadFile(deleted); err != nil || string(got) != "historical" {
					return Verdict{Err: errors.Join(err, errors.New("historical deletion state was not recreated"))}
				}
				cancel()
				return Verdict{Passed: true}
			}
			return Verdict{FirstFail: "broken"}
		},
	})

	_, err := runner.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	if _, statErr := os.Lstat(deleted); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("final restore did not delete probe-only file: %v", statErr)
	}
	if got, _ := os.ReadFile(status); string(got) != "broken" {
		t.Fatalf("current status was not restored: %q", got)
	}
}

func TestRunnerCrossProcessLockSerializesWorkspaceReconstruction(t *testing.T) {
	workspace := canonicalTempDir(t)
	target := filepath.Join(workspace, "state.txt")
	if err := os.WriteFile(target, []byte("current"), 0o644); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	first := configureRunner(t, workspace, &Runner{
		States: []map[string]checkpoint.FileState{{target: state("past")}},
		Verify: func(context.Context) Verdict {
			close(entered)
			<-release
			return Verdict{Passed: true}
		},
	})
	firstDone := make(chan error, 1)
	go func() {
		_, err := first.Run(context.Background())
		firstDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first bisect did not acquire the workspace")
	}
	second := &Runner{
		Workspace: workspace, JournalDir: first.JournalDir,
		States: []map[string]checkpoint.FileState{{target: state("other")}},
		Verify: func(context.Context) Verdict { return Verdict{Passed: true} },
	}
	if _, err := second.Run(context.Background()); !errors.Is(err, ErrLocked) {
		t.Fatalf("second bisect error = %v, want ErrLocked", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first bisect: %v", err)
	}
}

func TestRunnerRejectsOversizeHistoricalStateBeforeVerifier(t *testing.T) {
	workspace := canonicalTempDir(t)
	target := filepath.Join(workspace, "state.txt")
	if err := os.WriteFile(target, []byte("current"), 0o644); err != nil {
		t.Fatal(err)
	}
	verified := false
	runner := configureRunner(t, workspace, &Runner{
		States: []map[string]checkpoint.FileState{{target: {
			Existed: true, Mode: 0o644, Content: make([]byte, maxBisectFileBytes+1),
		}}},
		Verify: func(context.Context) Verdict {
			verified = true
			return Verdict{Passed: true}
		},
	})
	if _, err := runner.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "checkpoint limit") {
		t.Fatalf("oversize historical state error = %v", err)
	}
	if verified {
		t.Fatal("verifier ran with an oversize historical state")
	}
	if got, _ := os.ReadFile(target); string(got) != "current" {
		t.Fatalf("oversize plan changed current file: %q", got)
	}
}
