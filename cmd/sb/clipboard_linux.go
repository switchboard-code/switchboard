package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/safeexec"
)

type linuxClipboardHelper struct {
	name      string
	args      []string
	preferred []string
}

const x11ClipboardSocketDirectory = "/tmp/.X11-unix"

func nativeClipboardWrite(text string) (bool, error) {
	helpers := []linuxClipboardHelper{}
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		helpers = append(helpers, clipboardHelper("wl-copy"))
	}
	helpers = append(helpers,
		linuxClipboardHelper{name: "xclip", args: []string{"-in", "-selection", "clipboard"}, preferred: systemHelperPaths("xclip")},
		linuxClipboardHelper{name: "xsel", args: []string{"--input", "--clipboard"}, preferred: systemHelperPaths("xsel")},
		clipboardHelper("termux-clipboard-set"),
		clipboardHelper("clip.exe"),
	)
	for _, helper := range helpers {
		cmd, err := linuxClipboardCommand(helper)
		if err != nil {
			// An unsafe or absent candidate is not a reason to abandon other
			// independently resolved clipboard backends. None receives bytes
			// until its own resolution and identity recheck succeeds.
			continue
		}
		cmd.Stdin = strings.NewReader(text)
		return true, cmd.Run()
	}
	return false, nil
}

func clipboardHelper(name string) linuxClipboardHelper {
	return linuxClipboardHelper{name: name, preferred: systemHelperPaths(name)}
}

func systemHelperPaths(name string) []string {
	return []string{"/usr/bin/" + name, "/usr/local/bin/" + name, "/bin/" + name}
}

func linuxClipboardCommand(helper linuxClipboardHelper) (*exec.Cmd, error) {
	roots, err := clipboardUntrustedRoots()
	if err != nil {
		return nil, err
	}
	var executable safeexec.Executable
	for _, path := range helper.preferred {
		if _, statErr := os.Lstat(path); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			return nil, statErr
		}
		executable, err = safeexec.ResolvePathOutside(path, roots...)
		if err != nil {
			return nil, err
		}
		break
	}
	if executable.Path() == "" {
		executable, err = safeexec.ResolveOutside(helper.name, roots...)
		if err != nil {
			return nil, err
		}
	}
	cmd, err := executable.Command(helper.args...)
	if err != nil {
		return nil, err
	}
	cmd.Env, err = clipboardHelperEnvironment(roots, helper.name)
	if err != nil {
		return nil, err
	}
	return cmd, nil
}

func clipboardHelperEnvironment(roots []string, helper string) ([]string, error) {
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
		case "BROWSER", "DEFAULT_BROWSER", "WAYLAND_DISPLAY", "WAYLAND_SOCKET", "XDG_RUNTIME_DIR", "DISPLAY":
			continue
		}
		filtered = append(filtered, entry)
	}
	seen := map[string]struct{}{}
	safePath := make([]string, 0, 8)
	add := func(path string) {
		canonical, ok := safeClipboardDirectory(path, roots)
		if !ok {
			return
		}
		if _, exists := seen[canonical]; exists {
			return
		}
		seen[canonical] = struct{}{}
		safePath = append(safePath, canonical)
	}
	for _, path := range []string{"/usr/bin", "/bin", "/usr/local/bin"} {
		add(path)
	}
	for _, path := range filepath.SplitList(pathValue) {
		add(path)
	}
	if len(safePath) == 0 {
		return nil, errors.New("clipboard helper has no trusted interpreter search path")
	}
	switch helper {
	case "wl-copy":
		wayland, err := validatedWaylandClipboardEnvironment(roots)
		if err != nil {
			return nil, err
		}
		filtered = append(filtered, wayland...)
	case "xclip", "xsel":
		display, err := validatedX11ClipboardDisplay(roots)
		if err != nil {
			return nil, err
		}
		filtered = append(filtered, "DISPLAY="+display)
	}
	return append(filtered, "PATH="+strings.Join(safePath, string(os.PathListSeparator))), nil
}

