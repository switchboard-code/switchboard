//go:build unix

package scm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/safeexec"
)

func TestRunGitCancellationKillsPipeHoldingDescendant(t *testing.T) {
	workspace := t.TempDir()
	bin := t.TempDir()
	childPIDPath := filepath.Join(workspace, "child.pid")
	marker := filepath.Join(workspace, "child-ran")
	script := `#!/bin/sh
(
  while :; do
    printf x >> "$SB_SCM_CHILD_MARKER"
    printf . >&2
    sleep 0.02
  done
) &
printf '%s\n' "$!" > "$SB_SCM_CHILD_PID"
wait
`
	gitPath := filepath.Join(bin, "git")
	if err := os.WriteFile(gitPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SB_SCM_CHILD_PID", childPIDPath)
	t.Setenv("SB_SCM_CHILD_MARKER", marker)
	git, err := safeexec.ResolveOutside("git", workspace)
	if err != nil {
		t.Fatal(err)
	}

	// If the process-group boundary regresses, keep the test suite recoverable:
	// the direct shell dies on context cancellation but its pipe-holding child
	// otherwise survives and keeps the old cmd.Run blocked indefinitely.
	cleanupChild := func() {
		raw, readErr := os.ReadFile(childPIDPath)
		if readErr != nil {
			return
		}
		pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
		if parseErr == nil && pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	t.Cleanup(cleanupChild)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan commandResult, 1)
	started := time.Now()
	go func() {
		done <- runGit(ctx, git, workspace, 128, "status", "--porcelain=v2")
	}()

	var result commandResult
	select {
	case result = <-done:
	case <-time.After(6 * time.Second):
		cleanupChild()
		t.Fatal("Git cancellation hung on a descendant that retained stderr")
	}
	if !errors.Is(result.err, context.DeadlineExceeded) {
		t.Fatalf("runGit error = %v, want context deadline", result.err)
	}
	if elapsed := time.Since(started); elapsed >= 5*time.Second {
		t.Fatalf("Git process group took %s to stop", elapsed)
	}
	before, err := os.Stat(marker)
	if err != nil || before.Size() == 0 {
		t.Fatalf("descendant fixture did not run: info=%v err=%v", before, err)
	}
	time.Sleep(150 * time.Millisecond)
	after, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("Git descendant survived cancellation: marker grew from %d to %d bytes", before.Size(), after.Size())
	}
}
