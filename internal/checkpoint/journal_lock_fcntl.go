//go:build aix || solaris

package checkpoint

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func acquireJournalLock(f *os.File) error {
	lock := unix.Flock_t{
		Type:   unix.F_WRLCK,
		Whence: int16(io.SeekStart),
		Start:  0,
		Len:    0,
	}
	return unix.FcntlFlock(f.Fd(), unix.F_SETLK, &lock)
}
