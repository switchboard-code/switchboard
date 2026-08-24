package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/safeexec"
)

func TestClipboardRefusesWorkspacePATHShadowWithoutSendingText(t *testing.T) {
	workspace := t.TempDir()
	marker := filepath.Join(workspace, "clipboard-received")
	name := "switchboard-test-clipboard"
	if err := os.WriteFile(filepath.Join(workspace, name), []byte("#!/bin/sh\n/bin/cat > '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workspace)
	t.Setenv("PATH", workspace)

	_, err := linuxClipboardCommand(linuxClipboardHelper{name: name})
	if !errors.Is(err, safeexec.ErrUntrustedPath) {
		t.Fatalf("error = %v, want workspace helper refusal", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("workspace clipboard helper received text: %v", statErr)
	}
}

func TestClipboardNestedVCSMarkerCannotHideOuterRepositoryPATHShadow(t *testing.T) {
	for _, nestedMarker := range []string{".hg", ".svn"} {
		t.Run(strings.TrimPrefix(nestedMarker, "."), func(t *testing.T) {
			repository := t.TempDir()
			if err := os.Mkdir(filepath.Join(repository, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			nested := filepath.Join(repository, "nested")
			cwd := filepath.Join(nested, "package")
			bin := filepath.Join(repository, "bin")
			for _, dir := range []string{filepath.Join(nested, nestedMarker), cwd, bin} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			name := "switchboard-test-clipboard"
			received := filepath.Join(repository, "clipboard-received")
			if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n/bin/cat > '"+received+"'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Chdir(cwd)
			t.Setenv("PATH", bin)

			cmd, err := linuxClipboardCommand(linuxClipboardHelper{name: name})
			if err == nil {
				cmd.Stdin = strings.NewReader("private transcript bytes")
				_ = cmd.Run()
			}
			if _, statErr := os.Stat(received); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("outer-repository clipboard helper received transcript bytes: %v", statErr)
			}
			if !errors.Is(err, safeexec.ErrUntrustedPath) {
				t.Fatalf("nested %s marker helper error = %v, want workspace-authority refusal", nestedMarker, err)
			}
		})
	}
}

func TestClipboardSymlinkSpelledPWDCannotHideOuterRepositoryPATHShadow(t *testing.T) {
	repository := t.TempDir()
	if err := os.Mkdir(filepath.Join(repository, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(repository, "nested", "package")
	bin := filepath.Join(repository, "bin")
	for _, dir := range []string{cwd, bin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(t.TempDir(), "workspace-alias")
	if err := os.Symlink(cwd, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	name := "switchboard-test-clipboard"
	received := filepath.Join(repository, "clipboard-received")
	if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n/bin/cat > '"+received+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(alias)
	t.Setenv("PWD", alias)
	t.Setenv("PATH", bin)

	cmd, err := linuxClipboardCommand(linuxClipboardHelper{name: name})
	if err == nil {
		cmd.Stdin = strings.NewReader("private transcript bytes")
		_ = cmd.Run()
	}
	if _, statErr := os.Stat(received); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("outer-repository clipboard helper received transcript bytes: %v", statErr)
	}
	if !errors.Is(err, safeexec.ErrUntrustedPath) {
		t.Fatalf("symlink-spelled PWD helper error = %v, want workspace-authority refusal", err)
	}
}

func TestClipboardRefusesWorkspaceWaylandSocketWithoutSendingText(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	socketPath := filepath.Join(workspace, "wayland-0")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	marker := filepath.Join(external, "clipboard-received")
	helperPath := filepath.Join(external, "wl-copy")
	if err := os.WriteFile(helperPath, []byte("#!/bin/sh\n/bin/cat > '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workspace)
	t.Setenv("PATH", external)
	t.Setenv("XDG_RUNTIME_DIR", workspace)
	t.Setenv("WAYLAND_DISPLAY", socketPath)
	t.Setenv("WAYLAND_SOCKET", "")

	_, err = linuxClipboardCommand(linuxClipboardHelper{name: "wl-copy", preferred: []string{helperPath}})
	if err == nil {
		t.Fatal("workspace Wayland socket was accepted as clipboard authority")
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("workspace Wayland endpoint received clipboard text: %v", statErr)
	}
}

func TestClipboardAcceptsOwnerPrivateExternalWaylandSocket(t *testing.T) {
	workspace := t.TempDir()
	runtimeDir := t.TempDir()
	external := t.TempDir()
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(runtimeDir, "wayland-0")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	helperPath := filepath.Join(external, "wl-copy")
	if err := os.WriteFile(helperPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workspace)
	t.Setenv("PATH", external)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	t.Setenv("WAYLAND_SOCKET", "")

	cmd, err := linuxClipboardCommand(linuxClipboardHelper{name: "wl-copy", preferred: []string{helperPath}})
	if err != nil {
		t.Fatal(err)
	}
	canonicalRuntime, err := filepath.EvalSymlinks(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	canonicalSocket, err := filepath.EvalSymlinks(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Env, "\n")
	if !strings.Contains(joined, "XDG_RUNTIME_DIR="+canonicalRuntime) || !strings.Contains(joined, "WAYLAND_DISPLAY="+canonicalSocket) {
		t.Fatalf("validated Wayland authority was not preserved: %q", cmd.Env)
	}
}

func TestClipboardRefusesWaylandSocketEscapingRuntimeThroughSymlink(t *testing.T) {
	workspace := t.TempDir()
	runtimeDir := t.TempDir()
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(workspace, "wayland-0")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := os.Symlink(workspace, filepath.Join(runtimeDir, "redirect")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("WAYLAND_DISPLAY", filepath.Join("redirect", "wayland-0"))
	t.Setenv("WAYLAND_SOCKET", "")

	if _, err := validatedWaylandClipboardEnvironment([]string{workspace}); err == nil {
		t.Fatal("Wayland socket escaping its runtime through a parent symlink was accepted")
	}
}

func TestClipboardRefusesAmbientWaylandSocketDescriptor(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	helperPath := filepath.Join(external, "wl-copy")
	if err := os.WriteFile(helperPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workspace)
	t.Setenv("PATH", external)
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	t.Setenv("WAYLAND_SOCKET", "9")
	if _, err := linuxClipboardCommand(linuxClipboardHelper{name: "wl-copy", preferred: []string{helperPath}}); err == nil {
		t.Fatal("unbound WAYLAND_SOCKET descriptor was accepted")
	}
}

func TestClipboardRefusesWorkspacePathAsX11Display(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	socketPath := filepath.Join(workspace, "x11")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	helperPath := filepath.Join(external, "xclip")
	if err := os.WriteFile(helperPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workspace)
	t.Setenv("PATH", external)
	t.Setenv("DISPLAY", socketPath)
	if _, err := linuxClipboardCommand(linuxClipboardHelper{name: "xclip", preferred: []string{helperPath}}); err == nil {
		t.Fatal("workspace path was accepted as X11 clipboard authority")
	}
}

func TestClipboardRefusesRemoteX11Display(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	helperPath := filepath.Join(external, "xclip")
	if err := os.WriteFile(helperPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workspace)
	t.Setenv("PATH", external)
	for _, display := range []string{"attacker.example:0", "localhost:10.0", "127.0.0.1:0"} {
		t.Setenv("DISPLAY", display)
		if _, err := linuxClipboardCommand(linuxClipboardHelper{name: "xclip", preferred: []string{helperPath}}); err == nil {
			t.Fatalf("remote X11 display %q was accepted", display)
		}
	}
}

func TestValidatedX11ClipboardDisplayAcceptsBoundLocalSocket(t *testing.T) {
	workspace := t.TempDir()
	socketDirectory := t.TempDir()
	socketPath := filepath.Join(socketDirectory, "X42")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	t.Chdir(workspace)
	for _, want := range []string{":42.0", "unix:42.0"} {
		t.Setenv("DISPLAY", want)
		if display, err := validatedX11ClipboardDisplayAt([]string{workspace}, socketDirectory); err != nil || display != want {
			t.Fatalf("validated display = %q, %v; want %q", display, err, want)
		}
	}
}

func TestClipboardAllowsExternalAbsolutePATHAndScrubsEnvironment(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	name := "switchboard-test-clipboard"
	path := filepath.Join(external, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workspace)
	t.Setenv("PATH", external+string(os.PathListSeparator)+workspace+string(os.PathListSeparator)+".")
	t.Setenv("BROWSER", filepath.Join(workspace, "browser-shadow"))
	t.Setenv("SB_CLIPBOARD_TOKEN", "must-not-reach-helper")

	cmd, err := linuxClipboardCommand(linuxClipboardHelper{name: name})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Path != path {
		t.Fatalf("clipboard helper = %q, want %q", cmd.Path, path)
	}
	for _, entry := range cmd.Env {
		if strings.Contains(entry, "must-not-reach-helper") || strings.HasPrefix(entry, "SB_CLIPBOARD_TOKEN=") {
			t.Fatalf("credential-bearing environment reached clipboard helper: %q", entry)
		}
		if strings.HasPrefix(entry, "BROWSER=") || strings.HasPrefix(entry, "DEFAULT_BROWSER=") {
			t.Fatalf("dispatcher override reached clipboard helper: %q", entry)
		}
		if strings.HasPrefix(entry, "PATH=") && strings.Contains(entry, workspace) {
			t.Fatalf("workspace PATH entry reached clipboard helper: %q", entry)
		}
	}
}
