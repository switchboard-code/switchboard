package credential

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/safeexec"
)

func TestBrowserRefusesWorkspacePATHShadow(t *testing.T) {
	workspace := t.TempDir()
	marker := filepath.Join(workspace, "opened")
	name := "switchboard-test-xdg-open"
	writeExecutable(t, filepath.Join(workspace, name), "#!/bin/sh\nprintf ran > '"+marker+"'\n")
	t.Chdir(workspace)
	t.Setenv("PATH", workspace)

	if _, err := linuxBrowserCommand("https://example.invalid", name); !errors.Is(err, safeexec.ErrUntrustedPath) {
		t.Fatalf("error = %v, want refusal of workspace PATH shadow", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace opener ran despite refusal: %v", err)
	}
}

func TestBrowserAllowsExternalAbsolutePATHAndScrubsEnvironment(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	name := "switchboard-test-xdg-open"
	path := filepath.Join(external, name)
	writeExecutable(t, path, "#!/bin/sh\nexit 0\n")
	t.Chdir(workspace)
	t.Setenv("PATH", external+string(os.PathListSeparator)+workspace+string(os.PathListSeparator)+".")
	t.Setenv("BROWSER", filepath.Join(workspace, "browser-shadow"))
	t.Setenv("SB_BROWSER_TOKEN", value)

	cmd, err := linuxBrowserCommand("https://example.invalid", name)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Path != path {
		t.Fatalf("browser opener = %q, want %q", cmd.Path, path)
	}
	for _, entry := range cmd.Env {
		if strings.Contains(entry, value) || strings.HasPrefix(entry, "SB_BROWSER_TOKEN=") {
			t.Fatalf("credential-bearing environment reached browser opener: %q", entry)
		}
		if strings.HasPrefix(entry, "BROWSER=") || strings.HasPrefix(entry, "DEFAULT_BROWSER=") {
			t.Fatalf("dispatcher override reached browser opener: %q", entry)
		}
		if strings.HasPrefix(entry, "PATH=") && strings.Contains(entry, workspace) {
			t.Fatalf("workspace PATH entry reached browser opener: %q", entry)
		}
	}
}

func TestBrowserDropsWorkspaceDesktopHandlerAuthority(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	dataHome := filepath.Join(workspace, "xdg-data")
	applications := filepath.Join(dataHome, "applications")
	safeData := filepath.Join(external, "share")
	if err := os.MkdirAll(applications, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(safeData, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(applications, "switchboard-test.desktop"), []byte("[Desktop Entry]\nExec=/workspace/evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(external, "workspace-desktop-handler-ran")
	name := "switchboard-test-xdg-open"
	// This is the relevant xdg-open dispatch seam: a readable .desktop file
	// under XDG_DATA_HOME selects executable handler authority.
	writeExecutable(t, filepath.Join(external, name), "#!/bin/sh\n[ -n \"$XDG_DATA_HOME\" ] && [ -r \"$XDG_DATA_HOME/applications/switchboard-test.desktop\" ] && /usr/bin/touch '"+marker+"'\n")
	t.Chdir(workspace)
	t.Setenv("PATH", external)
	t.Setenv("HOME", workspace)
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(workspace, "xdg-config"))
	t.Setenv("XDG_DATA_DIRS", dataHome+string(os.PathListSeparator)+safeData)
	t.Setenv("XDG_CONFIG_DIRS", filepath.Join(workspace, "xdg-config"))
	t.Setenv("DISPLAY", ":77")
	t.Setenv("WAYLAND_DISPLAY", filepath.Join(workspace, "wayland-0"))
	t.Setenv("WAYLAND_SOCKET", "9")

	cmd, err := linuxBrowserCommand("https://example.invalid", name)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace .desktop handler received browser dispatch: %v", err)
	}
	for _, entry := range cmd.Env {
		if strings.Contains(entry, workspace) {
			t.Fatalf("workspace desktop-handler authority reached xdg-open: %q", entry)
		}
		for _, prefix := range []string{"DISPLAY=", "WAYLAND_DISPLAY=", "WAYLAND_SOCKET="} {
			if strings.HasPrefix(entry, prefix) {
				t.Fatalf("unbound display authority reached xdg-open: %q", entry)
			}
		}
	}
}

func TestBrowserDropsUnboundSessionBusAddresses(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	name := "switchboard-test-xdg-open"
	writeExecutable(t, filepath.Join(external, name), "#!/bin/sh\nexit 0\n")
	t.Chdir(workspace)
	t.Setenv("PATH", external)

	workspaceSocket := filepath.Join(workspace, "b")
	listener, err := net.Listen("unix", workspaceSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	for _, address := range []string{
		"unix:abstract=/switchboard-test-bus",
		"unix:path=" + workspaceSocket,
	} {
		t.Run(address, func(t *testing.T) {
			t.Setenv("DBUS_SESSION_BUS_ADDRESS", address)
			cmd, err := linuxBrowserCommand("https://example.invalid", name)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range cmd.Env {
				if strings.HasPrefix(entry, "DBUS_SESSION_BUS_ADDRESS=") {
					t.Fatalf("unbound session bus reached browser opener: %q", entry)
				}
			}
		})
	}
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
