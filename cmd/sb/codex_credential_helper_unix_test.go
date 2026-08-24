//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/config"
	"golang.org/x/sys/unix"
)

func makeCodexAuthNonPrivateForTest(path string) error {
	return os.Chmod(path, 0o644)
}

func TestUnixCodexCredentialHelperRefusesSymlinkAndHardlink(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(t *testing.T, home, authPath string)
	}{
		{
			name: "symlink",
			prepare: func(t *testing.T, home, authPath string) {
				target := filepath.Join(home, ".codex", "target.json")
				if err := os.Rename(authPath, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("target.json", authPath); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "hardlink",
			prepare: func(t *testing.T, home, authPath string) {
				if err := os.Link(authPath, filepath.Join(home, ".codex", "copy.json")); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			isolateTestHome(t, home)
			authPath := writePrivateCodexAuth(t, home, []byte(`{"tokens":{"access_token":"secret"}}`))
			test.prepare(t, home, authPath)

			var out strings.Builder
			if err := runCredentialHelperDispatch(&out, []string{codexCredentialHelperKind}); err == nil {
				t.Fatalf("%s Codex login was accepted", test.name)
			}
			if out.Len() != 0 {
				t.Fatalf("%s Codex login wrote stdout: %q", test.name, out.String())
			}
		})
	}
}

func TestUnixCodexCredentialHelperRefusesSymlinkedCodexDirectory(t *testing.T) {
	home := t.TempDir()
	isolateTestHome(t, home)
	realDir := filepath.Join(home, "real-codex")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real-codex", filepath.Join(home, ".codex")); err != nil {
		t.Fatal(err)
	}
	// It need not contain a token: opening the symlinked directory itself must
	// fail before a descendant is considered.
	var out strings.Builder
	if err := runCredentialHelperDispatch(&out, []string{codexCredentialHelperKind}); err == nil {
		t.Fatal("symlinked .codex directory was accepted")
	}
	if out.Len() != 0 {
		t.Fatalf("symlinked .codex directory wrote stdout: %q", out.String())
	}
}

func TestUnixCodexCredentialHelperFIFOIsNonblocking(t *testing.T) {
	home := t.TempDir()
	isolateTestHome(t, home)
	dir := filepath.Join(home, ".codex")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "auth.json"), 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		var out strings.Builder
		done <- runCredentialHelperDispatch(&out, []string{codexCredentialHelperKind})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO Codex login was accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("credential helper blocked opening a FIFO")
	}
	if codexLoginAvailable(&config.Config{}) {
		t.Fatal("FIFO Codex login was advertised by setup")
	}
}
