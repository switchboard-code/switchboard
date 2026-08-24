//go:build !linux && !darwin && !windows

package session

import "os"

func publicationObjectMutationStamp(marker *os.File) (publicationMutationStamp, error) {
	info, err := marker.Stat()
	if err != nil {
		return publicationMutationStamp{}, err
	}
	return publicationMutationStamp{
		size:                info.Size(),
		modifiedSeconds:     info.ModTime().Unix(),
		modifiedNanoseconds: int64(info.ModTime().Nanosecond()),
		attributes:          uint64(info.Mode()),
	}, nil
}
