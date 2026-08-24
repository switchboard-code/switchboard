//go:build darwin

package fileprivacy

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDarwinSecureStripsExtendedACLFromFileAndDirectory(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "private")
	f, err := Create(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	addDarwinACL(t, filePath, "everyone allow read")
	info, err := os.Stat(filePath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("ACL precondition mode=%v err=%v; the old mode-only check would not have accepted it", info, err)
	}
	f, err = os.OpenFile(filePath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ownerOnly, err := IsOwnerOnly(f); err != nil || ownerOnly {
		_ = f.Close()
		t.Fatalf("extended-ACL file owner-only=%v err=%v", ownerOnly, err)
	}
	if err := Secure(f); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if ownerOnly, err := IsOwnerOnly(f); err != nil || !ownerOnly {
		_ = f.Close()
		t.Fatalf("secured file owner-only=%v err=%v", ownerOnly, err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	dirPath := filepath.Join(root, "private-dir")
	if err := EnsurePrivateDir(dirPath); err != nil {
		t.Fatal(err)
	}
	addDarwinACL(t, dirPath, "everyone allow list,search")
	fd, err := unix.Open(dirPath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	dir := os.NewFile(uintptr(fd), dirPath)
	if dir == nil {
		_ = unix.Close(fd)
		t.Fatal("converting directory descriptor")
	}
	if ownerOnly, err := DirectoryIsOwnerOnly(dir); err != nil || ownerOnly {
		_ = dir.Close()
		t.Fatalf("extended-ACL directory owner-only=%v err=%v", ownerOnly, err)
	}
	if err := SecureDirectory(dir); err != nil {
		_ = dir.Close()
		t.Fatal(err)
	}
	if ownerOnly, err := DirectoryIsOwnerOnly(dir); err != nil || !ownerOnly {
		_ = dir.Close()
		t.Fatalf("secured directory owner-only=%v err=%v", ownerOnly, err)
	}
	if err := dir.Close(); err != nil {
		t.Fatal(err)
	}
}

func addDarwinACL(t *testing.T, path, rule string) {
	t.Helper()
	if output, err := exec.Command("chmod", "+a", rule, path).CombinedOutput(); err != nil {
		t.Fatalf("adding Darwin ACL: %v: %s", err, output)
	}
}
