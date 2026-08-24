package execution

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

// The home directory is denied wholesale and opened back up only where a
// toolchain needs it. Everywhere else on the filesystem stays broadly readable.
//
// The split is deliberate. System directories hold no user secrets, and an
// allowlist over them would break every compiler for no gain. Home is the
// opposite: it is where credentials actually live, and enumerating what leaks
// there is a race nobody wins. A survey of one ordinary developer machine found
// 51 top-level entries, of which a hand-written deny list covered six; the
// readable remainder included an npm auth token, shell history, and the
// credential directories of five different CLI tools.
//
// So reads follow the risk. Broad where secrets are not, closed where they are.

// homeReadable lists paths under the home directory a confined command may
// read. A version manager keeps the actual compiler under home, so denying
// these removes the tool rather than protecting anything.
var homeReadable = []string{
	// Build caches. These are also granted for writing.
	".cache",
	".npm",
	".cargo",
	filepath.Join("go", "pkg", "mod"),

	// Toolchains installed per-user.
	".rustup",
	".nvm",
	".asdf",
	".pyenv",
	".rbenv",
	".bun",
	".deno",
	".volta",
	".sdkman",
	".ghcup",
	".local",

	// Configuration a build legitimately reads.
	".gitconfig",
	filepath.Join(".config", "git"),
}

// homeSecrets are denied even though they sit inside something homeReadable
// opened. A toolchain directory is not uniformly safe: cargo keeps registry
// tokens beside its package cache, and the XDG data directory holds the Linux
// keyring beside legitimately shared files.
var homeSecrets = []string{
	".ssh",
	".aws",
	filepath.Join(".config", "gcloud"),
	".kube",
	".docker",
	".gnupg",
	filepath.Join("Library", "Keychains"),
	filepath.Join(".config", "ssh"),
	".switchboard",
	filepath.Join(".cargo", "credentials"),
	filepath.Join(".cargo", "credentials.toml"),
	filepath.Join(".config", "git", "credentials"),
	filepath.Join(".local", "share", "keyrings"),
	filepath.Join(".local", "share", "containers", "auth.json"),
}

// platformHomeReadable adds what a specific system puts under home. macOS keeps
// the Go build cache inside Library, which the shared list would otherwise
// leave denied.
func platformHomeReadable() []string {
	if runtime.GOOS == "darwin" {
		return []string{filepath.Join("Library", "Caches", "go-build")}
	}
	return nil
}

// existingHomePaths resolves a relative list against home and drops what is not
// there. It also rejects every symlink component. HOME is ambient input, and a
// checkout-controlled .asdf -> ~/.ssh alias must not turn an allowlisted
// toolchain path into a credential reopen after the account home was hidden.
func existingHomePaths(home string, rels []string) []string {
	var out []string
	for _, rel := range rels {
		if p, ok := safeHomeDescendant(home, rel, false); ok {
			out = append(out, p)
		}
	}
	return out
}

// safeHomeDescendant accepts a lexical descendant only when every existing
// component is a real directory (or the final requested regular file), never a
// symlink. When allowMissing is true, an absent suffix is accepted after all of
// its existing parents pass. It is used only to narrow authority; a false
// result simply omits an optional cache or toolchain path.
func safeHomeDescendant(home, rel string, allowMissing bool) (string, bool) {
	rel = filepath.Clean(rel)
	if rel == "." || filepath.IsAbs(rel) || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	root, err := rootedfs.OpenRoot(home)
	if err != nil {
		return "", false
	}
	defer root.Close()

	parts := strings.Split(rel, string(filepath.Separator))
	for i := range parts {
		prefix := filepath.Join(parts[:i+1]...)
		info, err := root.Lstat(prefix)
		if err != nil {
			if allowMissing && errors.Is(err, os.ErrNotExist) {
				return filepath.Join(home, rel), true
			}
			return "", false
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", false
		}
		if i < len(parts)-1 && !info.IsDir() {
			return "", false
		}
	}
	return filepath.Join(home, rel), true
}

func readableHomePaths(home string) []string {
	rels := append(append([]string{}, homeReadable...), platformHomeReadable()...)
	var out []string
	for _, rel := range rels {
		path, ok := safeHomeDescendant(home, rel, false)
		if !ok || hasSymlinkedProtectedDescendant(home, rel) {
			continue
		}
		out = append(out, path)
	}
	return out
}

func secretHomePaths(home string) []string {
	return existingHomePaths(home, homeSecrets)
}

// protectedHomePaths does not require the paths to exist. It is used when
// deciding whether an exact toolchain root is safe to reopen: a missing
// credential directory can be created later, so absence is not evidence that
// an overlapping root is harmless.
func protectedHomePaths(home string) []string {
	out := make([]string, 0, len(homeSecrets))
	for _, rel := range homeSecrets {
		out = append(out, filepath.Join(home, rel))
	}
	return out
}

// safeWritableHomeCache reports whether a whole cache directory can be
// reopened. Known credential paths live below some caches (notably .cargo).
// When one of those paths has any symlink component, reopening the parent would
// expose its target under broad filesystem reads. Callers fall back to an
// ephemeral cache or no cache grant instead.
func safeWritableHomeCache(home, rel string) bool {
	_, ok := safeHomeDescendant(home, rel, true)
	return ok && !hasSymlinkedProtectedDescendant(home, rel)
}

func hasSymlinkedProtectedDescendant(home, ancestorRel string) bool {
	ancestor := filepath.Join(home, ancestorRel)
	for _, protectedRel := range homeSecrets {
		protected := filepath.Join(home, protectedRel)
		if !directoryContains(ancestor, protected) {
			continue
		}
		if homePathHasSymlink(home, protectedRel) {
			return true
		}
	}
	return false
}

// homePathHasSymlink distinguishes an absent protected path (nothing to hide)
// from a symlinked one (whose target would escape a lexical deny). Errors other
// than absence fail closed.
func homePathHasSymlink(home, rel string) bool {
	root, err := rootedfs.OpenRoot(home)
	if err != nil {
		return true
	}
	defer root.Close()

	parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
	for i := range parts {
		prefix := filepath.Join(parts[:i+1]...)
		info, err := root.Lstat(prefix)
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		if err != nil {
			return true
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return true
		}
		if i < len(parts)-1 && !info.IsDir() {
			return false
		}
	}
	return false
}

// resolvedSymlinkedProtectedTargets returns the targets of credential paths
// whose ancestry contains a symlink. Seatbelt matches fully resolved paths, so
// denying only ~/.cargo/credentials does not stop that name from resolving to
// /tmp/credential under its broad filesystem-read floor. Only the canonical
// account home may contribute these extra denies; an ambient HOME is attacker
// input and must not be able to reshape policy around arbitrary host paths.
func resolvedSymlinkedProtectedTargets(accountHome string) ([]string, error) {
	seen := make(map[string]bool)
	var targets []string
	for _, rel := range homeSecrets {
		if !homePathHasSymlink(accountHome, rel) {
			continue
		}
		path := filepath.Join(accountHome, rel)
		first, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, fmt.Errorf("resolving protected account path %q: %w", path, err)
		}
		second, err := filepath.EvalSymlinks(path)
		if err != nil || second != first {
			return nil, fmt.Errorf("protected account path %q changed while it was resolved", path)
		}
		if first == path || seen[first] {
			continue
		}
		seen[first] = true
		targets = append(targets, first)
	}
	sort.Strings(targets)
	return targets, nil
}
