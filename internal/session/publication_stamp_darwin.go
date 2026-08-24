//go:build darwin

package session

import (
	"os"

	"golang.org/x/sys/unix"
)

func publicationObjectMutationStamp(marker *os.File) (publicationMutationStamp, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(marker.Fd()), &stat); err != nil {
		return publicationMutationStamp{}, err
	}
	return publicationMutationStamp{
		size:                stat.Size,
		modifiedSeconds:     stat.Mtim.Sec,
		modifiedNanoseconds: stat.Mtim.Nsec,
		changedSeconds:      stat.Ctim.Sec,
		changedNanoseconds:  stat.Ctim.Nsec,
		identityHigh:        uint64(stat.Gen),
		identityLow:         stat.Ino,
		volumeOrDevice:      uint64(stat.Dev),
		attributes:          uint64(stat.Mode),
	}, nil
}
