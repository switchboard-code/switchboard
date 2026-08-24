// Package scm provides a bounded, read-only view of a Git worktree.
//
// It never intentionally updates the index, refs, or worktree. Repository
// configuration is still consulted for ordinary Git semantics. External diff
// drivers, textconv filters, filesystem monitors, and lazy object fetches are
// disabled, but Git status can still invoke clean/process filters and hooks;
// every process therefore requires explicit repository execution authority.
package scm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/safeexec"
)

const (
	maxStatusBytes      = 8 << 20
	maxStatusEntries    = 4096
	maxDiagnosticBytes  = 64 << 10
	DefaultMaxDiffBytes = 4 << 20
	MaxDiffBytes        = 32 << 20
	maxPathspecs        = 4096
)

var (
	ErrNotRepository       = errors.New("not a Git worktree")
	ErrOutputLimit         = errors.New("Git output exceeded the limit")
	ErrOutsideRepo         = errors.New("path is outside the repository")
	ErrTooManyPaths        = errors.New("too many Git pathspecs")
	ErrMalformedStatus     = errors.New("malformed Git porcelain-v2 status")
	ErrExecutionNotTrusted = errors.New("Git execution requires a trusted workspace")
)

// ExecutionAuthority is the standing user decision that repository-controlled
// Git filters and hooks may execute. Discover checks it before the first Git
// process, and Repository checks it again before every later process.
type ExecutionAuthority interface {
	Trusted(workspace string) bool
}

// Repository is one discovered Git worktree. Root is absolute and symlink
// resolved when the platform permits it.
type Repository struct {
	Root      string
	workspace string
	authority ExecutionAuthority
	git       safeexec.Executable
}

// GitError is a failed Git invocation. Stderr is bounded, but retained because
// Git's diagnostic is usually the only useful explanation of a repository
// failure.
type GitError struct {
	Operation string
	ExitCode  int
	Stderr    string
}

func (e *GitError) Error() string {
	message := strings.TrimSpace(e.Stderr)
	if message == "" {
		message = "git exited " + strconv.Itoa(e.ExitCode)
	}
	if e.Operation == "" {
		return message
	}
	return e.Operation + ": " + message
}

// Discover finds the nearest containing Git worktree for workspace. Bare
// repositories and ordinary directories return ErrNotRepository.
func Discover(ctx context.Context, workspace string, authority ExecutionAuthority) (*Repository, error) {
	if authority == nil || !authority.Trusted(workspace) {
		return nil, ErrExecutionNotTrusted
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolving workspace: %w", err)
	}
	if info, statErr := os.Stat(abs); statErr != nil {
		return nil, fmt.Errorf("opening workspace: %w", statErr)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("opening workspace: %s is not a directory", abs)
	}
	roots, err := safeexec.WorkspaceAndCurrentAuthorityRoots(abs)
	if err != nil {
		return nil, fmt.Errorf("resolving Git workspace authority: %w", err)
	}
	git, err := safeexec.ResolveOutside("git", roots...)
	if err != nil {
		return nil, fmt.Errorf("finding trusted git executable: %w", err)
	}

	inside := runGit(ctx, git, abs, maxDiagnosticBytes, "rev-parse", "--is-inside-work-tree")
	if inside.err != nil {
		gitErr := inside.commandError("discovering Git worktree")
		return nil, fmt.Errorf("%w: %w", ErrNotRepository, gitErr)
	}
	if inside.stdoutTruncated {
		return nil, fmt.Errorf("%w: discovering Git worktree", ErrOutputLimit)
	}
	if !bytes.Equal(bytes.TrimSuffix(inside.stdout, []byte{'\n'}), []byte("true")) {
		return nil, fmt.Errorf("%w: git did not report an ordinary worktree", ErrNotRepository)
	}
	result := runGit(ctx, git, abs, maxDiagnosticBytes, "rev-parse", "--show-toplevel")
	if result.err != nil {
		return nil, result.commandError("resolving Git worktree root")
	}
	if result.stdoutTruncated {
		return nil, fmt.Errorf("%w: resolving Git worktree root", ErrOutputLimit)
	}
	root := string(bytes.TrimSuffix(result.stdout, []byte{'\n'}))
	if root == "" {
		return nil, fmt.Errorf("%w: git returned an empty worktree root", ErrNotRepository)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
		root = resolved
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolving Git worktree root: %w", err)
	}
	// The parent marker scan above protects the commands needed to discover
	// Root; now bind the result explicitly outside the root Git reported too.
	finalRoots := append(append([]string(nil), roots...), root)
	git, err = safeexec.ResolvePathOutside(git.Path(), finalRoots...)
	if err != nil {
		return nil, fmt.Errorf("binding git outside repository authority: %w", err)
	}
	return &Repository{
		Root: filepath.Clean(root), workspace: abs,
		authority: authority, git: git,
	}, nil
}

type commandResult struct {
	stdout, stderr                   []byte
	stdoutTruncated, stderrTruncated bool
	err                              error
}

func (r commandResult) exitCode() int {
	var exitErr *exec.ExitError
	if errors.As(r.err, &exitErr) {
		return exitErr.ExitCode()
	}
	if r.err != nil {
		return -1
	}
	return 0
}

