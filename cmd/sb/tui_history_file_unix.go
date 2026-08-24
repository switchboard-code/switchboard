//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
	"github.com/switchboard-code/switchboard/internal/rootedfs"
	"golang.org/x/sys/unix"
)

func openHistoryDataDescriptor(path string, writable, createNew bool) (*os.File, error) {
	flags := unix.O_RDONLY
	if writable {
		flags = unix.O_RDWR
	}
	if createNew {
		flags |= unix.O_CREAT | unix.O_EXCL
	}
	fd, err := unix.Open(path, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return nil, errors.New("converting prompt history descriptor")
	}
	return f, nil
}

func openHistoryLockDescriptor(path string, createNew bool) (*os.File, error) {
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if createNew {
		flags |= unix.O_CREAT | unix.O_EXCL
	}
	fd, err := unix.Open(path, flags, 0o600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return nil, errors.New("converting prompt history lock descriptor")
	}
	return f, nil
}

func createHistoryBoundPrivateFile(root *os.Root, name string) (*os.File, error) {
	f, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := secureHistoryFile(f); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func tryHistoryFileLock(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return false, err
}

func unlockHistoryFileLock(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}

func historyFileLinkCount(f *os.File) (uint64, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Nlink), nil
}

func historyFileIdentity(f *os.File) (string, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &stat); err != nil {
		return "", err
	}
	return fmt.Sprintf("unix:%d:%d", uint64(stat.Dev), uint64(stat.Ino)), nil
}

func secureHistoryFile(f *os.File) error { return fileprivacy.Secure(f) }

func historyFileIsOwnerOnly(f *os.File) (bool, error) {
	return fileprivacy.IsOwnerOnly(f)
}

func syncHistoryDirectory(path string) error {
	root, err := rootedfs.OpenRoot(path)
	if err != nil {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return errors.Join(err, root.Close())
	}
	return errors.Join(dir.Sync(), dir.Close(), root.Close())
}

func syncHistoryBoundDirectory(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

// POSIX has no unlink-by-descriptor operation. The caller has already moved
// the selected inode to an unguessable owner-only quarantine; erase its bytes
// through that exact descriptor and retain the empty inode. A final-seam name
// substitution can therefore leak neither history bytes nor cause a foreign
// pathname to be unlinked.
func scrubHistoryRetiredFile(f *os.File) error {
	links, err := historyFileLinkCount(f)
	if err != nil {
		return err
	}
	if links > 1 {
		return fmt.Errorf("retired prompt history has %d hard links", links)
	}
	if err := secureHistoryFile(f); err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	return f.Sync()
}
