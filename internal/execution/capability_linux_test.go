package execution

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func testBubblewrapFixture(t *testing.T) (bubblewrapExecutable, string, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(bin, "bwrap")
	if err := os.WriteFile(path, testELFImage("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	bwrap, err := resolveBubblewrapExecutable(path, root, uint32(os.Geteuid()), false)
	if err != nil {
		t.Fatalf("resolving trusted fixture: %v", err)
	}
	return bwrap, root, path
}

func testELFImage(payload string) []byte {
	return append([]byte("\x7fELF"), []byte(payload)...)
}

func testBubblewrapExecutable(t *testing.T) bubblewrapExecutable {
	t.Helper()
	bwrap, _, _ := testBubblewrapFixture(t)
	return bwrap
}

func TestBubblewrapResolutionReturnsCanonicalAbsoluteSystemPath(t *testing.T) {
	bwrap, root, path := testBubblewrapFixture(t)
	alias := filepath.Join(root, "bin", "bwrap-alias")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveBubblewrapExecutable(alias, root, uint32(os.Geteuid()), false)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(resolved.path) {
		t.Fatalf("resolved path %q is not absolute", resolved.path)
	}
	if resolved.path != bwrap.path {
		t.Fatalf("resolved path = %q, want canonical target %q", resolved.path, bwrap.path)
	}
}

func TestDetectRejectsUntrustedBubblewrapFromPATH(t *testing.T) {
	_, _, path := testBubblewrapFixture(t)
	t.Setenv("PATH", filepath.Dir(path))

	capability := detectPlatform()
	if !capability.MechanismPresent {
		t.Fatal("PATH candidate existed but was not reported as present")
	}
	if capability.AutomaticExecutionAllowed() {
		t.Fatal("user-controlled PATH candidate enabled automatic execution")
	}
	if !strings.Contains(capability.Detail, "not a trusted system executable") {
		t.Fatalf("detail = %q, want trust refusal", capability.Detail)
	}
}

func TestBubblewrapResolutionRejectsEnvShebangBeforeWorkspaceInterpreterCanRun(t *testing.T) {
	root := t.TempDir()
	trustedBin := filepath.Join(root, "trusted-bin")
	workspaceBin := filepath.Join(root, "workspace-bin")
	if err := os.Mkdir(trustedBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workspaceBin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "workspace-bash-ran")
	workspaceBash := "#!/bin/sh\n/usr/bin/touch '" + marker + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(workspaceBin, "bash"), []byte(workspaceBash), 0o755); err != nil {
		t.Fatal(err)
	}
	bwrapPath := filepath.Join(trustedBin, "bwrap")
	if err := os.WriteFile(bwrapPath, []byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", workspaceBin+string(os.PathListSeparator)+trustedBin)

	_, err := resolveBubblewrapExecutable(bwrapPath, root, uint32(os.Geteuid()), false)
	if err == nil || !strings.Contains(err.Error(), "native ELF") {
		t.Fatalf("env-shebang bubblewrap error = %v, want native-ELF refusal", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("workspace PATH interpreter executed before confinement: %v", err)
	}
}

func TestLinuxHostKeyDoesNotExecutePATHUname(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "uname-ran")
	shadow := filepath.Join(dir, "uname")
	if err := os.WriteFile(shadow, []byte("#!/bin/sh\n: > \"$UNAME_MARKER\"\nprintf poison\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("UNAME_MARKER", marker)
	t.Setenv("PATH", dir)

	if key := linuxHostKey(); key == "unknown" || strings.Contains(key, "poison") {
		t.Fatalf("linux host key = %q, want kernel identity from uname(2)", key)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("PATH-shadowed uname executed; marker stat error = %v", err)
	}
}

func TestEgressProbeDoesNotExecutePATHShadow(t *testing.T) {
	workspace := t.TempDir()
	shadowMarker := filepath.Join(workspace, "shadow-ran")
	shadow := filepath.Join(workspace, "curl")
	if err := os.WriteFile(shadow, []byte("#!/bin/sh\n: > \"$SHADOW_PROBE_MARKER\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fixedDir := t.TempDir()
	fixedMarker := filepath.Join(fixedDir, "fixed-ran")
	fixed := filepath.Join(fixedDir, "curl")
	if err := os.WriteFile(fixed, []byte("#!/bin/sh\n: > \"$FIXED_PROBE_MARKER\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", workspace)
	t.Setenv("SHADOW_PROBE_MARKER", shadowMarker)
	t.Setenv("FIXED_PROBE_MARKER", fixedMarker)

	argv := selectEgressProbe([]egressProbeCandidate{{paths: []string{"curl", fixed}}})
	if len(argv) == 0 || argv[0] != fixed {
		t.Fatalf("egress probe = %q, want fixed absolute candidate %q", argv, fixed)
	}
	if err := exec.Command(argv[0], argv[1:]...).Run(); err != nil {
		t.Fatalf("running fixed probe fixture: %v", err)
	}
	if _, err := os.Stat(fixedMarker); err != nil {
		t.Fatalf("fixed probe fixture did not run: %v", err)
	}
	if _, err := os.Stat(shadowMarker); !os.IsNotExist(err) {
		t.Fatalf("PATH-shadowed network probe executed: %v", err)
	}
}

func TestBubblewrapResolutionRejectsWritableOrUntrustedProvenance(t *testing.T) {
	t.Run("writable executable", func(t *testing.T) {
		_, root, path := testBubblewrapFixture(t)
		if err := os.Chmod(path, 0o775); err != nil {
			t.Fatal(err)
		}
		_, err := resolveBubblewrapExecutable(path, root, uint32(os.Geteuid()), false)
		if err == nil || !strings.Contains(err.Error(), "group- or other-writable") {
			t.Fatalf("err = %v, want writable-executable refusal", err)
		}
	})

	t.Run("writable parent", func(t *testing.T) {
		_, root, path := testBubblewrapFixture(t)
		if err := os.Chmod(filepath.Dir(path), 0o777); err != nil {
			t.Fatal(err)
		}
		_, err := resolveBubblewrapExecutable(path, root, uint32(os.Geteuid()), false)
		if err == nil || !strings.Contains(err.Error(), "parent") || !strings.Contains(err.Error(), "writable") {
			t.Fatalf("err = %v, want writable-parent refusal", err)
		}
	})

	t.Run("untrusted owner", func(t *testing.T) {
		_, root, path := testBubblewrapFixture(t)
		otherUID := uint32(os.Geteuid()) ^ 1
		_, err := resolveBubblewrapExecutable(path, root, otherUID, false)
		if err == nil || !strings.Contains(err.Error(), "owned by uid") {
			t.Fatalf("err = %v, want ownership refusal", err)
		}
	})

	t.Run("current user access through mode or ACL", func(t *testing.T) {
		_, root, path := testBubblewrapFixture(t)
		_, err := resolveBubblewrapExecutable(path, root, uint32(os.Geteuid()), true)
		if err == nil || !strings.Contains(err.Error(), "writable by the current user") {
			t.Fatalf("err = %v, want effective-current-user writability refusal", err)
		}
	})
}

func TestBubblewrapReplacementAfterDetectionFailsClosed(t *testing.T) {
	bwrap, root, path := testBubblewrapFixture(t)
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	keyBefore, err := linuxProfileKey(bwrap)
	if err != nil {
		t.Fatalf("profile key before replacement: %v", err)
	}
	replacement := path + ".replacement"
	if err := os.WriteFile(replacement, testELFImage("replacement"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(replacement, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}

	if _, err := bwrap.wrap(Policy{Workspace: root, Network: NetworkLoopback}, []string{"/bin/true"}); err == nil || !strings.Contains(err.Error(), "changed after sandbox verification") {
		t.Fatalf("wrap err = %v, want changed-executable refusal", err)
	}

	current, err := resolveBubblewrapExecutable(path, root, uint32(os.Geteuid()), false)
	if err != nil {
		t.Fatalf("resolving replacement: %v", err)
	}
	keyAfter, err := linuxProfileKey(current)
	if err != nil {
		t.Fatalf("profile key after replacement: %v", err)
	}
	if keyAfter == keyBefore {
		t.Fatalf("profile key remained %q across executable replacement", keyBefore)
	}
}

func TestBubblewrapEndsOptionsBeforeModelArgv(t *testing.T) {
	bwrap, root, _ := testBubblewrapFixture(t)
	modelArgv := []string{"--bind", root, "/etc", "/bin/true"}
	wrapperArgv, err := bwrap.wrap(Policy{Workspace: root, Network: NetworkLoopback}, modelArgv)
	if err != nil {
		t.Fatal(err)
	}
	commandAt := len(wrapperArgv) - len(modelArgv)
	if commandAt < 1 || wrapperArgv[commandAt-1] != "--" {
		t.Fatalf("wrapper argv does not end options before model argv: %q", wrapperArgv)
	}
	if !slices.Equal(wrapperArgv[commandAt:], modelArgv) {
		t.Fatalf("model argv changed: got %q, want %q", wrapperArgv[commandAt:], modelArgv)
	}
}

func TestBubblewrapReopensOnlyBoundToolchainRoot(t *testing.T) {
	bwrap, root, _ := testBubblewrapFixture(t)
	toolchain := filepath.Join(root, "toolchain")
	if err := os.Mkdir(toolchain, 0o755); err != nil {
		t.Fatal(err)
	}
	wrapperArgv, err := bwrap.wrap(Policy{
		Workspace:     root,
		Network:       NetworkLoopback,
		readOnlyRoots: []string{toolchain},
	}, []string{"/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--ro-bind", toolchain, toolchain}
	if !containsArgvSequence(wrapperArgv, want) {
		t.Fatalf("wrapper argv omitted bound toolchain root %q: %q", toolchain, wrapperArgv)
	}
}

func TestBubblewrapProtectsAccountHomeWhenHOMEIsWorkspace(t *testing.T) {
	account, err := accountHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	sensitive, err := os.MkdirTemp(account, ".switchboard-home-alias-target-")
	if err != nil {
		t.Skipf("cannot stage account-home alias target: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sensitive) })
	readAlias := filepath.Join(workspace, ".asdf")
	writeAlias := filepath.Join(workspace, "go")
	if err := os.Symlink(sensitive, readAlias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sensitive, writeAlias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", workspace)

	argv, err := wrapBubblewrap("/usr/bin/bwrap", Policy{
		Workspace: workspace,
		Network:   NetworkLoopback,
	}, []string{"/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	if !containsArgvSequence(argv, []string{"--tmpfs", account}) {
		t.Fatalf("wrapper did not hide canonical account home %q: %q", account, argv)
	}
	if !containsArgvSequence(argv, []string{"--tmpfs", workspace}) {
		t.Fatalf("wrapper did not close distinct ambient home %q: %q", workspace, argv)
	}
	if !containsArgvSequence(argv, []string{"--bind", workspace, workspace}) {
		t.Fatalf("wrapper did not reopen the selected workspace %q: %q", workspace, argv)
	}
	if containsArgvSequence(argv, []string{"--remount-ro", workspace}) {
		t.Fatalf("wrapper made the selected workspace read-only: %q", argv)
	}
	if containsArgvSequence(argv, []string{"--ro-bind", readAlias, readAlias}) {
		t.Fatalf("wrapper reopened checkout symlink alias %q into the account home: %q", readAlias, argv)
	}
	if containsArgvSequence(argv, []string{"--bind", filepath.Join(workspace, "go", "pkg", "mod"), filepath.Join(workspace, "go", "pkg", "mod")}) {
		t.Fatalf("wrapper persisted a writable cache through checkout symlink %q: %q", writeAlias, argv)
	}
	if _, err := os.Stat(filepath.Join(sensitive, "pkg")); !os.IsNotExist(err) {
		t.Fatalf("constructing the wrapper mutated symlink target: %v", err)
	}
}

func TestPreparingWritableCachesRejectsReplacementFIFOWithoutBlocking(t *testing.T) {
	parent := t.TempDir()
	home := filepath.Join(parent, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(home, home+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(home, 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := prepareWritableHomeCaches(home)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("replacement FIFO was accepted as a cache root")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("opening a replacement FIFO as a cache root blocked")
	}
}

func TestResolvedCredentialTargetIsMaskedAfterExactReopen(t *testing.T) {
	home := t.TempDir()
	credentialDir := filepath.Join(home, ".cargo")
	goRoot := filepath.Join(home, ".asdf", "go")
	if err := os.MkdirAll(credentialDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(goRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(goRoot, "credential")
	if err := os.WriteFile(target, []byte(canaryToken), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(credentialDir, "credentials")); err != nil {
		t.Fatal(err)
	}

	flags, err := linuxResolvedSecretFlags(home)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--ro-bind", os.DevNull, target}
	if !containsArgvSequence(flags, want) {
		t.Fatalf("resolved target flags %q omit final mask %q", flags, want)
	}

	argv := append([]string{"--ro-bind", goRoot, goRoot}, flags...)
	reopenAt, maskAt := -1, -1
	for i := range argv {
		if i+2 < len(argv) && slices.Equal(argv[i:i+3], []string{"--ro-bind", goRoot, goRoot}) {
			reopenAt = i
		}
		if i+2 < len(argv) && slices.Equal(argv[i:i+3], want) {
			maskAt = i
		}
	}
	if reopenAt < 0 || maskAt <= reopenAt {
		t.Fatalf("credential mask must follow exact root reopen: argv=%q", argv)
	}
}

func containsArgvSequence(argv, want []string) bool {
	for i := 0; i+len(want) <= len(argv); i++ {
		if slices.Equal(argv[i:i+len(want)], want) {
			return true
		}
	}
	return false
}
