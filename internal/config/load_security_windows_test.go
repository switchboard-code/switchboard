//go:build windows

package config

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func makeLegacyBroadConfigForTest(path string) error {
	return setWindowsConfigACL(path, false)
}

func setWindowsConfigACL(path string, worldReadable bool) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sddl := "O:" + user.User.Sid.String() + "D:" +
		"(A;ID;FA;;;" + user.User.Sid.String() + ")" +
		"(A;ID;FA;;;SY)(A;ID;FA;;;BA)"
	if worldReadable {
		sddl += "(A;ID;FR;;;WD)"
	}
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(name,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION,
		user.User.Sid, nil, dacl, nil)
}

func TestWindowsLoadFileMigratesWorldReadableCurrentOwnedConfig(t *testing.T) {
	path := write(t, configSecurityFixture)
	if err := setWindowsConfigACL(path, true); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Tier("t1"); !ok {
		t.Fatal("repaired broad Windows config did not parse")
	}
	assertConfigOwnerOnly(t, path)
}

func TestWindowsLoadFileMigratesOrdinaryTokenOwnedConfig(t *testing.T) {
	// Create through the ordinary API, which assigns TOKEN_OWNER when no
	// explicit descriptor is supplied. Elevated tokens commonly use an
	// owner-capable group here even though TokenUser is the final authority.
	path := write(t, configSecurityFixture)
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Tier("t1"); !ok {
		t.Fatal("repaired ordinary Windows config did not parse")
	}
	assertConfigOwnerOnly(t, path)
}

func TestWindowsInheritedACLFixtureIsActuallyUnprotectedAndBroad(t *testing.T) {
	path := write(t, configSecurityFixture)
	if err := makeLegacyBroadConfigForTest(path); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED != 0 || dacl == nil || dacl.AceCount < 3 {
		t.Fatalf("fixture control=%#x DACL=%v; want inherited-style multi-principal ACL", control, dacl)
	}
}

func TestWindowsLoadFileRejectsBroadACLWithoutRepairRights(t *testing.T) {
	path := write(t, configSecurityFixture)
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("O:" + user.User.Sid.String() +
		"D:P(A;;FR;;;" + user.User.Sid.String() + ")(A;;FR;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		user.User.Sid, nil, dacl, nil); err != nil {
		_ = windows.CloseHandle(handle)
		t.Fatal(err)
	}
	if err := windows.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err == nil || !strings.Contains(err.Error(), "could not be opened for repair") {
		t.Fatalf("unrepairable Windows config error = %v", err)
	}
}
