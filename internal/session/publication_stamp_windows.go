//go:build windows

package session

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type publicationWindowsBasicInfo struct {
	creationTime, lastAccessTime int64
	lastWriteTime, changeTime    int64
	attributes                   uint32
	_                            uint32
}

func publicationObjectMutationStamp(marker *os.File) (publicationMutationStamp, error) {
	handle := windows.Handle(marker.Fd())
	var basic publicationWindowsBasicInfo
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileBasicInfo,
		(*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic))); err != nil {
		return publicationMutationStamp{}, err
	}
	var identity windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &identity); err != nil {
		return publicationMutationStamp{}, err
	}
	return publicationMutationStamp{
		size:            int64(uint64(identity.FileSizeHigh)<<32 | uint64(identity.FileSizeLow)),
		modifiedSeconds: basic.lastWriteTime,
		changedSeconds:  basic.changeTime,
		identityHigh:    uint64(identity.FileIndexHigh),
		identityLow:     uint64(identity.FileIndexLow),
		volumeOrDevice:  uint64(identity.VolumeSerialNumber),
		attributes:      uint64(basic.attributes),
	}, nil
}
