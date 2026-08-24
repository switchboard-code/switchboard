//go:build windows

package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
	"golang.org/x/sys/windows"
)

var historyReOpenFileWindows = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile")

type historyFileIDInfoWindows struct {
	VolumeSerialNumber uint64
	FileID             [16]byte
}

func openHistoryDataDescriptor(path string, writable, createNew bool) (*os.File, error) {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	access := uint32(windows.GENERIC_READ | windows.READ_CONTROL)
	if writable {
		access |= windows.GENERIC_WRITE
	}
	if createNew {
		access |= windows.WRITE_DAC | windows.WRITE_OWNER
	}
	disposition := uint32(windows.OPEN_EXISTING)
	var attributes *windows.SecurityAttributes
	if createNew {
		disposition = windows.CREATE_NEW
		descriptor, err := historyOwnerOnlyDescriptor()
		if err != nil {
			return nil, err
		}
		attributes = &windows.SecurityAttributes{
			Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
			SecurityDescriptor: descriptor,
		}
	}
	handle, err := windows.CreateFile(path16, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		attributes, disposition, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(handle), path)
	if f == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("converting prompt history handle")
	}
	return f, nil
}

func openHistoryLockDescriptor(path string, createNew bool) (*os.File, error) {
	return openHistoryDataDescriptor(path, true, createNew)
}

// createHistoryBoundPrivateFile creates relative to the retained directory
// handle and installs the protected current-user DACL in the same NtCreateFile
// call. Narrowing an inherited DACL after creation is too late: another
// principal could retain a readable handle before prompt bytes are written.
func createHistoryBoundPrivateFile(root *os.Root, name string) (*os.File, error) {
	if root == nil || name == "" || !filepath.IsLocal(name) || filepath.Base(name) != name ||
		strings.ContainsAny(name, `/\\`) {
		return nil, errors.New("invalid private prompt history leaf name")
	}
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	descriptor, err := historyOwnerOnlyDescriptor()
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
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|
			windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER,
		attributes,
		&windows.IO_STATUS_BLOCK{},
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_CREATE,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|
			windows.FILE_OPEN_REPARSE_POINT|windows.FILE_WRITE_THROUGH,
		0,
		0,
	)
	runtime.KeepAlive(directory)
	runtime.KeepAlive(descriptor)
	if status, ok := err.(windows.NTStatus); ok && status == windows.STATUS_OBJECT_NAME_COLLISION {
		return nil, &os.PathError{Op: "create private prompt history", Path: name, Err: fs.ErrExist}
	}
	if err != nil {
		return nil, fmt.Errorf("creating private prompt history by root handle: %w", err)
	}
	f := os.NewFile(uintptr(handle), name)
	if f == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("converting private prompt history handle")
	}
	ownerOnly, err := historyFileIsOwnerOnly(f)
	if err != nil || !ownerOnly {
		return nil, errors.Join(errors.New("private prompt history create did not retain its protected DACL"), err, f.Close())
	}
	return f, nil
}

func tryHistoryFileLock(f *os.File) (bool, error) {
	var overlapped windows.Overlapped
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, math.MaxUint32, math.MaxUint32, &overlapped)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return false, nil
	}
	return false, err
}

func unlockHistoryFileLock(f *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, math.MaxUint32, math.MaxUint32, &overlapped)
}

func historyFileLinkCount(f *os.File) (uint64, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return 0, err
	}
	return uint64(info.NumberOfLinks), nil
}

func historyFileIdentity(f *os.File) (string, error) {
	var info historyFileIDInfoWindows
	if err := windows.GetFileInformationByHandleEx(
		windows.Handle(f.Fd()), windows.FileIdInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)),
	); err != nil {
		return "", err
	}
	if info.VolumeSerialNumber == 0 || info.FileID == ([16]byte{}) {
		return "", errors.New("Windows prompt history has no stable file identity")
	}
	return fmt.Sprintf("windows:%x:%s", info.VolumeSerialNumber, hex.EncodeToString(info.FileID[:])), nil
}

func currentHistoryUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	if user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return nil, errors.New("current Windows user has no valid SID")
	}
	return user.User.Sid.Copy()
}

func historyOwnerOnlyDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	sid, err := currentHistoryUserSID()
	if err != nil {
		return nil, err
	}
	// P protects this DACL from inherited entries. Exactly one full-access ACE
	// names the current process user; no group, administrator, or Everyone ACE
	// is admitted to the persisted prompt record.
	return windows.SecurityDescriptorFromString("O:" + sid.String() + "D:P(A;;FA;;;" + sid.String() + ")")
}

func secureHistoryFile(f *os.File) error {
	owned, err := fileprivacy.IsOwnedByCurrentTokenAuthority(f)
	if err != nil {
		return fmt.Errorf("checking prompt history owner: %w", err)
	}
	if !owned {
		return errors.New("prompt history is not owned by the current user")
	}
	exactOwner, err := fileprivacy.IsCurrentUserOwner(f)
	if err != nil {
		return fmt.Errorf("checking exact prompt history owner: %w", err)
	}
	descriptor, err := historyOwnerOnlyDescriptor()
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	mutation, err := reopenHistorySecurityFile(f, !exactOwner, dacl)
	if err != nil {
		return fmt.Errorf("reopening exact prompt history for DACL repair: %w", err)
	}
	securityInformation := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	var owner *windows.SID
	if !exactOwner {
		securityInformation |= windows.OWNER_SECURITY_INFORMATION
		owner, err = currentHistoryUserSID()
		if err != nil {
			_ = mutation.Close()
			return err
		}
	}
	setErr := windows.SetSecurityInfo(windows.Handle(mutation.Fd()), windows.SE_FILE_OBJECT,
		securityInformation, owner, nil, dacl, nil)
	closeErr := mutation.Close()
	if setErr != nil || closeErr != nil {
		return fmt.Errorf("setting current-user-only prompt history DACL: %w", errors.Join(setErr, closeErr))
	}
	ownerOnly, err := historyFileIsOwnerOnly(f)
	if err != nil {
		return err
	}
	if !ownerOnly {
		return errors.New("Windows did not retain the current-user-only prompt history DACL")
	}
	return nil
}

func reopenHistorySecurityFile(f *os.File, writeOwner bool, dacl *windows.ACL) (*os.File, error) {
	if writeOwner {
		if dacl == nil {
			return nil, errors.New("prompt history DACL repair has no bootstrap DACL")
		}
		bootstrap, err := reopenHistorySecurityFileWithAccess(f, false)
		if err != nil {
			return nil, fmt.Errorf("reopening exact prompt history for DACL bootstrap: %w", err)
		}
		setErr := windows.SetSecurityInfo(windows.Handle(bootstrap.Fd()), windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
			nil, nil, dacl, nil)
		closeErr := bootstrap.Close()
		if setErr != nil || closeErr != nil {
			return nil, fmt.Errorf("bootstrapping current-user prompt history DACL before owner repair: %w",
				errors.Join(setErr, closeErr))
		}
	}
	return reopenHistorySecurityFileWithAccess(f, writeOwner)
}

func reopenHistorySecurityFileWithAccess(f *os.File, writeOwner bool) (*os.File, error) {
	if f == nil {
		return nil, errors.New("prompt history DACL repair has no file handle")
	}
	access := uint32(windows.READ_CONTROL | windows.WRITE_DAC | windows.FILE_READ_ATTRIBUTES)
	if writeOwner {
		access |= windows.WRITE_OWNER
	}
	result, _, callErr := historyReOpenFileWindows.Call(
		f.Fd(),
		uintptr(access),
		uintptr(uint32(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)),
		uintptr(uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)),
	)
	runtime.KeepAlive(f)
	handle := windows.Handle(result)
	if handle == windows.InvalidHandle {
		if callErr == nil || callErr == windows.ERROR_SUCCESS {
			callErr = windows.ERROR_INVALID_HANDLE
		}
		return nil, callErr
	}
	mutation := os.NewFile(uintptr(handle), f.Name()+" (history DACL repair)")
	if mutation == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("converting exact prompt history DACL-repair handle")
	}
	want, wantErr := historyFileIdentity(f)
	got, gotErr := historyFileIdentity(mutation)
	links, linkErr := historyFileLinkCount(mutation)
	var info windows.ByHandleFileInformation
	infoErr := windows.GetFileInformationByHandle(handle, &info)
	if wantErr != nil || gotErr != nil || linkErr != nil || infoErr != nil || want != got || links != 1 ||
		info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return nil, errors.Join(errors.New("DACL-repair reopen returned a different or unsafe prompt history file"),
			wantErr, gotErr, linkErr, infoErr, mutation.Close())
	}
	return mutation, nil
}

