package scm

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/safeexec"
)

type testExecutionAuthority bool

func (a testExecutionAuthority) Trusted(string) bool { return bool(a) }

type mutableTestExecutionAuthority struct{ trusted bool }

func (a *mutableTestExecutionAuthority) Trusted(string) bool { return a != nil && a.trusted }

func TestDiscover(t *testing.T) {
	root := initTestRepo(t)
	nested := filepath.Join(root, "one", "two")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	repo, err := Discover(context.Background(), nested, testExecutionAuthority(true))
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Root != want {
		t.Fatalf("Root = %q, want %q", repo.Root, want)
	}
}

func TestDiscoverRootContainingNewline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Win32 paths cannot contain newlines")
	}
	root := filepath.Join(t.TempDir(), "repo\nroot")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	gitTest(t, root, "init", "--quiet")

	repo, err := Discover(context.Background(), root, testExecutionAuthority(true))
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Root != want {
		t.Fatalf("Root = %q, want %q", repo.Root, want)
	}
}

func TestDiscoverUnbornRepository(t *testing.T) {
	root := initTestRepo(t)
	repo, err := Discover(context.Background(), root, testExecutionAuthority(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".git", "HEAD")); err != nil {
		t.Fatal(err)
	}
	if repo.Root == "" {
		t.Fatal("Discover returned an empty root")
	}
}

func TestDiscoverNonRepositoryRetainsGitDiagnostic(t *testing.T) {
	_, err := Discover(context.Background(), t.TempDir(), testExecutionAuthority(true))
	if !errors.Is(err, ErrNotRepository) {
		t.Fatalf("error = %v, want ErrNotRepository", err)
	}
	var gitErr *GitError
	if !errors.As(err, &gitErr) {
		t.Fatalf("error = %T %v, want *GitError in chain", err, err)
	}
	if !strings.Contains(strings.ToLower(gitErr.Stderr), "not a git repository") {
		t.Fatalf("stderr = %q, want Git's non-repository diagnostic", gitErr.Stderr)
	}
}

func TestDiscoverRejectsBareRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	gitTest(t, root, "init", "--bare", "--quiet")
	if _, err := Discover(context.Background(), root, testExecutionAuthority(true)); !errors.Is(err, ErrNotRepository) {
		t.Fatalf("error = %v, want ErrNotRepository", err)
	}
}

func TestStableGitEnv(t *testing.T) {
	env := stableGitEnv([]string{
		"PATH=/test/bin",
		"LC_ALL=host-locale",
		"GIT_OPTIONAL_LOCKS=1",
		"GIT_LITERAL_PATHSPECS=0",
		"GIT_DIR=/elsewhere",
		"GIT_WORK_TREE=/elsewhere",
		"GIT_EXEC_PATH=/workspace/bin",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.hooksPath",
		"GIT_CONFIG_VALUE_0=/workspace/hooks",
	})
	values := make(map[string][]string)
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[strings.ToUpper(key)] = append(values[strings.ToUpper(key)], value)
		}
	}
	for key, want := range map[string]string{
		"GIT_OPTIONAL_LOCKS":    "0",
		"GIT_PAGER":             "cat",
		"LC_ALL":                "C",
		"GIT_LITERAL_PATHSPECS": "1",
		"GIT_NO_LAZY_FETCH":     "1",
	} {
		got := values[key]
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s = %q, want exactly %q", key, got, want)
		}
	}
	for _, key := range []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_EXEC_PATH", "GIT_CONFIG_COUNT",
		"GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0",
	} {
		if got := values[key]; len(got) != 0 {
			t.Errorf("%s leaked into child environment: %q", key, got)
		}
	}
}

