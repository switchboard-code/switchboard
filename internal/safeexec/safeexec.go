// Package safeexec resolves fixed helper programs without granting an
// untrusted workspace control over PATH. It is not for explicit user choices
// such as $EDITOR or a configured shell.
package safeexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var (
	ErrUntrustedPath = errors.New("executable resolves beneath untrusted authority")
	ErrChanged       = errors.New("executable changed after it was resolved")
)

// Executable is one canonical, identity-bound executable path. The zero value
// is invalid. Command revalidates the path immediately before constructing a
// child; the operating system's final path lookup remains the unavoidable
// portable exec boundary.
type Executable struct {
	path string
	info os.FileInfo
}

// WorkspaceAuthorityRoots canonicalizes workspace and returns it together
// with every enclosing VCS root. The nearest marker is not an ownership
// boundary: a checkout can contain a nested marker that would otherwise hide
// a containing repository and its sibling PATH entries. Marker inspection
// fails closed because treating an unreadable ancestor as absent would grant
// it executable authority.
func WorkspaceAuthorityRoots(workspace string) ([]string, error) {
	if strings.TrimSpace(workspace) == "" {
		return nil, errors.New("workspace path is required")
	}
	canonical, err := canonicalPath(workspace)
	if err != nil {
		return nil, fmt.Errorf("canonicalizing workspace authority: %w", err)
	}
	return workspaceAuthorityRootsCanonical(canonical, true)
}

func workspaceAuthorityRootsCanonical(canonical string, includePath bool) ([]string, error) {
	var roots []string
	if includePath {
		roots = append(roots, canonical)
	}
	for dir := canonical; ; dir = filepath.Dir(dir) {
		marked := false
		for _, marker := range []string{".git", ".hg", ".svn"} {
			if _, err := os.Lstat(filepath.Join(dir, marker)); err == nil {
				marked = true
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("inspecting workspace authority marker: %w", err)
			}
		}
		if marked && (!includePath || dir != canonical) {
			roots = append(roots, dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return roots, nil
		}
	}
}

// CurrentWorkspaceAuthorityRoots treats an ordinary launch directory as
// untrusted while preserving normal launches from the user's home directory
// or a filesystem root. Marked VCS roots remain untrusted even at those
// locations; ~/.switchboard alone is user configuration, not a checkout.
func CurrentWorkspaceAuthorityRoots() ([]string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolving launch workspace authority: %w", err)
	}
	canonical, err := canonicalPath(cwd)
	if err != nil {
		return nil, fmt.Errorf("canonicalizing launch workspace authority: %w", err)
	}
	includePath := filepath.Dir(canonical) != canonical
	if home, homeErr := os.UserHomeDir(); homeErr == nil && strings.TrimSpace(home) != "" {
		if canonicalHome, homeCanonicalErr := canonicalPath(home); homeCanonicalErr == nil && samePath(canonical, canonicalHome) {
			includePath = false
		}
	}
	return workspaceAuthorityRootsCanonical(canonical, includePath)
}

// WorkspaceAndCurrentAuthorityRoots returns the union of each target
// workspace's authority and the process launch directory's authority. A
// target selected with -workspace does not make an absolute PATH entry under
// the unrelated launch checkout trustworthy. Results preserve target order
// followed by current-directory order and are deduplicated canonically.
func WorkspaceAndCurrentAuthorityRoots(workspaces ...string) ([]string, error) {
	seen := make(map[string]struct{})
	roots := make([]string, 0, (len(workspaces)+1)*2)
	merge := func(collected []string) {
		for _, root := range collected {
			key := pathKey(root)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			roots = append(roots, root)
		}
	}
	for _, path := range workspaces {
		collected, err := WorkspaceAuthorityRoots(path)
		if err != nil {
			return nil, err
		}
		merge(collected)
	}
	current, err := CurrentWorkspaceAuthorityRoots()
	if err != nil {
		return nil, err
	}
	merge(current)
	return roots, nil
}

func pathKey(path string) string {
	key := filepath.Clean(path)
	if filepath.Separator == '\\' {
		key = strings.ToLower(key)
	}
	return key
}

func samePath(a, b string) bool { return pathKey(a) == pathKey(b) }

// ResolveOutside looks up name once, resolves symlinks, rejects any result
// beneath the supplied untrusted roots, and binds the final regular file's
// identity. Callers should retain the result for the lifetime of their use.
func ResolveOutside(name string, untrustedRoots ...string) (Executable, error) {
	if strings.TrimSpace(name) == "" {
		return Executable{}, errors.New("executable name is required")
	}
	path, err := exec.LookPath(name)
	if err != nil {
		if errors.Is(err, exec.ErrDot) {
			return Executable{}, errors.Join(ErrUntrustedPath, err)
		}
		return Executable{}, err
	}
	// A relative PATH entry lets the command's eventual working directory
	// retarget the executable after resolution. Fixed helpers require one
	// absolute namespace location.
	if !filepath.IsAbs(path) {
		return Executable{}, fmt.Errorf("%w: PATH resolved %s relatively", ErrUntrustedPath, name)
	}
	return ResolvePathOutside(path, untrustedRoots...)
}

// ResolvePathOutside is ResolveOutside for an already selected path. Relative
// paths are made absolute before canonicalization and authority checks.
func ResolvePathOutside(path string, untrustedRoots ...string) (Executable, error) {
	lexical, err := filepath.Abs(path)
	if err != nil {
		return Executable{}, err
	}
	lexical = filepath.Clean(lexical)
	canonical, err := canonicalPath(path)
	if err != nil {
		return Executable{}, err
	}
	if err := rejectAuthorityPath(lexical, canonical, untrustedRoots); err != nil {
		return Executable{}, err
	}
	info, err := stableExecutableInfo(canonical)
	if err != nil {
		return Executable{}, err
	}
	return Executable{path: canonical, info: info}, nil
}