func validatedWaylandClipboardEnvironment(roots []string) ([]string, error) {
	if strings.TrimSpace(os.Getenv("WAYLAND_SOCKET")) != "" {
		// os/exec does not inherit arbitrary descriptors. More importantly, a
		// numeric ambient override has no path identity Switchboard can verify.
		return nil, errors.New("refusing unbound WAYLAND_SOCKET clipboard authority")
	}
	display := strings.TrimSpace(os.Getenv("WAYLAND_DISPLAY"))
	if display == "" {
		return nil, errors.New("no Wayland display is configured")
	}

	runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR"))
	var socketPath string
	if filepath.IsAbs(display) {
		socketPath = filepath.Clean(display)
		runtimeDir = filepath.Dir(socketPath)
	} else {
		clean := filepath.Clean(display)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, errors.New("Wayland display escapes its runtime directory")
		}
		if runtimeDir == "" {
			return nil, errors.New("Wayland clipboard needs XDG_RUNTIME_DIR")
		}
		socketPath = filepath.Join(runtimeDir, clean)
	}
	lexicalRuntime, err := filepath.Abs(runtimeDir)
	if err != nil {
		return nil, errors.New("Wayland runtime directory cannot be resolved")
	}
	lexicalSocket, err := filepath.Abs(socketPath)
	if err != nil || !clipboardPathWithin(filepath.Clean(lexicalRuntime), filepath.Clean(lexicalSocket)) {
		return nil, errors.New("Wayland display escapes its lexical runtime directory")
	}

	canonicalRuntime, err := safeexec.ResolveDirectoryOutside(runtimeDir, roots...)
	if err != nil {
		return nil, errors.New("Wayland runtime directory is controlled by the workspace")
	}
	runtimeInfo, err := os.Stat(canonicalRuntime)
	if err != nil || runtimeInfo.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("Wayland runtime directory is not owner-private")
	}
	runtimeStat, ok := runtimeInfo.Sys().(*syscall.Stat_t)
	if !ok || int(runtimeStat.Uid) != os.Geteuid() {
		return nil, errors.New("Wayland runtime directory is not owned by the current user")
	}

	socketInfo, err := os.Lstat(socketPath)
	if err != nil || socketInfo.Mode()&os.ModeSocket == 0 {
		return nil, errors.New("Wayland display is not a live Unix socket")
	}
	socketStat, ok := socketInfo.Sys().(*syscall.Stat_t)
	if !ok || int(socketStat.Uid) != os.Geteuid() {
		return nil, errors.New("Wayland display socket is not owned by the current user")
	}
	canonicalSocket, err := filepath.EvalSymlinks(socketPath)
	if err != nil || !clipboardPathWithin(canonicalRuntime, canonicalSocket) {
		return nil, errors.New("Wayland display socket is outside its private runtime directory")
	}
	// Always pass the identity-checked absolute socket. Keeping a relative name
	// would make a later runtime-directory namespace change retarget the child.
	return []string{"XDG_RUNTIME_DIR=" + canonicalRuntime, "WAYLAND_DISPLAY=" + canonicalSocket}, nil
}

func validatedX11ClipboardDisplay(roots []string) (string, error) {
	return validatedX11ClipboardDisplayAt(roots, x11ClipboardSocketDirectory)
}

func validatedX11ClipboardDisplayAt(roots []string, socketDirectory string) (string, error) {
	display := strings.TrimSpace(os.Getenv("DISPLAY"))
	if display == "" {
		return "", errors.New("no X11 display is configured")
	}
	rest := ""
	switch {
	case strings.HasPrefix(display, ":"):
		rest = strings.TrimPrefix(display, ":")
	case strings.HasPrefix(display, "unix:"):
		rest = strings.TrimPrefix(display, "unix:")
	default:
		return "", fmt.Errorf("refusing non-local X11 clipboard authority %q", display)
	}
	parts := strings.Split(rest, ".")
	if len(parts) > 2 || parts[0] == "" || !allDecimal(parts[0]) ||
		len(parts) == 2 && (parts[1] == "" || !allDecimal(parts[1])) {
		return "", fmt.Errorf("refusing malformed X11 clipboard authority %q", display)
	}
	number, err := strconv.ParseUint(parts[0], 10, 16)
	if err != nil {
		return "", fmt.Errorf("refusing malformed X11 clipboard authority %q", display)
	}
	canonicalDirectory, err := safeexec.ResolveDirectoryOutside(socketDirectory, roots...)
	if err != nil {
		return "", errors.New("X11 socket directory is not outside workspace authority")
	}
	directoryInfo, err := os.Stat(canonicalDirectory)
	if err != nil {
		return "", errors.New("X11 socket directory is unavailable")
	}
	directoryStat, ok := directoryInfo.Sys().(*syscall.Stat_t)
	if !ok || int(directoryStat.Uid) != 0 && int(directoryStat.Uid) != os.Geteuid() {
		return "", errors.New("X11 socket directory has an unexpected owner")
	}
	socketPath := filepath.Join(canonicalDirectory, "X"+strconv.FormatUint(number, 10))
	socketInfo, err := os.Lstat(socketPath)
	if err != nil || socketInfo.Mode()&os.ModeSocket == 0 {
		return "", errors.New("X11 display is not a live local Unix socket")
	}
	socketStat, ok := socketInfo.Sys().(*syscall.Stat_t)
	if !ok || int(socketStat.Uid) != 0 && int(socketStat.Uid) != os.Geteuid() {
		return "", errors.New("X11 display socket has an unexpected owner")
	}
	canonicalSocket, err := filepath.EvalSymlinks(socketPath)
	if err != nil || !clipboardPathWithin(canonicalDirectory, canonicalSocket) {
		return "", errors.New("X11 display socket escaped its fixed directory")
	}
	return display, nil
}

func allDecimal(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != ""
}

func clipboardPathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func safeClipboardDirectory(path string, roots []string) (string, bool) {
	canonical, err := safeexec.ResolveDirectoryOutside(path, roots...)
	return canonical, err == nil
}

func clipboardUntrustedRoots() ([]string, error) {
	roots, err := safeexec.CurrentWorkspaceAuthorityRoots()
	if err != nil {
		return nil, fmt.Errorf("resolving clipboard workspace authority: %w", err)
	}
	return roots, nil
}
