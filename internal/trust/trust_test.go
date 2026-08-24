package trust

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
)

func openStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	s, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func TestGrantPersistsAcrossReopen(t *testing.T) {
	s, path := openStore(t)
	ws := t.TempDir()

	if s.Trusted(ws) {
		t.Fatal("a fresh store must trust nothing")
	}
	if err := s.Grant(ws); err != nil {
		t.Fatal(err)
	}
	if !s.Trusted(ws) {
		t.Fatal("grant did not take")
	}

	reopened, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Trusted(ws) {
		t.Error("grant did not survive a reopen")
	}
}

func TestRevokeRemovesTheGrant(t *testing.T) {
	s, path := openStore(t)
	ws := t.TempDir()

	if err := s.Grant(ws); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ws); err != nil {
		t.Fatal(err)
	}
	if s.Trusted(ws) {
		t.Error("revoke did not take")
	}

	reopened, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Trusted(ws) {
		t.Error("revoke did not survive a reopen")
	}
}

func TestTrustFollowsTheResolvedPath(t *testing.T) {
	s, _ := openStore(t)
	ws := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(ws, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := s.Grant(ws); err != nil {
		t.Fatal(err)
	}
	// A session opened through a symlink is the same checkout, and a grant
	// keyed on the unresolved spelling would make trust depend on how the
	// directory was typed.
	if !s.Trusted(link) {
		t.Error("a symlink to a trusted workspace must be trusted")
	}
}

func TestMissingWorkspaceIsNeverTrusted(t *testing.T) {
	s, _ := openStore(t)
	if s.Trusted(filepath.Join(t.TempDir(), "does-not-exist")) {
		t.Error("a path that cannot resolve must not be trusted")
	}
}

func TestFileCarriesTheHeaderAndTightPermissions(t *testing.T) {
	s, path := openStore(t)
	if err := s.Grant(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "# Workspaces trusted") {
		t.Error("the file must explain itself; it gets regenerated")
	}
	f, err := fileprivacy.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ownerOnly, ownerErr := fileprivacy.IsOwnerOnly(f)
	closeErr := f.Close()
	if ownerErr != nil || closeErr != nil || !ownerOnly {
		t.Errorf("trust store owner-only=%v, check=%v, close=%v", ownerOnly, ownerErr, closeErr)
	}
}

func TestGrantedListsSorted(t *testing.T) {
	s, _ := openStore(t)
	a, b := t.TempDir(), t.TempDir()
	if err := s.Grant(b); err != nil {
		t.Fatal(err)
	}
	if err := s.Grant(a); err != nil {
		t.Fatal(err)
	}
	got := s.Granted()
	if len(got) != 2 {
		t.Fatalf("Granted() = %v, want 2 entries", got)
	}
	if got[0] > got[1] {
		t.Errorf("Granted() not sorted: %v", got)
	}
}

func TestOpenRejectsUnsafeExistingTrustStores(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Run("loose-permissions", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), FileName)
			if err := os.WriteFile(path, []byte("[workspaces]\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenFile(path); err == nil || !strings.Contains(err.Error(), "permissions") {
				t.Fatalf("OpenFile loose permissions = %v", err)
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target.toml")
		writePrivateTrustFile(t, target, []byte("[workspaces]\n"))
		path := filepath.Join(dir, FileName)
		if err := os.Symlink(target, path); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := OpenFile(path); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("OpenFile symlink = %v", err)
		}
	})
	t.Run("oversized", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), FileName)
		writePrivateTrustFile(t, path, []byte("#"+strings.Repeat("x", maxTrustFileBytes)))
		if _, err := OpenFile(path); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("OpenFile oversized = %v", err)
		}
	})
}

func writePrivateTrustFile(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := fileprivacy.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNilStoreIsNeverTrusted(t *testing.T) {
	var store *Store
	if store.Trusted(t.TempDir()) {
		t.Fatal("nil store trusted a workspace")
	}
}
