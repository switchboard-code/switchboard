package credential

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSecretTool stands in for the system command and records exactly what it
// was invoked with.
func fakeSecretTool(t *testing.T) (store *OSStore, argvLog string) {
	t.Helper()
	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv")
	vault := filepath.Join(dir, "vault")

	// lookup writes the secret with no trailing newline, and is silent on a
	// miss: that silence is how a miss is told from a broken bus, so the fake
	// has to reproduce it exactly.
	body := `#!/bin/sh
printf '%s\n' "$*" >> ` + argvLog + `
case "$1" in
  store)  cat > ` + vault + ` ;;
  lookup) [ -f ` + vault + ` ] || exit 1
          cat ` + vault + ` ;;
  clear)  rm -f ` + vault + ` ;;
esac
`
	path := filepath.Join(dir, "secret-tool")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/fake/for/this/test")
	return &OSStore{bin: path}, argvLog
}

func TestSecretServiceGetCancellationStopsPipeHoldingDescendant(t *testing.T) {
	helper, pidPath := pipeHoldingCredentialHelper(t, 1)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/fake/for/this/test")
	store := &OSStore{bin: helper}
	ctx, cancel := context.WithTimeout(context.Background(), credentialHelperCancelTimeout)
	defer cancel()

	started := time.Now()
	_, err := store.Get(ctx, Ref{Provider: "anthropic"})
	assertCredentialHelperCanceled(t, pidPath, started, err)
}

func TestSecretServiceWithholdsOversizedCredentialOutput(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/fake/for/this/test")
	store := &OSStore{bin: overflowingCredentialHelper(t)}
	_, err := store.Get(context.Background(), Ref{Provider: "anthropic"})
	if err == nil || !strings.Contains(err.Error(), "output withheld") || strings.Contains(err.Error(), strings.Repeat("s", 100)) {
		t.Fatalf("oversized keyring output was not withheld: %v", err)
	}
}

// The command line of every process is readable by every user on the machine.
func TestKeyringKeepsTheSecretOutOfArgv(t *testing.T) {
	store, argvLog := fakeSecretTool(t)
	ctx := context.Background()
	ref := Ref{Provider: "anthropic", Account: "first-party"}

	if err := store.Set(ctx, ref, value); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.Expose() != value {
		t.Errorf("read back %q", got.Expose())
	}

	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(argv), value) {
		t.Errorf("the credential appeared on a command line, where any user on the machine can read it:\n%s", argv)
	}
}

func TestKeyringRefusesWorkspacePATHShadowWithoutSendingSecret(t *testing.T) {
	workspace := t.TempDir()
	bin := filepath.Join(workspace, "bin")
	subdir := filepath.Join(workspace, "nested", "package")
	if err := os.MkdirAll(filepath.Join(workspace, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(workspace, "received-secret")
	name := "switchboard-test-secret-tool"
	writeExecutable(t, filepath.Join(bin, name), "#!/bin/sh\n/bin/cat > '"+marker+"'\n")
	t.Chdir(subdir)
	t.Setenv("PATH", bin)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/fake/for/this/test")

	store := newSecretServiceStore(name)
	err := store.Set(context.Background(), Ref{Provider: "anthropic"}, value)
	var unavailable *Unavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("error = %v, want an unavailable safe helper", err)
	}
	if err != nil && strings.Contains(err.Error(), value) {
		t.Fatal("the refusal rendered the credential")
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("workspace secret-tool received credential input: %v", statErr)
	}
}

func TestKeyringNestedVCSMarkerCannotHideOuterRepositoryPATHShadow(t *testing.T) {
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

			name := "switchboard-test-secret-tool"
			received := filepath.Join(repository, "received-secret")
			writeExecutable(t, filepath.Join(bin, name), "#!/bin/sh\n/bin/cat > '"+received+"'\n")
			t.Chdir(cwd)
			t.Setenv("PATH", bin)
			setSafeTestSessionBus(t)

			store := newSecretServiceStore(name)
			err := store.Set(context.Background(), Ref{Provider: "anthropic"}, value)
			var unavailable *Unavailable
			if !errors.As(err, &unavailable) {
				t.Fatalf("nested %s marker hid outer repository: error = %v, want unavailable safe helper", nestedMarker, err)
			}
			if strings.Contains(err.Error(), value) {
				t.Fatal("the helper refusal rendered the credential")
			}
			if _, statErr := os.Stat(received); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("outer-repository PATH shadow received credential input: %v", statErr)
			}
		})
	}
}

