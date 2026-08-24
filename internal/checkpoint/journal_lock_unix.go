//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package checkpoint

import (
	"os"
	"syscall"
)

func acquireJournalLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
