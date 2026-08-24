//go:build windows

// Package fileprivacy creates and verifies owner-private regular files.
package fileprivacy

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Open opens an existing regular single-link file without traversing a final
// reparse point. Call IsOwnerOnly before trusting security-sensitive contents.
func Open(path string) (*os.File, error) {
	return openWindowsFile(path, windows.GENERIC_READ|windows.READ_CONTROL, nil, windows.OPEN_EXISTING)
}

// OpenInRoot is Open relative to a retained Windows directory handle.
func OpenInRoot(root *os.Root, name string) (*os.File, error) {
	if err := validateRootLeaf(root, name); err != nil {
		return nil, err
	}
	return openWindowsRootFile(root, name,
		windows.FILE_GENERIC_READ|windows.READ_CONTROL, windows.FILE_OPEN, nil)
}

// OpenWritable opens an existing regular single-link file with the rights
// needed to migrate its owner and DACL through the bound handle.
func OpenWritable(path string) (*os.File, error) {
	return openWindowsFile(path,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER,
		nil, windows.OPEN_EXISTING)
}

// IsCurrentUserOwner checks the object owner without imposing
// Switchboard's stricter protected-DACL contract.
func IsCurrentUserOwner(f *os.File) (bool, error) {
	current, err := currentWindowsUserSID()
	if err != nil {
		return false, err
	}
	descriptor, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	return windowsDescriptorIsCurrentUserOwner(descriptor, current)
}

func windowsDescriptorIsCurrentUserOwner(descriptor *windows.SECURITY_DESCRIPTOR, current *windows.SID) (bool, error) {
	owner, _, err := descriptor.Owner()
	if err != nil {
		return false, err
	}
	return owner != nil && owner.IsValid() && owner.Equals(current), nil
}

// Create applies a protected one-user DACL as part of CREATE_NEW, before any
// caller can write sensitive bytes, and then verifies it through the handle.
func Create(path string) (*os.File, error) {
	descriptor, err := ownerOnlyWindowsDescriptor()
	if err != nil {
		return nil, err
	}
	attributes := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	f, err := openWindowsFile(path,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER,
		attributes, windows.CREATE_NEW)
	if err != nil {
		return nil, err
	}
	if err := Secure(f); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// CreateTemp creates a random same-directory file with Create's atomic DACL
// contract.
func CreateTemp(dir, pattern string) (*os.File, error) {
	for range 100 {
		path, err := tempCandidate(dir, pattern)
		if err != nil {
			return nil, err
		}
		f, err := Create(path)
		if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			continue
		}
		return f, err
	}
	return nil, errors.New("could not allocate an owner-private temporary file")
}

// OpenReadWriteOrCreate opens an owner-private regular file for locking or
// other read/write metadata use, creating it with the protected DACL before
// publication when absent.
func OpenReadWriteOrCreate(path string) (*os.File, bool, error) {
	created, err := Create(path)
	if err == nil {
		return created, true, nil
	}
	if !errors.Is(err, windows.ERROR_FILE_EXISTS) && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return nil, false, err
	}
	f, err := openWindowsFile(path,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL,
		nil, windows.OPEN_EXISTING)
	if err != nil {
		return nil, false, err
	}
	ownerOnly, err := IsOwnerOnly(f)
	if err != nil || !ownerOnly {
		_ = f.Close()
		if err != nil {
			return nil, false, err
		}
		return nil, false, errors.New("existing file does not have a protected current-user-only DACL")
	}
	return f, false, nil
}

// OpenReadWriteOrCreateInRoot opens or atomically creates an owner-private
// regular file relative to a retained Windows directory handle. The protected
// one-user DACL is installed in the same NtCreateFile call that creates the
// object, before any caller can write security-sensitive bytes.
func OpenReadWriteOrCreateInRoot(root *os.Root, name string) (*os.File, bool, error) {
	if err := validateRootLeaf(root, name); err != nil {
		return nil, false, err
	}
	descriptor, err := ownerOnlyWindowsDescriptor()
	if err != nil {
		return nil, false, err
	}
	access := uint32(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE |
		windows.READ_CONTROL | windows.WRITE_DAC | windows.WRITE_OWNER)
	f, err := openWindowsRootFile(root, name, access, windows.FILE_CREATE, descriptor)
	if err == nil {
		ownerOnly, checkErr := IsOwnerOnly(f)
		if checkErr != nil || !ownerOnly {
			_ = f.Close()
			if checkErr != nil {
				return nil, false, checkErr
			}
			return nil, false, errors.New("created file does not have a protected current-user-only DACL")
		}
		return f, true, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, false, err
	}
	f, err = openWindowsRootFile(root, name,
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.READ_CONTROL,
		windows.FILE_OPEN, nil)
	if err != nil {
		return nil, false, err
	}
	ownerOnly, checkErr := IsOwnerOnly(f)
	if checkErr != nil || !ownerOnly {
		_ = f.Close()
		if checkErr != nil {
			return nil, false, checkErr
		}
		return nil, false, errors.New("existing file does not have a protected current-user-only DACL")
	}
	return f, false, nil
}

