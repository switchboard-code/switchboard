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

var filePrivacyReOpenFile = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile")

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

// OpenWritable opens an existing regular single-link file for read/write use.
// Secure reacquires ACL rights through this exact handle, including its
// TokenOwner bootstrap when the current DACL does not grant WRITE_OWNER.
func OpenWritable(path string) (*os.File, error) {
	return openWindowsFile(path,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL,
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

// IsOwnedByCurrentTokenAuthority reports whether f is owned either by the
// process user or by the process token's default owner. Windows assigns the
// latter to objects created without an explicit security descriptor; for an
// elevated token it can be an owner-capable group rather than TokenUser.
//
// This predicate is only an admission gate for descriptor-bound migration.
// Secure and SecureDirectory always rewrite the owner to TokenUser, and their
// final verification continues to require that exact user SID plus the
// protected one-user DACL.
func IsOwnedByCurrentTokenAuthority(f *os.File) (bool, error) {
	descriptor, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	current, defaultOwner, err := currentWindowsAuthoritySIDs()
	if err != nil {
		return false, err
	}
	return windowsDescriptorIsCurrentTokenAuthorityOwner(descriptor, current, defaultOwner)
}

func windowsDescriptorIsCurrentUserOwner(descriptor *windows.SECURITY_DESCRIPTOR, current *windows.SID) (bool, error) {
	owner, _, err := descriptor.Owner()
	if err != nil {
		return false, err
	}
	return owner != nil && owner.IsValid() && owner.Equals(current), nil
}

func windowsDescriptorIsCurrentTokenAuthorityOwner(descriptor *windows.SECURITY_DESCRIPTOR, current, defaultOwner *windows.SID) (bool, error) {
	owner, _, err := descriptor.Owner()
	if err != nil {
		return false, err
	}
	if owner == nil || !owner.IsValid() {
		return false, nil
	}
	return owner.Equals(current) || owner.Equals(defaultOwner), nil
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
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.READ_CONTROL|
			windows.WRITE_DAC|windows.WRITE_OWNER,
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

// CreatePrivateDirInRoot creates one literal directory beneath root with the
// protected current-user-only descriptor installed by the same NtCreateFile
// operation that publishes the directory. This closes the inherited-DACL
// window that a create-then-secure sequence would expose to another process.
func CreatePrivateDirInRoot(root *os.Root, name string) (*os.File, error) {
	if err := validateRootLeaf(root, name); err != nil {
		return nil, err
	}
	descriptor, err := ownerOnlyWindowsDirectoryDescriptor()
	if err != nil {
		return nil, err
	}
	directory, err := openWindowsRootDirectory(root, name, windows.FILE_CREATE, descriptor, true)
	if err != nil {
		return nil, err
	}
	ownerOnly, checkErr := DirectoryIsOwnerOnly(directory)
	linked, linkErr := root.Lstat(name)
	opened, statErr := directory.Stat()
	if checkErr != nil || statErr != nil || linkErr != nil || !ownerOnly ||
		linked == nil || opened == nil || !linked.IsDir() || !opened.IsDir() || !os.SameFile(linked, opened) {
		if checkErr == nil && statErr == nil && linkErr == nil && !ownerOnly {
			checkErr = errors.New("Windows did not retain the current-user-only directory DACL at creation")
		}
		return nil, errors.Join(checkErr, statErr, linkErr,
			errors.New("owner-private directory changed while it was created"), directory.Close())
	}
	return directory, nil
}

func openPrivateDirectoryInRoot(root *os.Root, name string) (*os.File, error) {
	if err := validateRootLeaf(root, name); err != nil {
		return nil, err
	}
	return openWindowsRootDirectory(root, name, windows.FILE_OPEN, nil, false)
}

func openWindowsRootDirectory(root *os.Root, name string, disposition uint32, descriptor *windows.SECURITY_DESCRIPTOR, writableACL bool) (*os.File, error) {
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
	access := uint32(windows.FILE_LIST_DIRECTORY | windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL | windows.SYNCHRONIZE)
	if writableACL {
		access |= windows.WRITE_DAC | windows.WRITE_OWNER
	}
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
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT,
		0,
		0,
	)
	runtime.KeepAlive(directory)
	runtime.KeepAlive(descriptor)
	if status, ok := err.(windows.NTStatus); ok && status == windows.STATUS_OBJECT_NAME_COLLISION {
		return nil, &os.PathError{Op: "create owner-private directory", Path: name, Err: os.ErrExist}
	}
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
	f, err := openWindowsDirectory(path, false)
	if err != nil {
		return err
	}
	defer f.Close()
	return SecureDirectory(f)
}

// SecureDirectory applies the same owner and ACL contract as EnsurePrivateDir
// to an already-open directory. When the caller's handle lacks ACL rights it
// obtains them by reopening that exact object rather than resolving its path.
func SecureDirectory(f *os.File) error {
	owned, err := IsOwnedByCurrentTokenAuthority(f)
	if err != nil {
		return fmt.Errorf("checking owner-private directory owner: %w", err)
	}
	if !owned {
		return errors.New("owner-private directory is not owned by the current user")
	}
	exactOwner, err := IsCurrentUserOwner(f)
	if err != nil {
		return fmt.Errorf("checking exact owner-private directory owner: %w", err)
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
	mutation, err := reopenWindowsObjectForSecurity(f, true, !exactOwner, dacl)
	if err != nil {
		return fmt.Errorf("reopening owner-private directory for security repair: %w", err)
	}
	securityInformation := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	var owner *windows.SID
	if !exactOwner {
		securityInformation |= windows.OWNER_SECURITY_INFORMATION
		owner = current
	}
	setErr := windows.SetSecurityInfo(windows.Handle(mutation.Fd()), windows.SE_FILE_OBJECT,
		securityInformation, owner, nil, dacl, nil)
	closeErr := mutation.Close()
	if setErr != nil || closeErr != nil {
		return errors.Join(setErr, closeErr)
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

// Secure installs and verifies the protected current-user-only DACL. When the
// caller's handle lacks ACL rights it obtains them by reopening that exact
// object rather than resolving its path.
func Secure(f *os.File) error {
	if err := verifyRegularSingleLink(f); err != nil {
		return err
	}
	owned, err := IsOwnedByCurrentTokenAuthority(f)
	if err != nil {
		return fmt.Errorf("checking owner-private file owner: %w", err)
	}
	if !owned {
		return errors.New("owner-private file is not owned by the current user")
	}
	exactOwner, err := IsCurrentUserOwner(f)
	if err != nil {
		return fmt.Errorf("checking exact owner-private file owner: %w", err)
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
	mutation, err := reopenWindowsObjectForSecurity(f, false, !exactOwner, dacl)
	if err != nil {
		return fmt.Errorf("reopening owner-private file for security repair: %w", err)
	}
	securityInformation := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	var owner *windows.SID
	if !exactOwner {
		securityInformation |= windows.OWNER_SECURITY_INFORMATION
		owner = current
	}
	setErr := windows.SetSecurityInfo(windows.Handle(mutation.Fd()), windows.SE_FILE_OBJECT,
		securityInformation, owner, nil, dacl, nil)
	closeErr := mutation.Close()
	if setErr != nil || closeErr != nil {
		return fmt.Errorf("setting current-user-only file DACL: %w", errors.Join(setErr, closeErr))
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

// reopenWindowsObjectForSecurity obtains the minimum ACL rights on the exact
// object selected by f. The reopen is identity-bound, so callers that received
// an os.Root handle without WRITE_DAC can repair it without a pathname reopen
// or a final-component substitution race.
//
// A token's default owner can differ from TokenUser, notably for an elevated
// process. Ownership implicitly grants WRITE_DAC but not WRITE_OWNER. In that
// case, first use WRITE_DAC to give TokenUser full control without changing the
// owner, close that handle, and only then request WRITE_OWNER for the final
// owner normalization.
func reopenWindowsObjectForSecurity(f *os.File, directory, writeOwner bool, dacl *windows.ACL) (*os.File, error) {
	if writeOwner {
		if dacl == nil {
			return nil, errors.New("owner-private security repair has no bootstrap DACL")
		}
		bootstrap, err := reopenWindowsObjectWithSecurityAccess(f, directory, false)
		if err != nil {
			return nil, fmt.Errorf("reopening exact object for DACL bootstrap: %w", err)
		}
		setErr := windows.SetSecurityInfo(windows.Handle(bootstrap.Fd()), windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
			nil, nil, dacl, nil)
		closeErr := bootstrap.Close()
		if setErr != nil || closeErr != nil {
			return nil, fmt.Errorf("bootstrapping current-user DACL before owner repair: %w",
				errors.Join(setErr, closeErr))
		}
	}
	return reopenWindowsObjectWithSecurityAccess(f, directory, writeOwner)
}

func reopenWindowsObjectWithSecurityAccess(f *os.File, directory, writeOwner bool) (*os.File, error) {
	if f == nil {
		return nil, errors.New("owner-private security repair has no object handle")
	}
	access := uint32(windows.WRITE_DAC)
	if writeOwner {
		access |= windows.WRITE_OWNER
	}
	var handle windows.Handle
	if directory {
		if err := validateWindowsSecurityRepairDirectory(f); err != nil {
			return nil, err
		}
		// The Win32 reopen used here previously combined WRITE_DAC with
		// metadata rights, and that directory open was denied on a token whose
		// owner authority could grant WRITE_DAC alone. NtCreateFile with an
		// empty name and the selected directory as RootDirectory reopens that
		// exact directory while requesting only the rights this mutation uses.
		emptyName, err := windows.NewNTUnicodeString("")
		if err != nil {
			return nil, err
		}
		attributes := &windows.OBJECT_ATTRIBUTES{
			RootDirectory: windows.Handle(f.Fd()),
			ObjectName:    emptyName,
			Attributes:    windows.OBJ_DONT_REPARSE,
		}
		attributes.Length = uint32(unsafe.Sizeof(*attributes))
		err = windows.NtCreateFile(
			&handle,
			access,
			attributes,
			&windows.IO_STATUS_BLOCK{},
			nil,
			0,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			windows.FILE_OPEN,
			windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT,
			0,
			0,
		)
		runtime.KeepAlive(f)
		if err != nil {
			if status, ok := err.(windows.NTStatus); ok {
				err = status.Errno()
			}
			return nil, err
		}
	} else {
		// ReOpenFile is proven for regular files and guarantees that the new
		// handle selects the same file. Keep its metadata rights because the
		// validation below consumes them.
		result, _, callErr := filePrivacyReOpenFile.Call(
			f.Fd(),
			uintptr(access|windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES),
			uintptr(uint32(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)),
			uintptr(uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)),
		)
		runtime.KeepAlive(f)
		handle = windows.Handle(result)
		if handle == windows.InvalidHandle {
			if callErr == nil || callErr == windows.ERROR_SUCCESS {
				callErr = windows.ERROR_INVALID_HANDLE
			}
			return nil, callErr
		}
	}
	mutation := os.NewFile(uintptr(handle), f.Name()+" (security repair)")
	if mutation == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("converting owner-private security-repair handle")
	}
	if directory {
		// The empty-name native open is bound by the kernel to f, so there is
		// no path-derived identity to compare. Recheck the stable source handle
		// at the seam; querying the minimal WRITE_DAC handle would require the
		// extra metadata right that caused this repair path to be denied.
		if err := validateWindowsSecurityRepairDirectory(f); err != nil {
			return nil, errors.Join(err, mutation.Close())
		}
		return mutation, nil
	}
	originalInfo, originalErr := f.Stat()
	mutationInfo, mutationErr := mutation.Stat()
	if originalErr != nil || mutationErr != nil || originalInfo == nil || mutationInfo == nil ||
		originalInfo.IsDir() || mutationInfo.IsDir() ||
		!os.SameFile(originalInfo, mutationInfo) {
		return nil, errors.Join(errors.New("security-repair reopen returned a different object"),
			originalErr, mutationErr, mutation.Close())
	}
	if err := verifyRegularSingleLink(mutation); err != nil {
		return nil, errors.Join(err, mutation.Close())
	}
	return mutation, nil
}

func validateWindowsSecurityRepairDirectory(f *os.File) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("security-repair directory has the wrong object type")
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

type windowsTokenOwner struct {
	Owner *windows.SID
}

func currentWindowsAuthoritySIDs() (*windows.SID, *windows.SID, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, nil, err
	}
	if user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return nil, nil, errors.New("current Windows user has no valid SID")
	}

	var size uint32
	err = windows.GetTokenInformation(token, windows.TokenOwner, nil, 0, &size)
	if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || size < uint32(unsafe.Sizeof(windowsTokenOwner{})) {
		if err == nil {
			err = errors.New("current Windows token has no default owner")
		}
		return nil, nil, err
	}
	buffer := make([]byte, size)
	if err := windows.GetTokenInformation(token, windows.TokenOwner, &buffer[0], size, &size); err != nil {
		return nil, nil, err
	}
	defaultOwner := (*windowsTokenOwner)(unsafe.Pointer(&buffer[0])).Owner
	if defaultOwner == nil || !defaultOwner.IsValid() {
		return nil, nil, errors.New("current Windows token has no valid default-owner SID")
	}
	currentCopy, err := user.User.Sid.Copy()
	if err != nil {
		return nil, nil, err
	}
	defaultCopy, err := defaultOwner.Copy()
	if err != nil {
		return nil, nil, err
	}
	return currentCopy, defaultCopy, nil
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
