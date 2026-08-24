//go:build windows

package checkpoint

import (
	"math"
	"os"

	"golang.org/x/sys/windows"
)

// Windows byte-range locks are mandatory even between handles in the owning
// process. Keep restore liveness on a sentinel beyond any viable workspace
// file so compare-and-publish fingerprints can read the data range through a
// separately bound descriptor. Journal/control files deliberately continue to
// use acquireJournalLock's full-range exclusion.
const windowsRestoreLivenessLockOffset = uint64(math.MaxInt64 - 1)

func acquireRestoreLivenessLock(f *os.File) error {
	offset := windowsRestoreLivenessLockOffset
	overlapped := windows.Overlapped{
		Offset:     uint32(offset),
		OffsetHigh: uint32(offset >> 32),
	}
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&overlapped,
	)
}

// Windows replacement is a recoverable three-rename exchange. Hold the same
// sentinel on the displaced target so another replace or remove cannot enter
// that exchange after both operations compared the same post-image.
func acquireReplacementTargetLivenessLock(f *os.File) error {
	return acquireRestoreLivenessLock(f)
}
