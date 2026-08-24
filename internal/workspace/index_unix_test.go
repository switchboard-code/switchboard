//go:build unix

package workspace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/switchboard-code/switchboard/internal/safeexec"
)

func TestGitNameStreamDeadlineKillsStdoutHoldingDescendant(t *testing.T) {
	root := t.TempDir()
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(root, "descendant.pid")
	index := NewIndex(w, 10)
	var wrapper *exec.Cmd
	index.gitCommand = func(_ context.Context, _ ...string) *exec.Cmd {
		// The wrapper exits after the descendant reports its pid. The descendant
		// retains stdout, which made the old ReadSlice wait past its deadline.
		script := `sh -c 'echo $$ > "$SB_WORKSPACE_DESCENDANT_PID"; exec sleep 30' & child=$!; while [ ! -s "$SB_WORKSPACE_DESCENDANT_PID" ]; do sleep 0.01; done; exit 0`
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append(os.Environ(), "SB_WORKSPACE_DESCENDANT_PID="+pidFile)
		wrapper = cmd
		return cmd
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _, err = index.streamGitNames(ctx, safeexec.Executable{}, nil, &gitListBudget{}, func([]byte) {})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stream error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("deadline return took %s; stdout-holding descendant was not stopped", elapsed)
	}
	if wrapper == nil || wrapper.ProcessState == nil {
		t.Fatal("Git wrapper was not reaped before streamGitNames returned")
	}

	rawPID, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("descendant did not report a pid: %v", readErr)
	}
	pid, parseErr := strconv.Atoi(string(bytes.TrimSpace(rawPID)))
	if parseErr != nil || pid <= 0 {
		t.Fatalf("descendant pid %q: %v", rawPID, parseErr)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := unix.Kill(pid, 0); errors.Is(err, unix.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("stdout-holding descendant %d survived cancellation", pid)
}

func TestGitIndexNeverExecutesRepositoryFSMonitorHook(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(git, append([]string{"-C", root}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	run("init", "-q")
	writeIndexFile(t, root, "tracked.go", "package tracked")
	run("add", "tracked.go")
	marker := filepath.Join(root, "fsmonitor-executed")
	t.Setenv("SB_FS_MONITOR_MARKER", marker)
	hook := filepath.Join(root, "fsmonitor-hook")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n: > \"$SB_FS_MONITOR_MARKER\"\nprintf '\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	run("config", "core.fsmonitor", hook)

	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	index := NewIndex(w, 100)
	if _, _, _, err := index.listGit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatalf("repository fsmonitor hook executed during inventory: %v", err)
	}
}

func TestGitIndexNeverExecutesRepositorySymlinkPATHShadow(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	cmd := exec.Command(git, "-C", root, "init", "-q")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
	writeIndexFile(t, root, "tracked.go", "package tracked")
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "shadow-ran")
	external := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(external, []byte("#!/bin/sh\n/usr/bin/touch '"+marker+"'\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(bin, "git")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	index := NewIndex(w, 100)
	snapshot, err := index.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("repository PATH shadow executed during inventory: %v", err)
	}
	if len(snapshot.Files) == 0 || snapshot.Files[0].Path != "tracked.go" {
		t.Fatalf("safe rooted fallback snapshot = %+v", snapshot)
	}
}

func TestGitIndexNestedMarkerNeverExecutesOuterRepositoryPATHShadow(t *testing.T) {
	repository := t.TempDir()
	if err := os.Mkdir(filepath.Join(repository, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(repository, "nested")
	if err := os.MkdirAll(filepath.Join(nested, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	workspacePath := filepath.Join(nested, "workspace")
	bin := filepath.Join(repository, "bin")
	for _, dir := range []string{workspacePath, bin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeIndexFile(t, workspacePath, "tracked.go", "package tracked")
	marker := filepath.Join(repository, "outer-shadow-ran")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\n/usr/bin/touch '"+marker+"'\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	root, err := Open(workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := NewIndex(root, 100).Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("outer-repository PATH shadow executed during inventory: %v", statErr)
	}
	if len(snapshot.Files) != 1 || snapshot.Files[0].Path != "tracked.go" {
		t.Fatalf("safe rooted fallback snapshot = %+v", snapshot)
	}
}

func TestGitIndexNeverExecutesLaunchWorkspacePATHShadowForDifferentTarget(t *testing.T) {
	launchWorkspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(launchWorkspace, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	launchBin := filepath.Join(launchWorkspace, "bin")
	if err := os.Mkdir(launchBin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(launchWorkspace, "launch-shadow-ran")
	if err := os.WriteFile(filepath.Join(launchBin, "git"), []byte("#!/bin/sh\nprintf executed > \"$SB_WORKSPACE_LAUNCH_GIT_MARKER\"\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SB_WORKSPACE_LAUNCH_GIT_MARKER", marker)
	t.Setenv("PATH", launchBin)
	t.Chdir(launchWorkspace)

	targetWorkspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(targetWorkspace, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeIndexFile(t, targetWorkspace, "tracked.go", "package tracked")
	root, err := Open(targetWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := NewIndex(root, 100).Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("launch-workspace PATH shadow executed while indexing a different target: %v", statErr)
	}
	if len(snapshot.Files) != 1 || snapshot.Files[0].Path != "tracked.go" {
		t.Fatalf("safe rooted fallback snapshot = %+v", snapshot)
	}
}

func TestGitIndexEnvShebangCannotDispatchWorkspaceInterpreter(t *testing.T) {
	root := t.TempDir()
	workspaceBin := filepath.Join(root, "bin")
	externalBin := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-external-bin")
	if err := os.Mkdir(workspaceBin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(externalBin, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(externalBin) })
	writeIndexFile(t, root, "tracked.go", "package tracked")
	workspaceMarker := filepath.Join(root, "workspace-interpreter-ran")
	trustedMarker := filepath.Join(root, "trusted-interpreter-ran")
	interpreter := "switchboard-index-shell"
	if err := os.WriteFile(filepath.Join(workspaceBin, interpreter), []byte("#!/bin/sh\n/usr/bin/touch '"+workspaceMarker+"'\nexit 90\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	trusted := "#!/bin/sh\n/usr/bin/touch '" + trustedMarker + "'\nexec /bin/sh \"$@\"\n"
	if err := os.WriteFile(filepath.Join(externalBin, interpreter), []byte(trusted), 0o700); err != nil {
		t.Fatal(err)
	}
	gitScript := "#!/usr/bin/env " + interpreter + "\ncase \" $* \" in\n  *\" --cached \"*) printf 'tracked.go\\000' ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(externalBin, "git"), []byte(gitScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", workspaceBin+string(os.PathListSeparator)+externalBin)

	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	index := NewIndex(w, 100)
	files, truncated, _, err := index.listGit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(files) != 1 || files[0].Path != "tracked.go" {
		t.Fatalf("Git inventory = %+v, truncated=%v", files, truncated)
	}
	if _, err := os.Stat(trustedMarker); err != nil {
		t.Fatalf("trusted external interpreter did not run: %v", err)
	}
	if _, err := os.Stat(workspaceMarker); !os.IsNotExist(err) {
		t.Fatalf("workspace interpreter executed through Git env shebang: %v", err)
	}
}

func TestWalkIndexDoesNotBlockOnWorkspaceRootFIFOReplacement(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeIndexFile(t, root, "inside.go", "package inside")
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, root+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(root, 0o600); err != nil {
		t.Fatal(err)
	}
	index := NewIndex(w, 10)
	done := make(chan error, 1)
	go func() {
		_, _, _, err := index.listWalk(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("workspace index accepted a FIFO replacement root")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("workspace index blocked on a FIFO replacement root")
	}
}
