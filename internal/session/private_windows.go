//go:build windows

package session

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
	"golang.org/x/sys/windows"
)

func preparePrivateSessionStore(root string) error {
	if err := ensurePrivateSessionDirectory(root); err != nil {
		return err
	}
	// Older Windows builds created these files with Unix-looking modes, which
	// do not constrain a Windows DACL. Repair every real object in the private
	// store through a handle before cleanup, discovery, or recovery can use it.
	// Keep the one-time migration finite: WalkDir reads and sorts an entire
	// directory before its callback, so a callback counter would not bound it.
	return securePrivateSessionTree(root, maxPrivateSessionMigrationObjects)
}

const maxPrivateSessionMigrationObjects = 65536

func securePrivateSessionTree(root string, limit int) error {
	if limit < 1 {
		return errors.New("private session migration limit must be positive")
	}
	directories := []string{root}
	seen := 0
	for len(directories) != 0 {
		directory := directories[0]
		directories = directories[1:]
		remaining := limit - seen
		if remaining <= 0 {
			return fmt.Errorf("%w: Windows session privacy migration exceeds %d objects; archive stale sessions before retrying", ErrSessionInventoryTooLarge, limit)
		}
		entries, err := readSessionDirectory(directory, remaining)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("scanning Windows session privacy state in %s: %w", directory, err)
		}
		seen += len(entries)
		for _, entry := range entries {
			if entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			path := filepath.Join(directory, entry.Name())
			if entry.IsDir() {
				if err := ensurePrivateSessionDirectory(path); err != nil {
					if errors.Is(err, os.ErrNotExist) {
						continue
					}
					return err
				}
				directories = append(directories, path)
				continue
			}
			if entry.Type().IsRegular() {
				if err := securePrivateSessionFilePath(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
		}
	}
	return nil
}

func ensurePrivateSessionDirectory(path string) error {
	path = filepath.Clean(path)
	info, err := os.Lstat(path)
	if err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("session directory %s is not a real directory", path)
		}
		return securePrivateSessionDirectoryPath(path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	parent := filepath.Dir(path)
	if parent == path {
		return fmt.Errorf("session directory %s has no creatable parent", path)
	}
	if parentInfo, parentErr := os.Lstat(parent); parentErr != nil {
		if !errors.Is(parentErr, os.ErrNotExist) {
			return parentErr
		}
		if err := ensurePrivateSessionDirectory(parent); err != nil {
			return err
		}
	} else if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("session directory parent %s is not a real directory", parent)
	}

	created, err := createPrivateWindowsDirectory(path)
	if err != nil {
		return err
	}
	if !created {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("session directory %s was replaced while it was created", path)
		}
	}
	return securePrivateSessionDirectoryPath(path)
}

func createPrivateWindowsDirectory(path string) (bool, error) {
	descriptor, err := privateSessionWindowsDescriptor(true)
	if err != nil {
		return false, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	attributes := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	if err := windows.CreateDirectory(name, attributes); err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func securePrivateSessionDirectoryPath(path string) error {
	f, err := openPrivateSessionWindowsObject(path, true, true)
	if err != nil {
		return err
	}
	return errors.Join(securePrivateSessionWindowsObject(f, true), f.Close())
}

func securePrivateSessionFilePath(path string) error {
	f, err := openPrivateSessionWindowsObject(path, false, true)
	if err != nil {
		return err
	}
	return errors.Join(securePrivateSessionWindowsObject(f, false), f.Close())
}

func openPrivateSessionWindowsObject(path string, directory, writableACL bool) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	access := uint32(windows.READ_CONTROL | windows.FILE_READ_ATTRIBUTES)
	if writableACL {
		access |= windows.WRITE_DAC | windows.WRITE_OWNER
	}
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	} else {
		flags |= windows.FILE_ATTRIBUTE_NORMAL
	}
	handle, err := windows.CreateFile(name, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(handle), path)
	if f == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("converting private session object handle")
	}
	if err := verifyPrivateSessionWindowsKind(f, directory); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func createPrivateSessionFile(path string) (*os.File, error) {
	descriptor, err := privateSessionWindowsDescriptor(false)
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	attributes := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	handle, err := windows.CreateFile(name,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		attributes, windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(handle), path)
	if f == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("converting private session file handle")
	}
	if err := securePrivateSessionFile(f); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func createPrivateSessionFileInRoot(root *os.Root, name string) (*os.File, error) {
	f, created, err := fileprivacy.OpenReadWriteOrCreateInRoot(root, name)
	if err != nil {
		return nil, err
	}
	if !created {
		return nil, errors.Join(os.ErrExist, f.Close())
	}
	return f, nil
}

