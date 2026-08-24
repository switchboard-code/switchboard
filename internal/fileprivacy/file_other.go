//go:build !unix && !windows

// Package fileprivacy creates and verifies owner-private regular files.
package fileprivacy

import (
	"errors"
	"os"
)

func Open(path string) (*os.File, error) { return os.Open(path) }

func OpenInRoot(*os.Root, string) (*os.File, error) {
	return nil, errors.New("secure rooted owner-private file opens are unavailable on this platform")
}

func OpenWritable(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR, 0)
}

// Platforms without a descriptor-level ownership primitive fail closed for
// authority-bearing state.
func IsCurrentUserOwner(*os.File) (bool, error) {
	return false, errors.New("current-user ownership checks are unavailable on this platform")
}

func Create(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := Secure(f); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

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

func OpenReadWriteOrCreate(path string) (*os.File, bool, error) {
	f, err := Create(path)
	if err == nil {
		return f, true, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, false, err
	}
	f, err = os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, false, err
	}
	ok, err := IsOwnerOnly(f)
	if err != nil || !ok {
		_ = f.Close()
		if err != nil {
			return nil, false, err
		}
		return nil, false, errors.New("existing file is not owner-only")
	}
	return f, false, nil
}

func OpenReadWriteOrCreateInRoot(*os.Root, string) (*os.File, bool, error) {
	return nil, false, errors.New("secure rooted owner-private file opens are unavailable on this platform")
}

func openPrivateDirectoryInRoot(*os.Root, string) (*os.File, error) {
	return nil, errors.New("secure rooted owner-private directory opens are unavailable on this platform")
}

func EnsurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func SecureDirectory(f *os.File) error {
	if err := f.Chmod(0o700); err != nil {
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
	info, err := f.Stat()
	return err == nil && info.IsDir() && info.Mode().Perm() == 0o700, err
}

func Secure(f *os.File) error {
	return SecureMode(f, 0o600)
}

func SecureMode(f *os.File, mode os.FileMode) error {
	if mode.Perm()&0o077 != 0 {
		return errors.New("owner-private file mode grants group or world access")
	}
	if err := f.Chmod(mode.Perm()); err != nil {
		return err
	}
	ok, err := IsOwnerOnlyMode(f, mode)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("file is not owner-only after securing it")
	}
	return nil
}

func IsOwnerOnly(f *os.File) (bool, error) {
	return IsOwnerOnlyMode(f, 0o600)
}

func IsOwnerOnlyMode(f *os.File, mode os.FileMode) (bool, error) {
	info, err := f.Stat()
	return err == nil && mode.Perm()&0o077 == 0 && info.Mode().IsRegular() && info.Mode().Perm() == mode.Perm(), err
}