func TestGitEnvShebangCannotDispatchWorkspaceInterpreter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("env-shebang fixture is Unix-only")
	}
	root := t.TempDir()
	workspaceBin := filepath.Join(root, "workspace-bin")
	externalBin := filepath.Join(root, "external-bin")
	if err := os.Mkdir(workspaceBin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(externalBin, 0o700); err != nil {
		t.Fatal(err)
	}
	workspaceMarker := filepath.Join(root, "workspace-interpreter-ran")
	trustedMarker := filepath.Join(root, "trusted-interpreter-ran")
	interpreter := "switchboard-test-shell"
	if err := os.WriteFile(filepath.Join(workspaceBin, interpreter), []byte("#!/bin/sh\n/usr/bin/touch '"+workspaceMarker+"'\nexit 90\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	trusted := "#!/bin/sh\n/usr/bin/touch '" + trustedMarker + "'\nexec /bin/sh \"$@\"\n"
	if err := os.WriteFile(filepath.Join(externalBin, interpreter), []byte(trusted), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(externalBin, "git"), []byte("#!/usr/bin/env "+interpreter+"\nprintf 'true\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", workspaceBin+string(os.PathListSeparator)+externalBin)
	git, err := safeexec.ResolveOutside("git", workspaceBin)
	if err != nil {
		t.Fatal(err)
	}
	result := runGit(context.Background(), git, workspaceBin, 128, "rev-parse", "--is-inside-work-tree")
	if result.err != nil {
		t.Fatal(result.commandError("test Git"))
	}
	if got := strings.TrimSpace(string(result.stdout)); got != "true" {
		t.Fatalf("Git output = %q, want true", got)
	}
	if _, err := os.Stat(trustedMarker); err != nil {
		t.Fatalf("trusted external interpreter did not run: %v", err)
	}
	if _, err := os.Stat(workspaceMarker); !os.IsNotExist(err) {
		t.Fatalf("workspace interpreter executed through Git env shebang: %v", err)
	}
}

func TestDiscoverRequiresExecutionAuthorityBeforeGitResolution(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("PATH", workspace)
	_, err := Discover(context.Background(), workspace, testExecutionAuthority(false))
	if !errors.Is(err, ErrExecutionNotTrusted) {
		t.Fatalf("error = %v, want ErrExecutionNotTrusted", err)
	}
}

func TestRepositoryRechecksRevokedExecutionAuthority(t *testing.T) {
	root := initTestRepo(t)
	authority := &mutableTestExecutionAuthority{trusted: true}
	repo, err := Discover(context.Background(), root, authority)
	if err != nil {
		t.Fatal(err)
	}
	authority.trusted = false
	if _, err := repo.Status(context.Background()); !errors.Is(err, ErrExecutionNotTrusted) {
		t.Fatalf("status error = %v, want ErrExecutionNotTrusted", err)
	}
	if _, err := repo.DiffHEAD(context.Background(), DiffOptions{}); !errors.Is(err, ErrExecutionNotTrusted) {
		t.Fatalf("diff error = %v, want ErrExecutionNotTrusted", err)
	}
}

func TestPathspecValidation(t *testing.T) {
	repo := openTestRepo(t, initTestRepo(t))
	outside := filepath.Join(filepath.Dir(repo.Root), "outside")
	if _, err := repo.StatusPaths(context.Background(), []string{outside}); !errors.Is(err, ErrOutsideRepo) {
		t.Fatalf("outside path error = %v, want ErrOutsideRepo", err)
	}
	if _, err := repo.StatusPaths(context.Background(), []string{"bad\x00path"}); err == nil {
		t.Fatal("NUL path was accepted")
	}
	paths := make([]string, maxPathspecs+1)
	for i := range paths {
		paths[i] = "x"
	}
	if _, err := repo.StatusPaths(context.Background(), paths); !errors.Is(err, ErrTooManyPaths) {
		t.Fatalf("path count error = %v, want ErrTooManyPaths", err)
	}
}

func initTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	gitTest(t, root, "init", "--quiet")
	gitTest(t, root, "symbolic-ref", "HEAD", "refs/heads/main")
	gitTest(t, root, "config", "user.name", "Switchboard Test")
	gitTest(t, root, "config", "user.email", "switchboard@example.invalid")
	return root
}

func openTestRepo(t *testing.T, root string) *Repository {
	t.Helper()
	repo, err := Discover(context.Background(), root, testExecutionAuthority(true))
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func gitTest(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func gitTestFailure(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("git %s unexpectedly succeeded\n%s", strings.Join(args, " "), out)
	}
	return string(out)
}

func writeTestFile(t *testing.T, root, name string, content []byte) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitTestFiles(t *testing.T, root, message string) {
	t.Helper()
	gitTest(t, root, "add", "--all")
	gitTest(t, root, "commit", "--quiet", "-m", message)
}

func indexChecksum(t *testing.T, root string) [sha256.Size]byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

func requireNoIndexLock(t *testing.T, root string) {
	t.Helper()
	_, err := os.Lstat(filepath.Join(root, ".git", "index.lock"))
	if err == nil {
		t.Fatal("read-only SCM operation left .git/index.lock behind")
	}
	if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func executableScript(t *testing.T, root, name, marker string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script diff driver fixture is Unix-only")
	}
	path := filepath.Join(root, name)
	body := "#!/bin/sh\nprintf invoked > " + shellQuote(marker) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
