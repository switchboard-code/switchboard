//go:build aix || android || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package extensions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestInstallRejectsSpecialSourceAndGitFiles(t *testing.T) {
	for _, relative := range []string{"fifo", ".git"} {
		t.Run(strings.ReplaceAll(relative, ".", "git"), func(t *testing.T) {
			root := makePlugin(t, DialectCodex, `{"name":"special"}`)
			plugin := discoverInstallPlugin(t, root, ScopeUser, DialectCodex)
			if err := unix.Mkfifo(filepath.Join(root, relative), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Install(plugin, t.TempDir()); err == nil || !strings.Contains(err.Error(), "special file") {
				t.Fatalf("Install() error = %v, want special-file rejection", err)
			}
		})
	}
}

func TestInstallNeverOverwritesSpecialDestination(t *testing.T) {
	root := makePlugin(t, DialectCodex, `{"name":"special-destination"}`)
	plugin := discoverInstallPlugin(t, root, ScopeUser, DialectCodex)
	cacheRoot := t.TempDir()
	destination := filepath.Join(canonicalInstallPath(t, cacheRoot), filepath.FromSlash(installDestination(plugin)))
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(destination, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(plugin, cacheRoot); err == nil {
		t.Fatal("Install accepted a special destination")
	}
	info, err := os.Lstat(destination)
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("special destination changed: %v %#v", err, info)
	}
}
