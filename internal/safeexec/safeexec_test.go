package safeexec

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWorkspaceAuthorityRootsCanonicalizesAliasAndCollectsEveryVCSAncestor(t *testing.T) {
	repository := t.TempDir()
	nested := filepath.Join(repository, "nested")
	workspace := filepath.Join(nested, "workspace")
	for _, dir := range []string{
		filepath.Join(repository, ".git"),
		filepath.Join(nested, ".hg"),
		filepath.Join(workspace, ".svn"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(t.TempDir(), "workspace-alias")
	if err := os.Symlink(workspace, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	roots, err := WorkspaceAuthorityRoots(alias)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{workspace, nested, repository}
	if len(roots) != len(want) {
		t.Fatalf("authority roots = %q, want %q", roots, want)
	}
	for i := range want {
		canonical, err := filepath.EvalSymlinks(want[i])
		if err != nil {
			t.Fatal(err)
		}
		if roots[i] != filepath.Clean(canonical) {
			t.Fatalf("authority root %d = %q, want %q", i, roots[i], canonical)
		}
	}
}

func TestWorkspaceAndCurrentAuthorityRootsRejectsLaunchCheckoutForDifferentTarget(t *testing.T) {
	launch := t.TempDir()
	if err := os.Mkdir(filepath.Join(launch, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	launchBin := filepath.Join(launch, "bin")
	if err := os.Mkdir(launchBin, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "switchboard-cross-workspace-shadow"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	shadow := filepath.Join(launchBin, name)
	if err := os.WriteFile(shadow, []byte("shadow"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(launch)
	target := t.TempDir()
	roots, err := WorkspaceAndCurrentAuthorityRoots(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolvePathOutside(shadow, roots...); !errors.Is(err, ErrUntrustedPath) {
		t.Fatalf("launch-checkout executable error = %v, want ErrUntrustedPath", err)
	}

	trusted := t.TempDir()
	filtered, err := FilterEnvironmentPath([]string{
		"PATH=" + launchBin + string(os.PathListSeparator) + trusted,
	}, roots...)
	if err != nil {
		t.Fatal(err)
	}
	trustedCanonical, err := filepath.EvalSymlinks(trusted)
	if err != nil {
		t.Fatal(err)
	}
	if got := environmentValue(filtered, "PATH"); got != trustedCanonical {
		t.Fatalf("filtered PATH = %q, want only %q", got, trustedCanonical)
	}
}

func TestCurrentAuthorityPreservesHomeLaunchUserInstall(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "switchboard-user-install"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(bin, name)
	if err := os.WriteFile(path, []byte("user install"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Exercise the home exception with an explicit account-database value.
	// Production deliberately does not derive this value from mutable HOME.
	roots, err := currentWorkspaceAuthorityRoots(home, home)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolvePathOutside(path, roots...)
	if err != nil {
		t.Fatalf("home-launched user install was rejected: %v", err)
	}
	want, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Path() != want {
		t.Fatalf("resolved path = %q, want %q", resolved.Path(), want)
	}
}

func TestCurrentAuthorityDoesNotTrustForgedHome(t *testing.T) {
	workspace := t.TempDir()
	t.Chdir(workspace)
	t.Setenv("HOME", workspace)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", workspace)
		t.Setenv("HOMEDRIVE", filepath.VolumeName(workspace))
		t.Setenv("HOMEPATH", strings.TrimPrefix(workspace, filepath.VolumeName(workspace)))
	}

	roots, err := CurrentWorkspaceAuthorityRoots()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace, "forged-home-helper")
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	if err := os.WriteFile(path, []byte("workspace authority"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolvePathOutside(path, roots...); !errors.Is(err, ErrUntrustedPath) {
		t.Fatalf("forged-home executable error = %v, want ErrUntrustedPath", err)
	}
}

func TestCurrentAuthorityPreservesFilesystemRootLaunch(t *testing.T) {
	root := string(filepath.Separator)
	if volume := filepath.VolumeName(os.TempDir()); volume != "" {
		root = volume + string(filepath.Separator)
	}
	t.Chdir(root)
	bin := t.TempDir()
	name := "switchboard-root-launch-install"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(bin, name)
	if err := os.WriteFile(path, []byte("system install"), 0o755); err != nil {
		t.Fatal(err)
	}
	roots, err := WorkspaceAndCurrentAuthorityRoots(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolvePathOutside(path, roots...); err != nil {
		t.Fatalf("filesystem-root launch rejected external install: %v", err)
	}
}

func TestMarkedPackagePrefixRemainsTrustedWhenNotAnAuthorityRoot(t *testing.T) {
	// Package managers such as Homebrew keep their installation prefix in a
	// Git checkout. A marker alone does not make an unrelated absolute PATH
	// entry workspace-controlled; only the selected target and launch
	// authorities do.
	prefix := t.TempDir()
	if err := os.Mkdir(filepath.Join(prefix, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(prefix, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "switchboard-package-install"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(bin, name)
	if err := os.WriteFile(path, []byte("package install"), 0o755); err != nil {
		t.Fatal(err)
	}
	roots, err := WorkspaceAndCurrentAuthorityRoots(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if _, err := ResolveOutside(name, roots...); err != nil {
		t.Fatalf("marked package-prefix executable was rejected: %v", err)
	}
	filtered, err := FilterEnvironmentPath([]string{"PATH=" + bin}, roots...)
	if err != nil {
		t.Fatalf("marked package-prefix PATH was rejected: %v", err)
	}
	wantBin, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatal(err)
	}
	if got := environmentValue(filtered, "PATH"); got != wantBin {
		t.Fatalf("filtered PATH = %q, want %q", got, wantBin)
	}
}

func environmentValue(environ []string, name string) string {
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(key, name) {
			return value
		}
	}
	return ""
}

func TestResolveOutsideRejectsWorkspacePATHShadow(t *testing.T) {
	workspace := t.TempDir()
	name := "switchboard-shadow-helper"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(workspace, name)
	if err := os.WriteFile(path, []byte("not really executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", workspace+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := ResolveOutside(name, workspace); !errors.Is(err, ErrUntrustedPath) {
		t.Fatalf("error = %v, want ErrUntrustedPath", err)
	}
}

func TestResolveOutsideRejectsRelativePATHResult(t *testing.T) {
	workspace := t.TempDir()
	bin := filepath.Join(workspace, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "switchboard-relative-shadow"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(bin, name), []byte("shadow"), 0o755); err != nil {
		t.Fatal(err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workspace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	t.Setenv("PATH", "bin")
	if _, err := ResolveOutside(name, workspace); !errors.Is(err, ErrUntrustedPath) {
		t.Fatalf("error = %v, want ErrUntrustedPath", err)
	}
}

func TestResolvePathOutsideAllowsExternalExecutable(t *testing.T) {
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err := ResolvePathOutside(path, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if executable.Path() == "" {
		t.Fatal("canonical executable path is empty")
	}
	if _, err := executable.Command("-test.run=^$"); err != nil {
		t.Fatal(err)
	}
}

func TestResolveOutsideRejectsWorkspaceSymlinkToExternalExecutable(t *testing.T) {
	workspace := t.TempDir()
	external := filepath.Join(t.TempDir(), "external-helper")
	if runtime.GOOS == "windows" {
		external += ".exe"
	}
	if err := os.WriteFile(external, []byte("external"), 0o755); err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(external)
	if err := os.Symlink(external, filepath.Join(workspace, name)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("PATH", workspace+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := ResolveOutside(name, workspace); !errors.Is(err, ErrUntrustedPath) {
		t.Fatalf("error = %v, want ErrUntrustedPath", err)
	}
}

func TestResolveOutsideRejectsAliasedWorkspaceDirectorySymlink(t *testing.T) {
	physicalParent := t.TempDir()
	workspace := filepath.Join(physicalParent, "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasBase := t.TempDir()
	alias := filepath.Join(aliasBase, "parent-alias")
	if err := os.Symlink(physicalParent, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	evilBin := t.TempDir()
	name := "switchboard-directory-shadow"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(evilBin, name), []byte("external"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(evilBin, filepath.Join(workspace, "bin")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	aliasedBin := filepath.Join(alias, "workspace", "bin")
	t.Setenv("PATH", aliasedBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := ResolveOutside(name, workspace); !errors.Is(err, ErrUntrustedPath) {
		t.Fatalf("error = %v, want ErrUntrustedPath", err)
	}
	if _, err := ResolveDirectoryOutside(aliasedBin, workspace); !errors.Is(err, ErrUntrustedPath) {
		t.Fatalf("PATH directory error = %v, want ErrUntrustedPath", err)
	}
}

func TestFilterEnvironmentPathOmitsWorkspaceAndRelativeDispatchers(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	alias := filepath.Join(workspace, "external-alias")
	if err := os.Symlink(external, alias); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	environ := []string{
		"SB_SAFEEXEC_TEST=kept",
		"PATH=" + strings.Join([]string{workspace, "relative-bin", alias, external, external}, string(os.PathListSeparator)),
	}
	filtered, err := FilterEnvironmentPath(environ, workspace)
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string][]string)
	for _, entry := range filtered {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[strings.ToUpper(key)] = append(values[strings.ToUpper(key)], value)
		}
	}
	want, err := filepath.EvalSymlinks(external)
	if err != nil {
		t.Fatal(err)
	}
	if got := values["PATH"]; len(got) != 1 || got[0] != want {
		t.Fatalf("filtered PATH = %q, want exactly %q", got, want)
	}
	if got := values["SB_SAFEEXEC_TEST"]; len(got) != 1 || got[0] != "kept" {
		t.Fatalf("harmless environment = %q", got)
	}
}

func TestFilterEnvironmentPathFailsClosedWithoutTrustedDirectory(t *testing.T) {
	workspace := t.TempDir()
	_, err := FilterEnvironmentPath([]string{"PATH=" + workspace + string(os.PathListSeparator) + "relative"}, workspace)
	if !errors.Is(err, ErrUntrustedPath) {
		t.Fatalf("error = %v, want ErrUntrustedPath", err)
	}
}
