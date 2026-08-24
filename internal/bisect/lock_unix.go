//go:build unix

package bisect

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func acquireBisectLock(file *os.File) error {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return ErrLocked
	}
	return err
}

func releaseBisectLock(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
