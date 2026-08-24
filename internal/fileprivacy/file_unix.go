//go:build unix

// Package fileprivacy creates and verifies owner-private regular files.
package fileprivacy

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Open opens an existing regular file without following its final symlink.
// Call IsOwnerOnly before trusting security-sensitive contents.
func Open(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return nil, errors.New("converting owner-private file descriptor")
	}
	if err := verifyRegularSingleLink(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// OpenInRoot is Open relative to a retained directory capability.
func OpenInRoot(root *os.Root, name string) (*os.File, error) {
	if err := validateRootLeaf(root, name); err != nil {
		return nil, err
	}
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if err := verifyRegularSingleLink(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// OpenWritable opens an existing regular single-link file without following
// its final symlink. It is for descriptor-bound migrations that must narrow
// legacy permissions before trusting the file's contents.
func OpenWritable(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return nil, errors.New("converting owner-private file descriptor")
	}
	if err := verifyRegularSingleLink(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// IsCurrentUserOwner verifies ownership through the already-open descriptor.
func IsCurrentUserOwner(f *os.File) (bool, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &stat); err != nil {
		return false, err
	}
	return stat.Uid == uint32(unix.Geteuid()), nil
}

// IsOwnedByCurrentTokenAuthority is IsCurrentUserOwner on Unix. It exists so
// callers can use the Windows pre-migration admission rule without weakening
// the final owner-only check on other platforms.
func IsOwnedByCurrentTokenAuthority(f *os.File) (bool, error) {
	return IsCurrentUserOwner(f)
}

// Create creates path exclusively with mode 0600 and verifies the descriptor
// before returning it to a caller that may write sensitive bytes.
func Create(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return nil, errors.New("converting owner-private file descriptor")
	}
	if err := Secure(f); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// CreateTemp is os.CreateTemp with the same owner-private descriptor contract
// as Create. os.CreateTemp itself uses 0600 on Unix; Secure verifies it.
func CreateTemp(dir, pattern string) (*os.File, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	if err := Secure(f); err != nil {
		name := f.Name()
		return nil, errors.Join(err, f.Close(), os.Remove(name))
	}
	return f, nil
}

// OpenReadWriteOrCreate opens an owner-private regular file for locking or
// other read/write metadata use, creating it atomically when absent.
func OpenReadWriteOrCreate(path string) (*os.File, bool, error) {
	created, err := Create(path)
	if err == nil {
		return created, true, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, false, err
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, false, err
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return nil, false, errors.New("converting owner-private file descriptor")
	}
	if err := verifyRegularSingleLink(f); err != nil {
		_ = f.Close()
		return nil, false, err
	}
	ownerOnly, err := IsOwnerOnly(f)
	if err != nil || !ownerOnly {
		_ = f.Close()
		if err != nil {
			return nil, false, err
		}
		return nil, false, errors.New("existing file is not mode 0600")
	}
	return f, false, nil
}

// OpenReadWriteOrCreateInRoot is OpenReadWriteOrCreate relative to a retained
// directory capability. O_EXCL prevents a creation attempt from following a
// final symlink; O_NOFOLLOW and O_NONBLOCK make an existing symlink or FIFO a
// refusal rather than a traversal or blocking open.
func OpenReadWriteOrCreateInRoot(root *os.Root, name string) (*os.File, bool, error) {
	if err := validateRootLeaf(root, name); err != nil {
		return nil, false, err
	}
	f, err := root.OpenFile(name,
		os.O_RDWR|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err == nil {
		if secureErr := Secure(f); secureErr != nil {
			return nil, false, errors.Join(secureErr, f.Close())
		}
		return f, true, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, false, err
	}
	f, err = root.OpenFile(name, os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, false, err
	}
	if err := verifyRegularSingleLink(f); err != nil {
		_ = f.Close()
		return nil, false, err
	}
	ownerOnly, err := IsOwnerOnly(f)
	if err != nil || !ownerOnly {
		_ = f.Close()
		if err != nil {
			return nil, false, err
		}
		return nil, false, errors.New("existing file is not mode 0600")
	}
	return f, false, nil
}

func openPrivateDirectoryInRoot(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_DIRECTORY, 0)
}

// EnsurePrivateDir creates path when needed and narrows the final directory
// to mode 0700 without following a final symlink.
func EnsurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return errors.New("converting owner-private directory descriptor")
	}
	defer f.Close()
	return SecureDirectory(f)
}

// Secure narrows an already-open regular single-link file to mode 0600 and
// verifies the mode through that same descriptor.
func Secure(f *os.File) error {
	return SecureMode(f, 0o600)
}

// SecureMode strips extended ACLs, applies an owner-only regular-file mode,
// and verifies both through the same open descriptor.
func SecureMode(f *os.File, mode os.FileMode) error {
	if mode.Perm()&0o077 != 0 {
		return errors.New("owner-private file mode grants group or world access")
	}
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
	if err := removeExtendedACL(f); err != nil {
		return err
	}
	if err := f.Chmod(mode.Perm()); err != nil {
		return err
	}
	ownerOnly, err := IsOwnerOnlyMode(f, mode)
	if err != nil {
		return err
	}
	if !ownerOnly {
		return fmt.Errorf("file is not mode %04o after securing it", mode.Perm())
	}
	return nil
}

// IsOwnerOnly reports whether f is a regular single-link mode-0600 file.
func IsOwnerOnly(f *os.File) (bool, error) {
	return IsOwnerOnlyMode(f, 0o600)
}

// IsOwnerOnlyMode verifies an exact owner-only regular-file mode and the
// absence of any Darwin extended ACL through the open descriptor.
func IsOwnerOnlyMode(f *os.File, mode os.FileMode) (bool, error) {
	if mode.Perm()&0o077 != 0 {
		return false, nil
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &stat); err != nil {
		return false, err
	}
	if stat.Nlink != 1 {
		return false, nil
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != mode.Perm() {
		return false, err
	}
	hasACL, err := hasExtendedACL(f)
	return err == nil && !hasACL, err
}

// SecureDirectory strips any extended ACL, sets mode 0700, and verifies the
// result through the same open directory descriptor.
func SecureDirectory(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("owner-private directory is not a real directory")
	}
	owned, err := IsCurrentUserOwner(f)
	if err != nil {
		return fmt.Errorf("checking owner-private directory owner: %w", err)
	}
	if !owned {
		return errors.New("owner-private directory is not owned by the current user")
	}
	if err := removeExtendedACL(f); err != nil {
		return err
	}
	if err := f.Chmod(0o700); err != nil {
		return err
	}
	ownerOnly, err := DirectoryIsOwnerOnly(f)
	if err != nil {
		return err
	}
	if !ownerOnly {
		return errors.New("directory is not owner-only after securing it")
	}
	return nil
}

// DirectoryIsOwnerOnly reports whether f is a mode-0700 directory with no
// extended ACL that could grant access beyond the mode bits.
func DirectoryIsOwnerOnly(f *os.File) (bool, error) {
	info, err := f.Stat()
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return false, err
	}
	hasACL, err := hasExtendedACL(f)
	return err == nil && !hasACL, err
}

func verifyRegularSingleLink(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("owner-private file is not regular")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &stat); err != nil {
		return err
	}
	if stat.Nlink != 1 {
		return fmt.Errorf("owner-private file has %d hard links", stat.Nlink)
	}
	return nil
}
