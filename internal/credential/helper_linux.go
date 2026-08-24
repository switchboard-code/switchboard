package credential

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/safeexec"
)

// resolveLinuxHelper prefers conventional system installations, then admits a
// PATH installation only when it has one absolute canonical identity outside
// the current workspace authority. Relative PATH entries and repository-local
// shadows are refused by safeexec.
func resolveLinuxHelper(name string, preferred ...string) (safeexec.Executable, error) {
	roots, err := helperUntrustedRoots()
	if err != nil {
		return safeexec.Executable{}, err
	}
	for _, path := range preferred {
		if _, statErr := os.Lstat(path); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			return safeexec.Executable{}, statErr
		}
		executable, resolveErr := safeexec.ResolvePathOutside(path, roots...)
		if resolveErr != nil {
			return safeexec.Executable{}, resolveErr
		}
		return executable, nil
	}
	return safeexec.ResolveOutside(name, roots...)
}

// helperUntrustedRoots delegates to the common canonical all-ancestor
// collector so credential helpers, clipboard helpers, and fixed Git/Codex
// subprocesses cannot drift into different workspace-authority policies.
func helperUntrustedRoots() ([]string, error) {
	return safeexec.CurrentWorkspaceAuthorityRoots()
}

// linuxHelperEnvironment keeps only desktop/session state whose authority can
// be bound outside the workspace. BROWSER and PATH can redirect xdg-open's
// child, XDG handler roots can select executable .desktop files, and display
// endpoints can hand an OAuth target to a workspace-controlled server.
func linuxHelperEnvironment(requireSessionBus bool) ([]string, error) {
	roots, err := helperUntrustedRoots()
	if err != nil {
		return nil, err
	}
	base := execution.ScrubbedChildEnv()
	filtered := make([]string, 0, len(base)+8)
	var pathValue string
	for _, entry := range base {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch strings.ToUpper(name) {
		case "PATH":
			pathValue = value
			continue
		case "DBUS_SESSION_BUS_ADDRESS":
			// Never retain the ambient spelling. A validated, filesystem-bound
			// address is appended below; an invalid optional browser bus must be
			// absent rather than falling through from ScrubbedChildEnv.
			continue
		case "BROWSER", "DEFAULT_BROWSER":
			continue
		case "HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME":
			// xdg-open resolves executable .desktop handlers beneath these
			// directories. Preserve ordinary user locations, but never hand a
			// checkout authority over the browser dispatcher.
			if canonical, safe := safeHelperDirectory(value, roots); safe {
				filtered = append(filtered, name+"="+canonical)
			}
			continue
		case "XDG_DATA_DIRS", "XDG_CONFIG_DIRS":
			if directories := safeHelperPathList(value, roots); directories != "" {
				filtered = append(filtered, name+"="+directories)
			}
			continue
		case "XDG_RUNTIME_DIR":
			if canonical, safe := safeOwnerPrivateRuntimeDirectory(value, roots); safe {
				filtered = append(filtered, name+"="+canonical)
			}
			continue
		case "DISPLAY", "WAYLAND_DISPLAY", "WAYLAND_SOCKET":
			// Arbitrary descriptors are not inherited by os/exec and have no
			// filesystem identity this boundary can validate. Display names are
			// also withheld; when present, the identity-checked session bus is
			// the only desktop endpoint inherited by browser dispatch.
			continue
		}
		filtered = append(filtered, entry)
	}
	if !requireSessionBus {
		// Browser dispatch may need these nonsecret desktop labels. The keyring
		// helper does not, so its environment stays narrower.
		for _, name := range []string{
			"DESKTOP_SESSION",
			"GNOME_DESKTOP_SESSION_ID",
			"KDE_FULL_SESSION",
			"XDG_SESSION_CLASS",
			"XDG_SESSION_DESKTOP",
			"XDG_SESSION_TYPE",
		} {
			if value, ok := os.LookupEnv(name); ok && value != "" {
				filtered = append(filtered, name+"="+value)
			}
		}
	}
	bus, busErr := validatedSessionBusAddress(roots)
	if busErr != nil && requireSessionBus {
		return nil, busErr
	}
	if busErr == nil && bus != "" {
		filtered = append(filtered, "DBUS_SESSION_BUS_ADDRESS="+bus)
	}

	seen := map[string]struct{}{}
	safePath := make([]string, 0, 8)
	add := func(path string) {
		canonical, ok := safeHelperDirectory(path, roots)
		if !ok {
			return
		}
		if _, exists := seen[canonical]; exists {
			return
		}
		seen[canonical] = struct{}{}
		safePath = append(safePath, canonical)
	}
	// System dispatchers first, then user-selected absolute installations such
	// as Nix or Homebrew when they are outside the workspace authority.
	for _, path := range []string{"/usr/bin", "/bin", "/usr/local/bin"} {
		add(path)
	}
	for _, path := range filepath.SplitList(pathValue) {
		add(path)
	}
	filtered = append(filtered, "PATH="+strings.Join(safePath, string(os.PathListSeparator)))
	return filtered, nil
}

