//go:build unix

package safeexec

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func stableExecutableInfo(path string) (os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("executable is not an executable regular file: %s", path)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("binding executable descriptor")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	after, err := os.Lstat(path)
	if err != nil || !opened.Mode().IsRegular() || opened.Mode()&0o111 == 0 ||
		!sameExecutable(before, opened) || !sameExecutable(opened, after) {
		return nil, errors.Join(err, fmt.Errorf("%w: executable changed while it was bound", ErrChanged))
	}
	return opened, nil
}