func securePrivateSessionFile(f *os.File) error {
	return securePrivateSessionWindowsObject(f, false)
}

func securePrivateSessionWindowsObject(f *os.File, directory bool) error {
	if err := verifyPrivateSessionWindowsKind(f, directory); err != nil {
		return err
	}
	owned, err := fileprivacy.IsOwnedByCurrentTokenAuthority(f)
	if err != nil {
		return fmt.Errorf("checking private session object owner: %w", err)
	}
	if !owned {
		return errors.New("private session object is not owned by the current user")
	}
	descriptor, err := privateSessionWindowsDescriptor(directory)
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	current, err := currentSessionWindowsUserSID()
	if err != nil {
		return err
	}
	if err := windows.SetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		current, nil, dacl, nil); err != nil {
		return fmt.Errorf("setting current-user-only session DACL: %w", err)
	}
	ownerOnly, err := privateSessionWindowsObjectIsOwnerOnly(f, directory)
	if err != nil {
		return err
	}
	if !ownerOnly {
		return errors.New("Windows did not retain the current-user-only session DACL")
	}
	return nil
}

func privateSessionFileIsOwnerOnly(f *os.File) (bool, error) {
	if err := verifyPrivateSessionWindowsKind(f, false); err != nil {
		return false, err
	}
	return privateSessionWindowsObjectIsOwnerOnly(f, false)
}

func verifyPrivateSessionWindowsKind(f *os.File, directory bool) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return err
	}
	isDirectory := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if isDirectory != directory || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("private session handle has the wrong object type")
	}
	if !directory && info.NumberOfLinks != 1 {
		return fmt.Errorf("private session file has %d hard links", info.NumberOfLinks)
	}
	return nil
}

func currentSessionWindowsUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	if user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return nil, errors.New("current Windows user has no valid SID")
	}
	return user.User.Sid.Copy()
}

func privateSessionWindowsDescriptor(directory bool) (*windows.SECURITY_DESCRIPTOR, error) {
	sid, err := currentSessionWindowsUserSID()
	if err != nil {
		return nil, err
	}
	flags := ""
	if directory {
		// Inheritance keeps the schedule ledger, retry journal, recovery
		// temporaries, and any other workspace-local control file private even
		// when their owning package uses an ordinary Windows CreateFile call.
		flags = "OICI"
	}
	return windows.SecurityDescriptorFromString("O:" + sid.String() + "D:P(A;" + flags + ";FA;;;" + sid.String() + ")")
}

func privateSessionWindowsObjectIsOwnerOnly(f *os.File, directory bool) (bool, error) {
	current, err := currentSessionWindowsUserSID()
	if err != nil {
		return false, err
	}
	descriptor, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	return privateSessionWindowsDescriptorIsOwnerOnly(descriptor, current, directory)
}

func privateSessionWindowsObjectIsCurrentUserOwner(f *os.File) (bool, error) {
	current, err := currentSessionWindowsUserSID()
	if err != nil {
		return false, err
	}
	descriptor, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	return privateSessionWindowsDescriptorIsCurrentUserOwner(descriptor, current)
}

func privateSessionWindowsDescriptorIsCurrentUserOwner(descriptor *windows.SECURITY_DESCRIPTOR, current *windows.SID) (bool, error) {
	owner, _, err := descriptor.Owner()
	if err != nil {
		return false, err
	}
	return owner != nil && owner.IsValid() && owner.Equals(current), nil
}

func privateSessionWindowsDescriptorIsOwnerOnly(descriptor *windows.SECURITY_DESCRIPTOR, current *windows.SID, directory bool) (bool, error) {
	owned, err := privateSessionWindowsDescriptorIsCurrentUserOwner(descriptor, current)
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
	aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	return aceSID.IsValid() && aceSID.Equals(current), nil
}

func createPrivateSessionTempDir(parent, pattern string) (string, error) {
	for range 100 {
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return "", err
		}
		path := filepath.Join(parent, pattern+hex.EncodeToString(token[:]))
		created, err := createPrivateWindowsDirectory(path)
		if err != nil {
			return "", err
		}
		if !created {
			continue
		}
		if err := securePrivateSessionDirectoryPath(path); err != nil {
			return "", errors.Join(err, os.Remove(path))
		}
		return path, nil
	}
	return "", errors.New("could not allocate a private session temporary directory")
}
