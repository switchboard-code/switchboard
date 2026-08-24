//go:build unix

package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUnixSessionArtifactsRemainOwnerOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	assertUnixSessionMode(t, root, 0o700)

	workspace := t.TempDir()
	dir, err := store.WorkspaceDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	assertUnixSessionMode(t, dir, 0o700)

	sess, err := store.CreateStaged(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	path := sess.Path()
	assertUnixSessionMode(t, path, 0o600)
	if outcome, err := sess.PublishDurably(); err != nil || !outcome.Visible || !outcome.Durable {
		_ = sess.Close()
		t.Fatalf("PublishDurably() = %+v, %v", outcome, err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	assertUnixSessionMode(t, publicationMarkerPath(path), 0o600)

	temp, err := createPrivateSessionTempDir(dir, ".session-remove-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(temp)
	assertUnixSessionMode(t, temp, 0o700)
}

func TestUnixSessionStoreRefusesSymlinkedDirectory(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "sessions")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(link); err == nil {
		t.Fatal("NewStore accepted a symlinked session root")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("refused symlink changed target mode to %04o", got)
	}
}

func assertUnixSessionMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %04o, want %04o", path, got, want)
	}
}
