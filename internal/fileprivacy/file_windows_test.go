//go:build windows

package fileprivacy

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsCreateAndTempUseProtectedCurrentUserDACL(t *testing.T) {
	dir := t.TempDir()
	for _, create := range []func() (*os.File, error){
		func() (*os.File, error) { return Create(filepath.Join(dir, "fixed")) },
		func() (*os.File, error) { return CreateTemp(dir, "private-*") },
	} {
		f, err := create()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("private state\n"); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		ownerOnly, ownerErr := IsOwnerOnly(f)
		closeErr := f.Close()
		if ownerErr != nil || closeErr != nil || !ownerOnly {
			t.Fatalf("owner-only=%v ownerErr=%v closeErr=%v", ownerOnly, ownerErr, closeErr)
		}
	}
}

func TestWindowsOwnerOnlyRejectsEveryoneAndHardLinks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "private")
	f, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	world, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
	if err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	dacl, _, err := world.DACL()
	if err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := windows.SetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if ownerOnly, err := IsOwnerOnly(f); err != nil || ownerOnly {
		_ = f.Close()
		t.Fatalf("Everyone DACL owner-only=%v err=%v", ownerOnly, err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	linked := filepath.Join(dir, "linked")
	if err := os.Link(path, linked); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("hard-linked private file was accepted")
	}
}

func TestWindowsPrivateDirectoryAndLockFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	directory, err := openWindowsDirectory(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	ownerOnly, ownerErr := windowsObjectIsOwnerOnly(directory, true)
	closeErr := directory.Close()
	if ownerErr != nil || closeErr != nil || !ownerOnly {
		t.Fatalf("directory owner-only=%v ownerErr=%v closeErr=%v", ownerOnly, ownerErr, closeErr)
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

func TestWindowsRootedPrivateLockUsesProtectedCurrentUserDACL(t *testing.T) {
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
	ownerOnly, ownerErr := IsOwnerOnly(first)
	if ownerErr != nil || !ownerOnly {
		_ = first.Close()
		t.Fatalf("rooted lock owner-only=%v err=%v", ownerOnly, ownerErr)
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
}

func TestWindowsEnsurePrivateDirInRootUsesProtectedCurrentUserDACL(t *testing.T) {
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
	directory, err := openWindowsDirectory(filepath.Join(parent, "journal"), false)
	if err != nil {
		t.Fatal(err)
	}
	ownerOnly, ownerErr := windowsObjectIsOwnerOnly(directory, true)
	closeErr := directory.Close()
	if ownerErr != nil || closeErr != nil || !ownerOnly {
		t.Fatalf("rooted directory owner-only=%v ownerErr=%v closeErr=%v", ownerOnly, ownerErr, closeErr)
	}
}

func TestWindowsOwnerOnlyRequiresCurrentUserAsObjectOwner(t *testing.T) {
	current, err := currentWindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("O:WDD:P(A;;FA;;;" + current.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	if ownerOnly, err := windowsDescriptorIsOwnerOnly(descriptor, current, false); err != nil || ownerOnly {
		t.Fatalf("foreign-owner descriptor owner-only=%v err=%v", ownerOnly, err)
	}
	if owned, err := windowsDescriptorIsCurrentUserOwner(descriptor, current); err != nil || owned {
		t.Fatalf("foreign-owner descriptor current-owned=%v err=%v", owned, err)
	}
}

func TestWindowsOpenWritableRepairsBroadCurrentOwnerFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy")
	f, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	world, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
	if err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	dacl, _, err := world.DACL()
	if err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := windows.SetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	f, err = OpenWritable(path)
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
