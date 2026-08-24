//go:build windows

package session

import (
	"errors"
	"fmt"
	"math"
	"os"

	"golang.org/x/sys/windows"
)

// LockFileEx is held on a sentinel byte far beyond any viable transcript,
// from before migration and replay until Close. Windows byte-range locks also
// deny the owning process access through a second handle, so locking the data
// range would prevent read-only replay and live forks while a Session is open.
// Kernel ownership means a crashed process releases the lock without leaving
// a stale sidecar behind.
const windowsSessionLockOffset = uint64(math.MaxInt64 - 1)

func windowsSessionLockOverlapped() windows.Overlapped {
	offset := windowsSessionLockOffset
	return windows.Overlapped{
		Offset:     uint32(offset),
		OffsetHigh: uint32(offset >> 32),
	}
}

func acquireLock(f *os.File) error {
	overlapped := windowsSessionLockOverlapped()
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&overlapped,
	)
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return ErrSessionLocked
	}
	return fmt.Errorf("locking session: %w", err)
}

func releaseLock(f *os.File) error {
	overlapped := windowsSessionLockOverlapped()
	return windows.UnlockFileEx(
		windows.Handle(f.Fd()),
		0,
		1,
		0,
		&overlapped,
	)
}
