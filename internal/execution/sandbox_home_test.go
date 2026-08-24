//go:build darwin || linux

package execution

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The home directory is closed by default rather than by enumeration. This is
// the property the deny-list posture could not provide: a survey of one
// ordinary machine found 51 top-level entries in home, of which a hand-written
// deny list covered six, leaving an npm auth token, shell history, and five
// CLI tools' credential directories readable by any confined command.
//
// Each name below is one that no list mentions.
func TestUnlistedHomeFilesAreNotReadable(t *testing.T) {
	ws := workspaceFor(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	names := []string{
		".npmrc",                   // registry auth tokens
		".sb-test-unlisted-secret", // a name nothing could have anticipated
		".config/sb-test-tool/credentials",
	}

	for _, name := range names {
		path := filepath.Join(home, filepath.FromSlash(name))
		if _, err := os.Lstat(path); err == nil {
			// Never write over something the user already has.
			res := runConfined(t, ws, NetworkLoopback, []string{"/bin/cat", path}, false)
			if res.ExitCode == 0 && len(res.Output) > 0 {
				t.Errorf("%s is readable from inside the sandbox", name)
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Skipf("cannot stage %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(canaryToken), 0o600); err != nil {
			t.Skipf("cannot stage %s: %v", name, err)
		}
		t.Cleanup(func() { os.Remove(path) })

		res := runConfined(t, ws, NetworkLoopback, []string{"/bin/cat", path}, false)
		if res.ExitCode == 0 {
			t.Errorf("reading %s from inside the sandbox succeeded", name)
		}
		if strings.Contains(res.Output, canaryToken) {
			t.Errorf("%s leaked its contents into the sandbox", name)
		}
	}
}

// A build cache and the workspace have to stay reachable, or the closure is
// just a broken sandbox rather than a tighter one.
func TestAllowlistedHomePathsStillWork(t *testing.T) {
	ws := workspaceFor(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	cache := filepath.Join(home, ".cache", "sb-test-readback")
	if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
		t.Skipf("cannot stage a cache file: %v", err)
	}
	if err := os.WriteFile(cache, []byte("cache-contents"), 0o600); err != nil {
		t.Skipf("cannot stage a cache file: %v", err)
	}
	defer os.Remove(cache)

	if res := runConfined(t, ws, NetworkLoopback, []string{"/bin/cat", cache}, false); res.ExitCode != 0 {
		t.Errorf("the build cache is unreadable from inside the sandbox: %s", res.Output)
	}

	probe := filepath.Join(ws, "in-workspace")
	if err := os.WriteFile(probe, []byte("workspace-contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := runConfined(t, ws, NetworkLoopback, []string{"/bin/cat", probe}, false); res.ExitCode != 0 {
		t.Errorf("the workspace is unreadable from inside the sandbox: %s", res.Output)
	}
}

func TestNestedCredentialSymlinkIsNotReopened(t *testing.T) {
	home, err := accountHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	cargo := filepath.Join(home, ".cargo")
	createdCargo := false
	if info, err := os.Lstat(cargo); errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(cargo, 0o700); err != nil {
			t.Skipf("cannot stage .cargo under account home: %v", err)
		}
		createdCargo = true
	} else if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Skipf("account .cargo is not a real directory: %v", err)
	}
	if createdCargo {
		t.Cleanup(func() { _ = os.Remove(cargo) })
	}

	var credential string
	for _, name := range []string{"credentials", "credentials.toml"} {
		candidate := filepath.Join(cargo, name)
		if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			credential = candidate
			break
		}
	}
	if credential == "" {
		t.Skip("both cargo credential paths already exist; refusing to replace user state")
	}
	outside := filepath.Join(t.TempDir(), "outside-credential")
	if err := os.WriteFile(outside, []byte(canaryToken), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, credential); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(credential) })

	// Use the account home as the functional home too. This exercises the
	// persistent-cache path where .cargo would otherwise be reopened wholesale.
	t.Setenv("HOME", home)
	workspace := workspaceFor(t)
	res := runConfined(t, workspace, NetworkLoopback, []string{"/bin/cat", credential}, false)
	if res.ExitCode == 0 || strings.Contains(res.Output, canaryToken) {
		t.Fatalf("nested credential symlink escaped the home deny: exit=%d output=%q", res.ExitCode, res.Output)
	}
}