func safeHelperPathList(value string, roots []string) string {
	seen := map[string]struct{}{}
	var safe []string
	for _, path := range filepath.SplitList(value) {
		canonical, ok := safeHelperDirectory(path, roots)
		if !ok {
			continue
		}
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		safe = append(safe, canonical)
	}
	return strings.Join(safe, string(os.PathListSeparator))
}

func safeOwnerPrivateRuntimeDirectory(path string, roots []string) (string, bool) {
	canonical, ok := safeHelperDirectory(path, roots)
	if !ok {
		return "", false
	}
	info, err := os.Stat(canonical)
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		return "", false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return canonical, ok && int(stat.Uid) == os.Geteuid()
}

// validatedSessionBusAddress accepts only the ordinary filesystem-backed user
// bus. An arbitrary/abstract D-Bus address would let a checkout receive the
// credential even though secret-tool itself was safely resolved.
func validatedSessionBusAddress(roots []string) (string, error) {
	raw := strings.TrimSpace(os.Getenv("DBUS_SESSION_BUS_ADDRESS"))
	if raw == "" {
		return "", errors.New("no D-Bus session bus is configured")
	}
	if strings.Contains(raw, ";") {
		return "", errors.New("multiple D-Bus session bus addresses are not accepted for credential storage")
	}
	transport, options, ok := strings.Cut(raw, ":")
	if !ok || transport != "unix" {
		return "", errors.New("the D-Bus session bus is not a filesystem-backed Unix socket")
	}
	var encodedPath string
	for _, field := range strings.Split(options, ",") {
		key, value, present := strings.Cut(field, "=")
		if !present {
			return "", errors.New("the D-Bus session bus address is malformed")
		}
		switch key {
		case "path":
			if encodedPath != "" {
				return "", errors.New("the D-Bus session bus address carries multiple paths")
			}
			encodedPath = value
		case "guid":
			// A server identifier does not change the socket authority.
		default:
			return "", fmt.Errorf("the D-Bus session bus uses unsupported %q addressing", key)
		}
	}
	if encodedPath == "" {
		return "", errors.New("abstract or pathless D-Bus session buses are not accepted for credential storage")
	}
	path, err := url.PathUnescape(encodedPath)
	if err != nil || !filepath.IsAbs(path) {
		return "", errors.New("the D-Bus session bus path is not a valid absolute path")
	}
	path = filepath.Clean(path)
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return "", errors.New("the D-Bus session bus path is not a live Unix socket")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return "", errors.New("the D-Bus session bus socket is not owned by the current user")
	}
	canonicalSocket, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", errors.New("the D-Bus session bus path could not be bound")
	}
	for _, root := range roots {
		rootAbs, absErr := filepath.Abs(root)
		if absErr != nil {
			return "", errors.New("the current workspace authority could not be resolved")
		}
		rootCanonical, canonicalErr := filepath.EvalSymlinks(rootAbs)
		if canonicalErr != nil ||
			helperPathWithin(filepath.Clean(rootAbs), path) || helperPathWithin(rootCanonical, canonicalSocket) {
			return "", errors.New("the D-Bus session bus is controlled by the current workspace")
		}
	}

	candidates := []string{os.Getenv("XDG_RUNTIME_DIR"), filepath.Join("/run/user", fmt.Sprint(os.Geteuid()))}
	for _, runtimeDir := range candidates {
		canonical, safe := safeHelperDirectory(runtimeDir, roots)
		if !safe {
			continue
		}
		runtimeInfo, statErr := os.Stat(canonical)
		if statErr != nil || runtimeInfo.Mode().Perm()&0o077 != 0 {
			continue
		}
		runtimeStat, ownerOK := runtimeInfo.Sys().(*syscall.Stat_t)
		if !ownerOK || int(runtimeStat.Uid) != os.Geteuid() {
			continue
		}
		if !helperPathWithin(canonical, canonicalSocket) {
			continue
		}
		// The lexical check prevents a workspace link to a safe runtime socket
		// from laundering workspace-controlled addressing.
		if !helperPathWithin(filepath.Clean(runtimeDir), path) {
			continue
		}
		return raw, nil
	}
	return "", errors.New("the D-Bus session bus is outside the owner's private runtime directory")
}

func safeHelperDirectory(path string, roots []string) (string, bool) {
	canonical, err := safeexec.ResolveDirectoryOutside(path, roots...)
	return canonical, err == nil
}

func helperPathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
