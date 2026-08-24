//go:build windows

package fileprivacy

import (
	"errors"
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
	// Existing rooted files must retain the ACL rights promised to callers
	// that revalidate them before use. The old open mask omitted both rights
	// and made this Secure call fail with ERROR_ACCESS_DENIED.
	if err := Secure(second); err != nil {
		_ = second.Close()
		t.Fatalf("securing reopened rooted lock: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsSecureReopensExactFileAcrossPathSubstitution(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "limited-handle")
	moved := filepath.Join(dir, "selected-handle")
	created, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	world, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
	if err != nil {
		_ = created.Close()
		t.Fatal(err)
	}
	dacl, _, err := world.DACL()
	if err != nil {
		_ = created.Close()
		t.Fatal(err)
	}
	if err := windows.SetSecurityInfo(windows.Handle(created.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		_ = created.Close()
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}

	limited, err := openWindowsFile(path,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL,
		nil, windows.OPEN_EXISTING)
	if err != nil {
		t.Fatal(err)
	}
	defer limited.Close()
	if err := os.Rename(path, moved); err != nil {
		t.Fatalf("moving selected file before exact-handle repair: %v", err)
	}
	replacement, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if err := windows.SetSecurityInfo(windows.Handle(replacement.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if err := Secure(limited); err != nil {
		t.Fatalf("securing through a handle without WRITE_DAC: %v", err)
	}
	if private, err := IsOwnerOnly(limited); err != nil || !private {
		t.Fatalf("repaired owner-only=%v err=%v", private, err)
	}
	if private, err := IsOwnerOnly(replacement); err != nil || private {
		t.Fatalf("replacement owner-only=%v err=%v; exact-handle repair touched the path replacement", private, err)
	}
}

func TestWindowsSecureNormalizesLimitedOwnerOnlyACE(t *testing.T) {
	path := filepath.Join(t.TempDir(), "limited-owner-only")
	created, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	current, err := currentWindowsUserSID()
	if err != nil {
		_ = created.Close()
		t.Fatal(err)
	}
	limited, err := windows.SecurityDescriptorFromString("O:" + current.String() +
		"D:P(A;;FR;;;" + current.String() + ")")
	if err != nil {
		_ = created.Close()
		t.Fatal(err)
	}
	dacl, _, err := limited.DACL()
	if err != nil {
		_ = created.Close()
		t.Fatal(err)
	}
	if err := windows.SetSecurityInfo(windows.Handle(created.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		_ = created.Close()
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}

	readOnly, err := openWindowsFile(path, windows.GENERIC_READ|windows.READ_CONTROL, nil, windows.OPEN_EXISTING)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	if err := Secure(readOnly); err != nil {
		t.Fatalf("normalizing limited owner-only ACE: %v", err)
	}
	writable, err := openWindowsFile(path, windows.GENERIC_WRITE|windows.READ_CONTROL, nil, windows.OPEN_EXISTING)
	if err != nil {
		t.Fatalf("normalized DACL did not restore owner write access: %v", err)
	}
	_ = writable.Close()
}

func TestWindowsSecureDirectoryReopensExactHandleAcrossPathSubstitution(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "limited-directory")
	moved := filepath.Join(dir, "selected-directory")
	if err := EnsurePrivateDir(path); err != nil {
		t.Fatal(err)
	}
	writable, err := openWindowsDirectory(path, true)
	if err != nil {
		t.Fatal(err)
	}
	world, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;WD)")
	if err != nil {
		_ = writable.Close()
		t.Fatal(err)
	}
	dacl, _, err := world.DACL()
	if err != nil {
		_ = writable.Close()
		t.Fatal(err)
	}
	if err := windows.SetSecurityInfo(windows.Handle(writable.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		_ = writable.Close()
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}

	limited, err := openWindowsDirectory(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer limited.Close()
	if err := os.Rename(path, moved); err != nil {
		t.Fatalf("moving selected directory before exact-handle repair: %v", err)
	}
	if err := EnsurePrivateDir(path); err != nil {
		t.Fatal(err)
	}
	replacement, err := openWindowsDirectory(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if err := windows.SetSecurityInfo(windows.Handle(replacement.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if err := SecureDirectory(limited); err != nil {
		t.Fatalf("securing directory through a handle without WRITE_DAC: %v", err)
	}
	if private, err := DirectoryIsOwnerOnly(limited); err != nil || !private {
		t.Fatalf("repaired directory owner-only=%v err=%v", private, err)
	}
	if private, err := DirectoryIsOwnerOnly(replacement); err != nil || private {
		t.Fatalf("replacement directory owner-only=%v err=%v; exact-handle repair touched the path replacement", private, err)
	}
}

func TestWindowsCreatePrivateDirInRootPublishesProtectedDACL(t *testing.T) {
	parent := t.TempDir()
	root, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	directory, err := CreatePrivateDirInRoot(root, "private")
	if err != nil {
		t.Fatal(err)
	}
	if private, err := DirectoryIsOwnerOnly(directory); err != nil || !private {
		_ = directory.Close()
		t.Fatalf("created directory owner-only=%v err=%v", private, err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := CreatePrivateDirInRoot(root, "private"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("duplicate create = %v, want os.ErrExist", err)
	}
}

func TestWindowsTokenDefaultOwnerIsRepairAuthorityButNotFinalOwner(t *testing.T) {
	current, err := currentWindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	defaultOwner := differentWindowsOwnerSID(t, current)
	unrelated := differentWindowsOwnerSID(t, current, defaultOwner)

	descriptor, err := windows.SecurityDescriptorFromString("O:" + defaultOwner.String() +
		"D:P(A;;FA;;;" + current.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	if owned, err := windowsDescriptorIsCurrentTokenAuthorityOwner(descriptor, current, defaultOwner); err != nil || !owned {
		t.Fatalf("token-default owner admitted=%v err=%v", owned, err)
	}
	if ownerOnly, err := windowsDescriptorIsOwnerOnly(descriptor, current, false); err != nil || ownerOnly {
		t.Fatalf("token-default owner final owner-only=%v err=%v", ownerOnly, err)
	}

	foreign, err := windows.SecurityDescriptorFromString("O:" + unrelated.String() +
		"D:P(A;;FA;;;" + current.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	if owned, err := windowsDescriptorIsCurrentTokenAuthorityOwner(foreign, current, defaultOwner); err != nil || owned {
		t.Fatalf("unrelated owner admitted=%v err=%v", owned, err)
	}
}

func TestWindowsSecureNormalizesOrdinaryCreationToExactUserOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ordinary")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	f, err = OpenWritable(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if admitted, err := IsOwnedByCurrentTokenAuthority(f); err != nil || !admitted {
		t.Fatalf("ordinary Windows owner admitted=%v err=%v", admitted, err)
	}
	if err := Secure(f); err != nil {
		t.Fatal(err)
	}
	if exact, err := IsCurrentUserOwner(f); err != nil || !exact {
		t.Fatalf("repaired exact user owner=%v err=%v", exact, err)
	}
	if private, err := IsOwnerOnly(f); err != nil || !private {
		t.Fatalf("repaired protected owner-only=%v err=%v", private, err)
	}
}

func differentWindowsOwnerSID(t *testing.T, excluded ...*windows.SID) *windows.SID {
	t.Helper()
	for _, kind := range []windows.WELL_KNOWN_SID_TYPE{
		windows.WinWorldSid,
		windows.WinLocalSystemSid,
		windows.WinBuiltinUsersSid,
		windows.WinBuiltinGuestsSid,
	} {
		candidate, err := windows.CreateWellKnownSid(kind)
		if err != nil {
			t.Fatal(err)
		}
		different := true
		for _, sid := range excluded {
			if candidate.Equals(sid) {
				different = false
				break
			}
		}
		if different {
			return candidate
		}
	}
	t.Fatal("could not find a distinct well-known Windows SID")
	return nil
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