// historyFileIsOwnerOnly verifies the ACL through the open handle rather than
// trusting os.FileMode, which does not represent Windows discretionary ACLs.
func historyFileIsOwnerOnly(f *os.File) (bool, error) {
	current, err := currentHistoryUserSID()
	if err != nil {
		return false, err
	}
	descriptor, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	return historyWindowsDescriptorIsOwnerOnly(descriptor, current)
}

func historyFileIsCurrentUserOwner(f *os.File) (bool, error) {
	current, err := currentHistoryUserSID()
	if err != nil {
		return false, err
	}
	descriptor, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	return historyWindowsDescriptorIsCurrentUserOwner(descriptor, current)
}

func historyWindowsDescriptorIsCurrentUserOwner(descriptor *windows.SECURITY_DESCRIPTOR, current *windows.SID) (bool, error) {
	owner, _, err := descriptor.Owner()
	if err != nil {
		return false, err
	}
	return owner != nil && owner.IsValid() && owner.Equals(current), nil
}

func historyWindowsDescriptorIsOwnerOnly(descriptor *windows.SECURITY_DESCRIPTOR, current *windows.SID) (bool, error) {
	owned, err := historyWindowsDescriptorIsCurrentUserOwner(descriptor, current)
	if err != nil {
		return false, err
	}
	if !owned {
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
	aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	return aceSID.IsValid() && aceSID.Equals(current), nil
}

func syncHistoryDirectory(string) error { return nil }

func syncHistoryBoundDirectory(*os.Root) error { return nil }

func reopenHistoryMutationFile(f *os.File) (*os.File, error) {
	result, _, callErr := historyReOpenFileWindows.Call(
		f.Fd(),
		uintptr(uint32(windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|windows.DELETE)),
		uintptr(uint32(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)),
		uintptr(uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_WRITE_THROUGH)),
	)
	runtime.KeepAlive(f)
	handle := windows.Handle(result)
	if handle == windows.InvalidHandle {
		if callErr == nil || callErr == windows.ERROR_SUCCESS {
			callErr = windows.ERROR_INVALID_HANDLE
		}
		return nil, callErr
	}
	mutation := os.NewFile(uintptr(handle), f.Name()+" (history mutation)")
	if mutation == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("converting exact prompt history mutation handle")
	}
	want, err := historyFileIdentity(f)
	if err != nil {
		return nil, errors.Join(err, mutation.Close())
	}
	got, err := historyFileIdentity(mutation)
	if err != nil || got != want {
		return nil, errors.Join(errors.New("ReOpenFile returned a different prompt history inode"), err, mutation.Close())
	}
	return mutation, nil
}

func scrubHistoryRetiredFile(f *os.File) error {
	mutation, err := reopenHistoryMutationFile(f)
	if err != nil {
		return err
	}
	defer mutation.Close()
	links, err := historyFileLinkCount(mutation)
	if err != nil {
		return err
	}
	if links > 1 {
		return fmt.Errorf("retired prompt history has %d hard links", links)
	}
	if err := secureHistoryFile(mutation); err != nil {
		return err
	}
	if err := mutation.Truncate(0); err != nil {
		return err
	}
	return mutation.Sync()
}