func TestKeyringSymlinkSpelledPWDCannotHideOuterRepositoryPATHShadow(t *testing.T) {
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

	name := "switchboard-test-secret-tool"
	received := filepath.Join(repository, "received-secret")
	writeExecutable(t, filepath.Join(bin, name), "#!/bin/sh\n/bin/cat > '"+received+"'\n")
	t.Chdir(alias)
	// os.Getwd may deliberately retain an absolute PWD spelling when it names
	// the same directory. The authority walk must use the physical cwd, not
	// this attacker-controlled namespace alias.
	t.Setenv("PWD", alias)
	t.Setenv("PATH", bin)
	setSafeTestSessionBus(t)

	store := newSecretServiceStore(name)
	err := store.Set(context.Background(), Ref{Provider: "anthropic"}, value)
	var unavailable *Unavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("symlink-spelled PWD hid outer repository: error = %v, want unavailable safe helper", err)
	}
	if strings.Contains(err.Error(), value) {
		t.Fatal("the helper refusal rendered the credential")
	}
	if _, statErr := os.Stat(received); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("outer-repository PATH shadow received credential input: %v", statErr)
	}
}

func TestKeyringRefusesWorkspaceSymlinkPATHShadowWithoutSendingSecret(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	marker := filepath.Join(external, "received-secret")
	name := "switchboard-test-secret-tool"
	attacker := filepath.Join(external, "attacker")
	writeExecutable(t, attacker, "#!/bin/sh\n/bin/cat > '"+marker+"'\n")
	if err := os.Symlink(attacker, filepath.Join(workspace, name)); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workspace)
	t.Setenv("PATH", workspace)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/fake/for/this/test")

	store := newSecretServiceStore(name)
	err := store.Set(context.Background(), Ref{Provider: "anthropic"}, value)
	var unavailable *Unavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("error = %v, want refusal of workspace symlink helper", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("symlinked attacker received credential input: %v", statErr)
	}
}

func TestKeyringKeepsResolvedExternalHelperWhenPATHChanges(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	name := "switchboard-test-secret-tool"
	trustedMarker := filepath.Join(external, "trusted-ran")
	shadowMarker := filepath.Join(workspace, "shadow-ran")
	writeExecutable(t, filepath.Join(external, name), "#!/bin/sh\n/bin/cat > '"+trustedMarker+"'\n")
	writeExecutable(t, filepath.Join(workspace, name), "#!/bin/sh\n/bin/cat > '"+shadowMarker+"'\n")
	t.Chdir(workspace)
	t.Setenv("PATH", external)
	setSafeTestSessionBus(t)
	store := newSecretServiceStore(name)

	// Resolution belongs to the store, not to each exec call. A later PATH
	// mutation cannot redirect the credential to the checkout.
	t.Setenv("PATH", workspace)
	t.Setenv("BROWSER", filepath.Join(workspace, "browser-shadow"))
	t.Setenv("SB_KEYRING_TOKEN", value)
	cmd, err := store.command(context.Background(), "store")
	if err != nil {
		t.Fatal(err)
	}
	foundSessionBus := false
	for _, entry := range cmd.Env {
		if strings.Contains(entry, value) || strings.HasPrefix(entry, "SB_KEYRING_TOKEN=") {
			t.Fatalf("credential-bearing environment reached keyring helper: %q", entry)
		}
		if strings.HasPrefix(entry, "BROWSER=") || strings.HasPrefix(entry, "DEFAULT_BROWSER=") {
			t.Fatalf("dispatcher override reached keyring helper: %q", entry)
		}
		if strings.HasPrefix(entry, "PATH=") && strings.Contains(entry, workspace) {
			t.Fatalf("workspace PATH entry reached keyring helper: %q", entry)
		}
		if strings.HasPrefix(entry, "DBUS_SESSION_BUS_ADDRESS=unix:path=") {
			foundSessionBus = true
		}
	}
	if !foundSessionBus {
		t.Fatal("validated owner-private session bus was not passed to keyring helper")
	}
	if err := store.Set(context.Background(), Ref{Provider: "anthropic"}, value); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(trustedMarker); err != nil {
		t.Fatalf("resolved external helper did not run: %v", err)
	}
	if _, err := os.Stat(shadowMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("later PATH shadow ran: %v", err)
	}
}

func TestKeyringDropsWorkspaceGIOModuleLoadersBeforeSendingSecret(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	name := "switchboard-test-secret-tool"
	moduleDir := filepath.Join(workspace, "gio-modules")
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "evil.so"), []byte("marker"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(external, "gio-module-loaded")
	writeExecutable(t, filepath.Join(external, name), "#!/bin/sh\nif { [ -n \"$GIO_EXTRA_MODULES\" ] && [ -e \"$GIO_EXTRA_MODULES/evil.so\" ]; } || { [ -n \"$GIO_MODULE_DIR\" ] && [ -e \"$GIO_MODULE_DIR/evil.so\" ]; }; then /usr/bin/touch '"+marker+"'; fi\n/bin/cat >/dev/null\n")
	t.Chdir(workspace)
	t.Setenv("PATH", external)
	t.Setenv("GIO_EXTRA_MODULES", moduleDir)
	t.Setenv("GIO_MODULE_DIR", moduleDir)
	setSafeTestSessionBus(t)

	store := newSecretServiceStore(name)
	if err := store.Set(context.Background(), Ref{Provider: "anthropic"}, value); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace GIO module authority reached secret-tool: %v", err)
	}
}

