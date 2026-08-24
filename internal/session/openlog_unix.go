//go:build unix

package session

import (
	"os"

	"golang.org/x/sys/unix"
)

func openSessionLogDescriptor(path string, writable bool) (*os.File, error) {
	flags := unix.O_RDONLY
	if writable {
		flags = unix.O_RDWR
	}
	fd, err := unix.Open(path, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func sessionLogLinkCount(f *os.File) (uint64, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Nlink), nil
}