func openWindowsRootFile(root *os.Root, name string, access, disposition uint32, descriptor *windows.SECURITY_DESCRIPTOR) (*os.File, error) {
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		RootDirectory:      windows.Handle(directory.Fd()),
		ObjectName:         objectName,
		Attributes:         windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
		SecurityDescriptor: descriptor,
	}
	attributes.Length = uint32(unsafe.Sizeof(*attributes))
	var handle windows.Handle
	err = windows.NtCreateFile(
		&handle,
		access,
		attributes,
		&windows.IO_STATUS_BLOCK{},
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		disposition,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|
			windows.FILE_OPEN_REPARSE_POINT|windows.FILE_WRITE_THROUGH,
		0,
		0,
	)
	runtime.KeepAlive(directory)
	runtime.KeepAlive(descriptor)
	if status, ok := err.(windows.NTStatus); ok && status == windows.STATUS_OBJECT_NAME_COLLISION {
		return nil, &os.PathError{Op: "create owner-private file", Path: name, Err: os.ErrExist}
	}
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(handle), name)
	if f == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("converting rooted owner-private file handle")
	}
	if err := verifyRegularSingleLink(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func openPrivateDirectoryInRoot(root *os.Root, name string) (*os.File, error) {
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		RootDirectory: windows.Handle(directory.Fd()),
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	attributes.Length = uint32(unsafe.Sizeof(*attributes))
	var handle windows.Handle
	err = windows.NtCreateFile(
		&handle,
		windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|
			windows.WRITE_DAC|windows.WRITE_OWNER|windows.SYNCHRONIZE,
		attributes,
		&windows.IO_STATUS_BLOCK{},
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT,
		0,
		0,
	)
	runtime.KeepAlive(directory)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(handle), name)
	if f == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("converting rooted owner-private directory handle")
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = f.Close()
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = f.Close()
		return nil, errors.New("rooted owner-private directory has the wrong object type")
	}
	return f, nil
}

// EnsurePrivateDir creates path when needed, then installs and verifies a
// protected current-user-only DACL whose ACE is inherited by child files and
// directories.
func EnsurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	f, err := openWindowsDirectory(path, true)
	if err != nil {
		return err
	}
	defer f.Close()
	owned, err := IsCurrentUserOwner(f)
	if err != nil {
		return fmt.Errorf("checking owner-private directory owner: %w", err)
	}
	if !owned {
		return errors.New("owner-private directory is not owned by the current user")
	}
	descriptor, err := ownerOnlyWindowsDirectoryDescriptor()
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	current, err := currentWindowsUserSID()
	if err != nil {
		return err
	}
	if err := windows.SetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		current, nil, dacl, nil); err != nil {
		return fmt.Errorf("setting current-user-only directory DACL: %w", err)
	}
	ownerOnly, err := windowsObjectIsOwnerOnly(f, true)
	if err != nil {
		return err
	}
	if !ownerOnly {
		return errors.New("Windows did not retain the current-user-only directory DACL")
	}
	return nil
}

// SecureDirectory applies the same owner and ACL contract as
// EnsurePrivateDir to an already-open directory handle with WRITE_DAC and
// WRITE_OWNER access.
func SecureDirectory(f *os.File) error {
	owned, err := IsCurrentUserOwner(f)
	if err != nil {
		return fmt.Errorf("checking owner-private directory owner: %w", err)
	}
	if !owned {
		return errors.New("owner-private directory is not owned by the current user")
	}
	descriptor, err := ownerOnlyWindowsDirectoryDescriptor()
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	current, err := currentWindowsUserSID()
	if err != nil {
		return err
	}
	if err := windows.SetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		current, nil, dacl, nil); err != nil {
		return err
	}
	ok, err := DirectoryIsOwnerOnly(f)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("directory is not owner-only after securing it")
	}
	return nil
}