func TestKeyringRefusesWorkspaceSessionBusWithoutSendingSecret(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	name := "switchboard-test-secret-tool"
	marker := filepath.Join(external, "received-secret")
	writeExecutable(t, filepath.Join(external, name), "#!/bin/sh\n/bin/cat > '"+marker+"'\n")
	if err := os.Chmod(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(workspace, "fake-session-bus")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	t.Chdir(workspace)
	t.Setenv("PATH", external)
	t.Setenv("XDG_RUNTIME_DIR", workspace)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+socketPath)

	store := newSecretServiceStore(name)
	err = store.Set(context.Background(), Ref{Provider: "anthropic"}, value)
	var unavailable *Unavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("error = %v, want refusal of workspace session bus", err)
	}
	if err != nil && strings.Contains(err.Error(), value) {
		t.Fatal("the session-bus refusal rendered the credential")
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("helper received credential despite workspace session bus: %v", statErr)
	}
}

func TestKeyringRefusesAbstractSessionBus(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:abstract=switchboard-test")
	if _, err := validatedSessionBusAddress([]string{t.TempDir()}); err == nil {
		t.Fatal("abstract session bus was accepted for credential storage")
	}
}

func TestKeyringRefusesResolvedHelperReplacementWithoutSendingSecret(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	name := "switchboard-test-secret-tool"
	path := filepath.Join(external, name)
	marker := filepath.Join(external, "replacement-received")
	writeExecutable(t, path, "#!/bin/sh\nexit 0\n")
	t.Chdir(workspace)
	t.Setenv("PATH", external)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/fake/for/this/test")
	store := newSecretServiceStore(name)

	replacement := filepath.Join(external, "replacement")
	writeExecutable(t, replacement, "#!/bin/sh\n/bin/cat > '"+marker+"'\n")
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	err := store.Set(context.Background(), Ref{Provider: "anthropic"}, value)
	var unavailable *Unavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("error = %v, want refusal after executable replacement", err)
	}
	if err != nil && strings.Contains(err.Error(), value) {
		t.Fatal("the replacement refusal rendered the credential")
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("replacement executable received credential input: %v", statErr)
	}
}

func TestKeyringMissIsNotAFailure(t *testing.T) {
	store, _ := fakeSecretTool(t)

	_, err := store.Get(context.Background(), Ref{Provider: "anthropic"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want a miss so the resolver moves on", err)
	}
}

// clear succeeds whether or not anything matched. Reporting a removal that did
// not happen would let a user believe a credential is gone when it is stored
// under another name and still authenticating.
func TestKeyringDeletingWhatIsNotThereReportsAMiss(t *testing.T) {
	store, _ := fakeSecretTool(t)

	err := store.Delete(context.Background(), Ref{Provider: "anthropic"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want a miss rather than a false success", err)
	}
}

// A machine with no session bus has no keyring. That is a fact about the
// machine, not a failure of the lookup, and the resolver has to be able to
// carry on to the sources that work headlessly.
func TestNoSessionBusIsUnavailableNotAnError(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	store := NewOSStore()

	_, err := store.Get(context.Background(), Ref{Provider: "anthropic"})
	var un *Unavailable
	if !errors.As(err, &un) {
		t.Fatalf("err = %v, want an Unavailable", err)
	}

	resolver := NewResolver(&EnvStore{lookup: envOf(map[string]string{"SB_ANTHROPIC_API_KEY": "headless"})}, store)
	got, resolveErr := resolver.Get(context.Background(), Ref{Provider: "anthropic"})
	if resolveErr != nil {
		t.Fatalf("a missing keyring stopped the chain: %v", resolveErr)
	}
	if got.Expose() != "headless" {
		t.Errorf("resolved %q", got.Expose())
	}
}

func setSafeTestSessionBus(t *testing.T) {
	t.Helper()
	runtimeDir := t.TempDir()
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(runtimeDir, "bus")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+socketPath)
}

func requireLiveKeyring(t *testing.T) {
	t.Helper()
	if os.Getenv("SB_LIVE") == "" {
		t.Skip("set SB_LIVE=1 to exercise a real Secret Service")
	}
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		t.Skip("no session bus, so there is no keyring to test against")
	}
}

func TestLiveKeyringRoundTrip(t *testing.T) {
	requireLiveKeyring(t)

	ctx := context.Background()
	store := NewOSStore()
	ref := Ref{Provider: "sb-selftest", Account: "round-trip"}

	_ = store.Delete(ctx, ref)
	if _, err := store.Get(ctx, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the test item exists before the test wrote it: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, ref) })

	if err := store.Set(ctx, ref, value); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.Expose() != value {
		t.Errorf("read back %q, want the value that was stored", got.Expose())
	}

	const rotated = "sk-rotated-9876543210"
	if err := store.Set(ctx, ref, rotated); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Get(ctx, ref); err != nil {
		t.Fatal(err)
	} else if got.Expose() != rotated {
		t.Errorf("after rotation the store returned %q; a store that keeps the old value "+
			"authenticates with a key the user has already replaced", got.Expose())
	}

	if err := store.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, ref); !errors.Is(err, ErrNotFound) {
		t.Errorf("after deletion, err = %v, want a miss", err)
	}
}