func (r commandResult) commandError(operation string) error {
	if r.err == nil {
		return nil
	}
	if errors.Is(r.err, context.Canceled) || errors.Is(r.err, context.DeadlineExceeded) {
		return r.err
	}
	if errors.Is(r.err, safeexec.ErrChanged) || errors.Is(r.err, safeexec.ErrUntrustedPath) {
		return fmt.Errorf("%s: %w", operation, r.err)
	}
	stderr := string(r.stderr)
	if r.stderrTruncated {
		stderr += "\n… stderr truncated …"
	}
	return &GitError{Operation: operation, ExitCode: r.exitCode(), Stderr: stderr}
}

func runGit(ctx context.Context, git safeexec.Executable, dir string, stdoutLimit int, args ...string) commandResult {
	common := []string{
		"-c", "color.ui=false",
		"-c", "core.quotepath=false",
		"-c", "core.fsmonitor=false",
	}
	// RunProcess owns cancellation for the entire process group. Git status may
	// invoke repository filters; CommandContext would kill only Git and can then
	// wait forever on a descendant that retained a stdout or stderr pipe.
	cmd, err := git.Command(append(common, args...)...)
	if err != nil {
		return commandResult{err: err}
	}
	cmd.Dir = dir
	roots, err := safeexec.WorkspaceAndCurrentAuthorityRoots(dir)
	if err != nil {
		return commandResult{err: fmt.Errorf("resolving Git workspace authority: %w", err)}
	}
	environ, err := safeexec.FilterEnvironmentPath(execution.ScrubbedChildEnv(), roots...)
	if err != nil {
		return commandResult{err: fmt.Errorf("preparing trusted Git interpreter path: %w", err)}
	}
	cmd.Env = stableGitEnv(environ)
	out := &cappedBuffer{max: stdoutLimit}
	errout := &cappedBuffer{max: maxDiagnosticBytes}
	cmd.Stdout, cmd.Stderr = out, errout
	err = execution.RunProcess(ctx, cmd)
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
	}
	return commandResult{
		stdout: out.Bytes(), stderr: errout.Bytes(), err: err,
		stdoutTruncated: out.truncated, stderrTruncated: errout.truncated,
	}
}

type cappedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.max - b.buf.Len()
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		_, _ = b.buf.Write(p[:remaining])
	}
	if remaining < len(p) {
		b.truncated = true
	}
	return n, nil
}

func (b *cappedBuffer) Bytes() []byte { return append([]byte(nil), b.buf.Bytes()...) }

func stableGitEnv(environ []string) []string {
	overrides := []string{
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_PAGER=cat",
		"PAGER=cat",
		"LC_ALL=C",
		"LANG=C",
		"GIT_LITERAL_PATHSPECS=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_EXTERNAL_DIFF=",
		"GIT_DIFF_OPTS=",
	}
	set := make(map[string]string, len(overrides))
	for _, entry := range overrides {
		key, value, _ := strings.Cut(entry, "=")
		set[key] = value
	}
	remove := map[string]bool{
		"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true,
		"GIT_OBJECT_DIRECTORY": true, "GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
		"GIT_COMMON_DIR": true, "GIT_NAMESPACE": true, "GIT_PREFIX": true,
		"GIT_EXEC_PATH": true, "GIT_CEILING_DIRECTORIES": true,
		"GIT_DISCOVERY_ACROSS_FILESYSTEM": true, "GIT_CONFIG": true,
	}
	out := make([]string, 0, len(environ)+len(set))
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || envKeyIn(key, remove) || strings.HasPrefix(strings.ToUpper(key), "GIT_CONFIG_") {
			continue
		}
		if _, replace := envValue(key, set); replace {
			continue
		}
		out = append(out, entry)
	}
	out = append(out, overrides...)
	return out
}

func (r *Repository) executionAllowed() error {
	if r == nil || r.authority == nil || !r.authority.Trusted(r.workspace) {
		return ErrExecutionNotTrusted
	}
	return nil
}

func envKeyIn(key string, values map[string]bool) bool {
	if runtime.GOOS != "windows" {
		return values[key]
	}
	for candidate := range values {
		if strings.EqualFold(candidate, key) {
			return true
		}
	}
	return false
}

func envValue(key string, values map[string]string) (string, bool) {
	if runtime.GOOS != "windows" {
		value, ok := values[key]
		return value, ok
	}
	for candidate, value := range values {
		if strings.EqualFold(candidate, key) {
			return value, true
		}
	}
	return "", false
}

func (r *Repository) pathspecs(paths []string) ([]string, error) {
	if len(paths) > maxPathspecs {
		return nil, fmt.Errorf("%w: got %d, maximum %d", ErrTooManyPaths, len(paths), maxPathspecs)
	}
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			return nil, errors.New("Git path is empty")
		}
		if strings.IndexByte(path, 0) >= 0 {
			return nil, errors.New("Git path contains NUL")
		}
		var abs string
		if filepath.IsAbs(path) {
			abs = filepath.Clean(path)
		} else {
			abs = filepath.Join(r.Root, filepath.Clean(path))
		}
		rel, err := filepath.Rel(r.Root, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return nil, fmt.Errorf("%w: %s", ErrOutsideRepo, path)
		}
		rel = filepath.ToSlash(rel)
		if !seen[rel] {
			seen[rel] = true
			out = append(out, rel)
		}
	}
	return out, nil
}
