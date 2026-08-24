//go:build windows

package main

import (
	"errors"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// openCodexAuthFile holds the home-directory handle while opening each literal
// descendant with NtCreateFile.RootDirectory. FILE_OPEN_REPARSE_POINT and
// OBJ_DONT_REPARSE make a junction or symlink a refusal, not a traversal.
func openCodexAuthFile(home string) (*os.File, error) {
	homeDir, err := openAbsoluteWindowsDirectory(home)
	if err != nil {
		return nil, err
	}
	defer homeDir.Close()

	codexDir, err := openRelativeWindowsObject(homeDir, ".codex", true)
	if err != nil {
		return nil, err
	}
	defer codexDir.Close()

	return openRelativeWindowsObject(codexDir, "auth.json", false)
}

func openAbsoluteWindowsDirectory(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name,
		windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("converting the home-directory handle")
	}
	if err := verifyWindowsObject(file, true); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func openRelativeWindowsObject(parent *os.File, name string, directory bool) (*os.File, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		RootDirectory: windows.Handle(parent.Fd()),
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	attributes.Length = uint32(unsafe.Sizeof(*attributes))

	access := uint32(windows.FILE_GENERIC_READ | windows.READ_CONTROL)
	options := uint32(windows.FILE_NON_DIRECTORY_FILE | windows.FILE_SYNCHRONOUS_IO_NONALERT | windows.FILE_OPEN_REPARSE_POINT)
	if directory {
		access = windows.FILE_LIST_DIRECTORY | windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL | windows.SYNCHRONIZE
		options = windows.FILE_DIRECTORY_FILE | windows.FILE_SYNCHRONOUS_IO_NONALERT | windows.FILE_OPEN_REPARSE_POINT
	}
	var handle windows.Handle
	err = windows.NtCreateFile(&handle, access, attributes, &windows.IO_STATUS_BLOCK{}, nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN, options, 0, 0)
	runtime.KeepAlive(parent)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), name)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("converting the Codex login handle")
	}
	if err := verifyWindowsObject(file, directory); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func verifyWindowsObject(file *os.File, directory bool) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("the Codex login path contains a reparse point")
	}
	isDirectory := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if isDirectory != directory {
		return errors.New("the Codex login path has an unexpected object type")
	}
	return nil
}

// Codex uses ordinary Windows file creation for auth.json, so the file
// inherits the user's profile ACL. That ACL normally includes the user,
// LocalSystem, and built-in Administrators. Accept precisely those readers;
// requiring Switchboard's one-ACE DACL would reject a normal Codex install.
func codexAuthFileIsAcceptable(file *os.File) (bool, error) {
	handle := windows.Handle(file.Fd())
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return false, err
	}
	if info.NumberOfLinks != 1 || info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return false, nil
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	return codexWindowsDescriptorIsAcceptable(descriptor)
}

func codexWindowsDescriptorIsAcceptable(descriptor *windows.SECURITY_DESCRIPTOR) (bool, error) {
	current, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false, err
	}
	if current == nil || current.User.Sid == nil || !current.User.Sid.IsValid() {
		return false, errors.New("the current Windows user has no valid SID")
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return false, err
	}
	if owner == nil || !owner.IsValid() || !owner.Equals(current.User.Sid) {
		return false, nil
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return false, err
	}
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false, err
	}
	trusted := []*windows.SID{current.User.Sid, system, administrators}

	dacl, _, err := descriptor.DACL()
	if err != nil {
		return false, err
	}
	if dacl == nil {
		return false, nil
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return false, err
		}
		if ace == nil {
			return false, nil
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if !sid.IsValid() || !windowsSIDIn(sid, trusted) {
				return false, nil
			}
		default:
			// Object/callback allow ACEs have different layouts and can carry
			// access for another principal. A normal inherited profile DACL
			// does not need them, so an unfamiliar ACE fails closed.
			return false, nil
		}
	}
	return true, nil
}

func windowsSIDIn(candidate *windows.SID, allowed []*windows.SID) bool {
	for _, sid := range allowed {
		if candidate.Equals(sid) {
			return true
		}
	}
	return false
}
