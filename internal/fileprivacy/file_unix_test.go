//go:build unix

package fileprivacy

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestUnixCreateAndTempAreOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	for _, create := range []func() (*os.File, error){
		func() (*os.File, error) { return Create(filepath.Join(dir, "fixed")) },
		func() (*os.File, error) { return CreateTemp(dir, "private-*") },
	} {
		f, err := create()
		if err != nil {
			t.Fatal(err)
		}
		ownerOnly, ownerErr := IsOwnerOnly(f)
		closeErr := f.Close()
		if ownerErr != nil || closeErr != nil || !ownerOnly {
			t.Fatalf("owner-only=%v ownerErr=%v closeErr=%v", ownerOnly, ownerErr, closeErr)
		}
	}
}

func TestUnixOpenReportsLooseAndHardLinkedFiles(t *testing.T) {
	dir := t.TempDir()
	loose := filepath.Join(dir, "loose")
	if err := os.WriteFile(loose, []byte("state"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Open(loose)
	if err != nil {
		t.Fatal(err)
	}
	ownerOnly, ownerErr := IsOwnerOnly(f)
	closeErr := f.Close()
	if ownerErr != nil || closeErr != nil || ownerOnly {
		t.Fatalf("loose owner-only=%v ownerErr=%v closeErr=%v", ownerOnly, ownerErr, closeErr)
	}

	linked := filepath.Join(dir, "linked")
	if err := os.Link(loose, linked); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(loose); err == nil {
		t.Fatal("hard-linked private file was accepted")
	}
}

func TestUnixOpenWritableRepairsCurrentOwnerFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy")
	if err := os.WriteFile(path, []byte("state"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := OpenWritable(path)
	if err != nil {
		t.Fatal(err)
	}
	owned, ownerErr := IsCurrentUserOwner(f)
	if ownerErr != nil || !owned {
		_ = f.Close()
		t.Fatalf("current-user owner=%v err=%v", owned, ownerErr)
	}
	if err := Secure(f); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	private, privateErr := IsOwnerOnly(f)
	closeErr := f.Close()
	if privateErr != nil || closeErr != nil || !private {
		t.Fatalf("repaired owner-only=%v privateErr=%v closeErr=%v", private, privateErr, closeErr)
	}
}

func TestUnixPrivateDirectoryAndLockFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("private directory info=%v err=%v", info, err)
	}
	path := filepath.Join(dir, "lock")
	first, created, err := OpenReadWriteOrCreate(path)
	if err != nil || !created {
		t.Fatalf("first open created=%v err=%v", created, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, created, err := OpenReadWriteOrCreate(path)
	if err != nil || created {
		t.Fatalf("second open created=%v err=%v", created, err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUnixRootedPrivateLockIsOwnerOnlyAndDoesNotFollowFIFO(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	first, created, err := OpenReadWriteOrCreateInRoot(root, "lock")
	if err != nil || !created {
		t.Fatalf("first rooted open created=%v err=%v", created, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, created, err := OpenReadWriteOrCreateInRoot(root, "lock")
	if err != nil || created {
		t.Fatalf("second rooted open created=%v err=%v", created, err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "lock")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "lock"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		f, _, err := OpenReadWriteOrCreateInRoot(root, "lock")
		if f != nil {
			_ = f.Close()
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("rooted private lock accepted a FIFO")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rooted private lock blocked on a FIFO")
	}
}

func TestUnixRootedPrivateOpenStaysWithRetainedParent(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "private")
	moved := filepath.Join(base, "moved")
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	f, created, err := OpenReadWriteOrCreateInRoot(root, "lock")
	if err != nil || !created {
		t.Fatalf("rooted open after parent move created=%v err=%v", created, err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(moved, "lock")); err != nil {
		t.Fatalf("retained parent did not receive lock: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "lock")); !os.IsNotExist(err) {
		t.Fatalf("replacement parent received lock: %v", err)
	}
}

func TestUnixEnsurePrivateDirInRootReturnsOwnerOnlyCapability(t *testing.T) {
	parent := t.TempDir()
	root, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	child, err := EnsurePrivateDirInRoot(root, "journal")
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	directory, err := child.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	ownerOnly, ownerErr := DirectoryIsOwnerOnly(directory)
	closeErr := directory.Close()
	if ownerErr != nil || closeErr != nil || !ownerOnly {
		t.Fatalf("rooted directory owner-only=%v ownerErr=%v closeErr=%v", ownerOnly, ownerErr, closeErr)
	}
}