func DirectoryIsOwnerOnly(f *os.File) (bool, error) {
	return windowsObjectIsOwnerOnly(f, true)
}

func openWindowsFile(path string, access uint32, attributes *windows.SecurityAttributes, disposition uint32) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		attributes, disposition,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(handle), path)
	if f == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("converting owner-private file handle")
	}
	if err := verifyRegularSingleLink(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// Secure installs and verifies the protected current-user-only DACL through
// an open handle that carries WRITE_DAC.
func Secure(f *os.File) error {
	if err := verifyRegularSingleLink(f); err != nil {
		return err
	}
	owned, err := IsCurrentUserOwner(f)
	if err != nil {
		return fmt.Errorf("checking owner-private file owner: %w", err)
	}
	if !owned {
		return errors.New("owner-private file is not owned by the current user")
	}
	descriptor, err := ownerOnlyWindowsDescriptor()
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	current, err := currentWindowsUserSID()
	if err != nil {
		return err
	}
	if err := windows.SetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		current, nil, dacl, nil); err != nil {
		return fmt.Errorf("setting current-user-only file DACL: %w", err)
	}
	ownerOnly, err := IsOwnerOnly(f)
	if err != nil {
		return err
	}
	if !ownerOnly {
		return errors.New("Windows did not retain the current-user-only file DACL")
	}
	return nil
}

// SecureMode is Secure on Windows because NTFS does not expose portable Unix
// mode bits; the current-user owner and protected DACL are the privacy contract.
func SecureMode(f *os.File, _ os.FileMode) error { return Secure(f) }

// IsOwnerOnly verifies a protected DACL containing exactly one explicit allow
// ACE for the current process user.
func IsOwnerOnly(f *os.File) (bool, error) {
	if err := verifyRegularSingleLink(f); err != nil {
		return false, err
	}
	return windowsObjectIsOwnerOnly(f, false)
}

func IsOwnerOnlyMode(f *os.File, _ os.FileMode) (bool, error) { return IsOwnerOnly(f) }

func windowsObjectIsOwnerOnly(f *os.File, directory bool) (bool, error) {
	current, err := currentWindowsUserSID()
	if err != nil {
		return false, err
	}
	descriptor, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	return windowsDescriptorIsOwnerOnly(descriptor, current, directory)
}

func windowsDescriptorIsOwnerOnly(descriptor *windows.SECURITY_DESCRIPTOR, current *windows.SID, directory bool) (bool, error) {
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
	if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
		return false, nil
	}
	inheritance := ace.Header.AceFlags & (windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE)
	if directory && inheritance != windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE {
		return false, nil
	}
	if !directory && inheritance != 0 {
		return false, nil
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	return sid.IsValid() && sid.Equals(current), nil
}

func openWindowsDirectory(path string, writeDACL bool) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	access := uint32(windows.READ_CONTROL)
	if writeDACL {
		access |= windows.WRITE_DAC | windows.WRITE_OWNER
	}
	handle, err := windows.CreateFile(name, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(handle), path)
	if f == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("converting owner-private directory handle")
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = f.Close()
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = f.Close()
		return nil, errors.New("owner-private directory is not a real directory")
	}
	return f, nil
}

func verifyRegularSingleLink(f *os.File) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return errors.New("owner-private file is not a real regular file")
	}
	if info.NumberOfLinks != 1 {
		return fmt.Errorf("owner-private file has %d hard links", info.NumberOfLinks)
	}
	return nil
}

func currentWindowsUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	if user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return nil, errors.New("current Windows user has no valid SID")
	}
	return user.User.Sid.Copy()
}

func ownerOnlyWindowsDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	sid, err := currentWindowsUserSID()
	if err != nil {
		return nil, err
	}
	return windows.SecurityDescriptorFromString("O:" + sid.String() + "D:P(A;;FA;;;" + sid.String() + ")")
}

func ownerOnlyWindowsDirectoryDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	sid, err := currentWindowsUserSID()
	if err != nil {
		return nil, err
	}
	return windows.SecurityDescriptorFromString("O:" + sid.String() + "D:P(A;OICI;FA;;;" + sid.String() + ")")
}

func tempCandidate(dir, pattern string) (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	star := strings.LastIndexByte(pattern, '*')
	if star < 0 {
		return filepath.Join(dir, pattern+hex.EncodeToString(token[:])), nil
	}
	return filepath.Join(dir, pattern[:star]+hex.EncodeToString(token[:])+pattern[star+1:]), nil
}