// ResolveDirectoryOutside canonicalizes one absolute PATH directory while
// rejecting lexical aliases and any earlier workspace-controlled ancestor,
// including a directory symlink that later escapes the workspace. It is for
// the PATH passed to fixed dispatcher scripts; it does not resolve programs.
func ResolveDirectoryOutside(path string, untrustedRoots ...string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: PATH directory is relative", ErrUntrustedPath)
	}
	lexical := filepath.Clean(path)
	canonical, err := canonicalPath(path)
	if err != nil {
		return "", err
	}
	if err := rejectAuthorityPath(lexical, canonical, untrustedRoots); err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", errors.Join(err, fmt.Errorf("PATH entry is not a directory: %s", canonical))
	}
	return canonical, nil
}

// FilterEnvironmentPath returns environ with exactly one PATH containing only
// canonical absolute directories outside untrustedRoots. Fixed executables may
// themselves be dispatcher scripts (for example, #!/usr/bin/env node), so
// binding the top-level executable is not enough when an untrusted workspace
// can still select its interpreter. Invalid, missing, relative, and
// workspace-controlled entries are omitted. The call fails closed if no
// trusted entry remains.
func FilterEnvironmentPath(environ []string, untrustedRoots ...string) ([]string, error) {
	out := make([]string, 0, len(environ)+1)
	var rawPath string
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(strings.TrimSpace(key), "PATH") {
			// Match os/exec's last-value-wins environment normalization while
			// ensuring the child never receives duplicate PATH entries.
			rawPath = value
			continue
		}
		out = append(out, entry)
	}

	seen := make(map[string]struct{})
	directories := make([]string, 0, 8)
	for _, entry := range filepath.SplitList(rawPath) {
		if strings.TrimSpace(entry) == "" || !filepath.IsAbs(entry) {
			continue
		}
		directory, err := ResolveDirectoryOutside(entry, untrustedRoots...)
		if err != nil {
			continue
		}
		key := directory
		if filepath.Separator == '\\' {
			key = strings.ToLower(key)
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		directories = append(directories, directory)
	}
	if len(directories) == 0 {
		return nil, fmt.Errorf("%w: no trusted absolute PATH directory remains", ErrUntrustedPath)
	}
	out = append(out, "PATH="+strings.Join(directories, string(os.PathListSeparator)))
	return out, nil
}

func rejectAuthorityPath(lexical, canonical string, untrustedRoots []string) error {
	located := lexical
	if physicalParent, parentErr := filepath.EvalSymlinks(filepath.Dir(lexical)); parentErr == nil {
		// Resolve namespace aliases and parent symlinks, but deliberately retain
		// the final component as a location. Canonicalizing the final symlink
		// alone would let a workspace-owned link to /tmp escape the lexical gate.
		located = filepath.Join(physicalParent, filepath.Base(lexical))
	}
	for _, root := range untrustedRoots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		canonicalRoot, rootErr := canonicalPath(root)
		if rootErr != nil {
			return fmt.Errorf("resolving untrusted root: %w", rootErr)
		}
		lexicalRoot, rootErr := filepath.Abs(root)
		if rootErr != nil {
			return fmt.Errorf("resolving untrusted root: %w", rootErr)
		}
		if within(filepath.Clean(lexicalRoot), lexical) || lexicalAncestorWithin(lexical, canonicalRoot) ||
			within(canonicalRoot, located) ||
			within(canonicalRoot, canonical) {
			return fmt.Errorf("%w: %s", ErrUntrustedPath, canonical)
		}
	}
	return nil
}

// lexicalAncestorWithin detects namespace aliases before following a later
// escaping symlink. For example, macOS may spell the candidate beneath /var
// while the retained workspace root is beneath /private/var, and
// workspace/bin itself may then point out to /tmp. Canonicalizing only the
// candidate's final parent or target loses the fact that an earlier ancestor
// was workspace-controlled.
func lexicalAncestorWithin(path, canonicalRoot string) bool {
	for ancestor := filepath.Dir(path); ; ancestor = filepath.Dir(ancestor) {
		if resolved, err := filepath.EvalSymlinks(ancestor); err == nil && within(canonicalRoot, resolved) {
			return true
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return false
		}
	}
}

func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(real), nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Path returns the canonical path for diagnostics and tests. It does not
// revalidate identity; use Command for execution.
func (e Executable) Path() string { return e.path }

// Command revalidates the exact executable identity and constructs a command.
func (e Executable) Command(args ...string) (*exec.Cmd, error) {
	return e.CommandContext(nil, args...)
}

// CommandContext is Command with cancellation. A nil context uses exec.Command.
func (e Executable) CommandContext(ctx context.Context, args ...string) (*exec.Cmd, error) {
	if e.path == "" || e.info == nil {
		return nil, errors.New("executable was not resolved")
	}
	canonical, err := canonicalPath(e.path)
	if err != nil || canonical != e.path {
		return nil, errors.Join(err, fmt.Errorf("%w: executable path changed", ErrChanged))
	}
	info, err := stableExecutableInfo(e.path)
	if err != nil || !sameExecutable(e.info, info) {
		return nil, errors.Join(err, fmt.Errorf("%w: executable identity changed", ErrChanged))
	}
	if ctx == nil {
		return exec.Command(e.path, args...), nil
	}
	return exec.CommandContext(ctx, e.path, args...), nil
}

func sameExecutable(expected, actual os.FileInfo) bool {
	return expected != nil && actual != nil && os.SameFile(expected, actual) &&
		expected.Mode() == actual.Mode() && expected.Size() == actual.Size() &&
		expected.ModTime().Equal(actual.ModTime())
}
