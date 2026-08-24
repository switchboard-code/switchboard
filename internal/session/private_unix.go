//go:build unix

package session

import (
	"errors"
	"fmt"
	"os"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
	"golang.org/x/sys/unix"
)

func preparePrivateSessionStore(root string) error {
	return ensurePrivateSessionDirectory(root)
}

func ensurePrivateSessionDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("session directory %s is not a real directory", path)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return errors.New("converting private session directory descriptor")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.IsDir() || !os.SameFile(before, opened) {
		return errors.Join(fmt.Errorf("session directory %s changed while it was opened", path), err)
	}
	if err := fileprivacy.SecureDirectory(f); err != nil {
		return err
	}
	after, err := f.Stat()
	if err != nil {
		return err
	}
	current, currentErr := os.Lstat(path)
	if currentErr != nil || !current.IsDir() || !os.SameFile(after, current) {
		return errors.Join(fmt.Errorf("session directory %s changed while it was secured", path), currentErr)
	}
	ownerOnly, ownerErr := fileprivacy.DirectoryIsOwnerOnly(f)
	if ownerErr != nil {
		return ownerErr
	}
	if after.Mode().Perm() != 0o700 || !ownerOnly {
		return fmt.Errorf("session directory %s is not owner-only", path)
	}
	return nil
}

func createPrivateSessionFile(path string) (*os.File, error) { return fileprivacy.Create(path) }

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
	return fileprivacy.Secure(f)
}

func privateSessionFileIsOwnerOnly(f *os.File) (bool, error) {
	return fileprivacy.IsOwnerOnly(f)
}

func createPrivateSessionTempDir(parent, pattern string) (string, error) {
	path, err := os.MkdirTemp(parent, pattern)
	if err != nil {
		return "", err
	}
	if err := ensurePrivateSessionDirectory(path); err != nil {
		return "", errors.Join(err, os.Remove(path))
	}
	return path, nil
}
