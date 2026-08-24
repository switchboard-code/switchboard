//go:build windows

package extensions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
	"golang.org/x/sys/windows"
)

func prepareInstallCacheDirectory(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("plugin cache root must not be a symbolic link")
		}
		if !info.IsDir() {
			return errors.New("plugin cache root is not a directory")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := fileprivacy.EnsurePrivateDir(path); err != nil {
		return err
	}
	return secureInstallWindowsPath(path, true, nil)
}

func validateInstallCacheDirectory(path string, info os.FileInfo) error {
	if err := validateInstallWindowsPath(path, true, info); err != nil {
		return fmt.Errorf("plugin cache root is not protected current-user-only: %w", err)
	}
	return nil
}

func securePrivateInstallDirectory(root *os.Root, rel string) error {
	info, err := safeInfo(root, rel)
	if err != nil {
		return err
	}
	if info == nil {
		return os.ErrNotExist
	}
	if !info.IsDir() {
		return errors.New("not a directory")
	}
	return secureInstallWindowsPath(installRootPath(root, rel), true, info)
}

func validatePrivateInstallDirectory(root *os.Root, rel string, info os.FileInfo) error {
	if !info.IsDir() {
		return errors.New("not a directory")
	}
	return validateInstallWindowsPath(installRootPath(root, rel), true, info)
}

func securePrivateInstallFile(root *os.Root, rel string, file *os.File, _ bool) error {
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	anchored, err := safeInfo(root, rel)
	if err != nil {
		return err
	}
	if anchored == nil || !os.SameFile(opened, anchored) {
		return errors.New("plugin file changed while it was secured")
	}
	return secureInstallWindowsPath(installRootPath(root, rel), false, opened)
}

func validatePrivateInstallFile(root *os.Root, rel string, info os.FileInfo, _ bool) error {
	if !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	return validateInstallWindowsPath(installRootPath(root, rel), false, info)
}

// NTFS does not carry a portable POSIX execute-bit class. Native executable
// capability comes from the plugin manifest and component kind; treating the
// synthetic Windows FileMode as an execute bit would make a copy change its
// content digest.
func installSourceExecutable(os.FileInfo) bool {
	return false
}

func installRootPath(root *os.Root, rel string) string {
	return filepath.Join(root.Name(), filepath.FromSlash(rel))
}

func secureInstallWindowsPath(path string, directory bool, expected os.FileInfo) error {
	file, err := openInstallWindowsObject(path, directory, windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER)
	if err != nil {
		return err
	}
	defer file.Close()
	if expected != nil {
		opened, err := file.Stat()
		if err != nil {
			return err
		}
		if !os.SameFile(expected, opened) {
			return errors.New("plugin cache object changed while it was secured")
		}
	}
	descriptor, err := privateInstallWindowsDescriptor(directory)
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	current, err := currentInstallWindowsUserSID()
	if err != nil {
		return err
	}
	if err := windows.SetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		current, nil, dacl, nil); err != nil {
		return fmt.Errorf("setting current-user-only plugin cache DACL: %w", err)
	}
	ownerOnly, err := installWindowsObjectIsOwnerOnly(file, directory)
	if err != nil {
		return err
	}
	if !ownerOnly {
		return errors.New("Windows did not retain the current-user-only plugin cache DACL")
	}
	return nil
}

func validateInstallWindowsPath(path string, directory bool, expected os.FileInfo) error {
	file, err := openInstallWindowsObject(path, directory, windows.READ_CONTROL)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if expected != nil && !os.SameFile(expected, opened) {
		return errors.New("plugin cache object changed while it was validated")
	}
	ownerOnly, err := installWindowsObjectIsOwnerOnly(file, directory)
	if err != nil {
		return err
	}
	if !ownerOnly {
		return errors.New("owner or DACL is not protected current-user-only")
	}
	return nil
}

func openInstallWindowsObject(path string, directory bool, access uint32) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	} else {
		flags |= windows.FILE_ATTRIBUTE_NORMAL
	}
	handle, err := windows.CreateFile(name, access|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("converting plugin cache object handle")
	}
	if err := verifyInstallWindowsKind(file, directory); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func verifyInstallWindowsKind(file *os.File, directory bool) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return err
	}
	isDirectory := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if isDirectory != directory || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("plugin cache handle has the wrong object type")
	}
	if !directory && info.NumberOfLinks != 1 {
		return fmt.Errorf("plugin cache file has %d hard links", info.NumberOfLinks)
	}
	return nil
}

func privateInstallWindowsDescriptor(directory bool) (*windows.SECURITY_DESCRIPTOR, error) {
	sid, err := currentInstallWindowsUserSID()
	if err != nil {
		return nil, err
	}
	flags := ""
	if directory {
		flags = "OICI"
	}
	return windows.SecurityDescriptorFromString("O:" + sid.String() + "D:P(A;" + flags + ";FA;;;" + sid.String() + ")")
}

func currentInstallWindowsUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	if user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return nil, errors.New("current Windows user has no valid SID")
	}
	return user.User.Sid.Copy()
}

func installWindowsObjectIsOwnerOnly(file *os.File, directory bool) (bool, error) {
	current, err := currentInstallWindowsUserSID()
	if err != nil {
		return false, err
	}
	descriptor, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	return installWindowsDescriptorIsOwnerOnly(descriptor, current, directory)
}

func installWindowsDescriptorIsOwnerOnly(descriptor *windows.SECURITY_DESCRIPTOR, current *windows.SID, directory bool) (bool, error) {
	owner, _, err := descriptor.Owner()
	if err != nil {
		return false, err
	}
	if owner == nil || !owner.IsValid() || !owner.Equals(current) {
		return false, nil
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return false, err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return false, nil
	}
	dacl, defaulted, err := descriptor.DACL()
	if err != nil {
		return false, err
	}
	if dacl == nil || defaulted || dacl.AceCount != 1 {
		return false, nil
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		return false, err
	}
	if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
		ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
		return false, nil
	}
	inherit := ace.Header.AceFlags & (windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE)
	if directory && inherit != windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE {
		return false, nil
	}
	if !directory && inherit != 0 {
		return false, nil
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	return sid.IsValid() && sid.Equals(current), nil
}
