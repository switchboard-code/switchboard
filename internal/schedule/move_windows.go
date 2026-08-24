//go:build windows

package schedule

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// scheduleRenameInfo is FILE_RENAME_INFORMATION's legacy no-replace layout.
// Some supported Windows kernels reject FileRenameInformationEx when its
// flags are zero; a false ReplaceIfExists is the native operation that states
// the intended no-replace semantic directly. RootDirectory binds the
// destination to the already-open private quarantine rather than resolving a
// mutable path.
type scheduleRenameInfo struct {
	ReplaceIfExists byte
	RootDirectory   windows.Handle
	FileNameLength  uint32
	FileName        [1]uint16
}

type scheduleWindowsFileID struct {
	volume uint32
	index  uint64
}

func moveScheduleNoReplace(source, destination *os.Root, from, to string, opened *os.File) (published bool, resultErr error) {
	if source == nil || destination == nil || opened == nil {
		return false, errors.New("schedule quarantine move requires live roots and an opened ledger")
	}
	mutation := windows.Handle(opened.Fd())
	if _, err := scheduleWindowsHandleID(mutation); err != nil {
		return false, fmt.Errorf("validating exact schedule mutation handle: %w", err)
	}
	destinationDir, err := leaseScheduleDestinationWindows(destination)
	if err != nil {
		return false, fmt.Errorf("leasing exact schedule destination: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(destinationDir)) }()
	var anchor windows.Handle
	if to == migrationQuarantineEntry {
		anchor, err = createScheduleDestinationAnchorWindows(destinationDir)
		if err != nil {
			return false, fmt.Errorf("anchoring private schedule destination: %w", err)
		}
		defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(anchor)) }()
	}

	encoded, err := windows.UTF16FromString(to)
	if err != nil {
		return false, err
	}
	encoded = encoded[:len(encoded)-1]
	var layout scheduleRenameInfo
	length := uintptr(len(encoded)) * unsafe.Sizeof(uint16(0))
	bufferLength := unsafe.Sizeof(layout) + length
	if bufferLength > uintptr(^uint32(0)) {
		return false, errors.New("schedule quarantine name is too long")
	}
	// Keep a zero terminator after FileNameLength for filesystem filters that
	// inspect the otherwise length-delimited name as a terminated string.
	buffer := make([]byte, bufferLength)
	info := (*scheduleRenameInfo)(unsafe.Pointer(&buffer[0]))
	info.RootDirectory = destinationDir
	info.FileNameLength = uint32(length)
	copy(unsafe.Slice(&info.FileName[0], len(encoded)), encoded)
	renameErr := windows.NtSetInformationFile(
		mutation,
		&windows.IO_STATUS_BLOCK{},
		&buffer[0],
		uint32(len(buffer)),
		windows.FileRenameInformation,
	)
	runtime.KeepAlive(buffer)
	runtime.KeepAlive(opened)
	if status, ok := renameErr.(windows.NTStatus); ok {
		renameErr = status.Errno()
	}
	if renameErr == nil {
		if flushErr := windows.FlushFileBuffers(mutation); flushErr != nil {
			return true, fmt.Errorf("flushing schedule quarantine rename: %w", flushErr)
		}
		return true, nil
	}
	// Namespace APIs can report an error after a redirector or filter driver
	// made the operation visible. Classify the outcome from the two bound names;
	// every ambiguous state is retained as published recovery evidence.
	if sameScheduleFileWindows(destination, to, opened) {
		return true, renameErr
	}
	if sameScheduleFileWindows(source, from, opened) {
		if _, destinationErr := destination.Lstat(to); destinationErr == nil || errors.Is(destinationErr, os.ErrNotExist) {
			return false, renameErr
		}
	}
	return true, errors.Join(renameErr, errors.New("schedule quarantine rename outcome is ambiguous"))
}

func leaseScheduleDestinationWindows(root *os.Root) (windows.Handle, error) {
	base, err := root.Open(".")
	if err != nil {
		return windows.InvalidHandle, err
	}
	defer base.Close()
	objectName, err := windows.NewNTUnicodeString("")
	if err != nil {
		return windows.InvalidHandle, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		RootDirectory: windows.Handle(base.Fd()),
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	attributes.Length = uint32(unsafe.Sizeof(*attributes))
	var handle windows.Handle
	err = windows.NtCreateFile(
		&handle,
		windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		attributes,
		&windows.IO_STATUS_BLOCK{},
		nil,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT,
		0,
		0,
	)
	runtime.KeepAlive(base)
	runtime.KeepAlive(objectName)
	if status, ok := err.(windows.NTStatus); ok {
		err = status.Errno()
	}
	if err != nil {
		return windows.InvalidHandle, err
	}
	baseID, baseErr := scheduleWindowsHandleID(windows.Handle(base.Fd()))
	leaseID, leaseErr := scheduleWindowsHandleID(handle)
	var info windows.ByHandleFileInformation
	infoErr := windows.GetFileInformationByHandle(handle, &info)
	if baseErr != nil || leaseErr != nil || infoErr != nil || baseID != leaseID ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return windows.InvalidHandle, errors.Join(
			baseErr,
			leaseErr,
			infoErr,
			errors.New("schedule destination lease did not retain the exact physical directory"),
			windows.CloseHandle(handle),
		)
	}
	return handle, nil
}

func createScheduleDestinationAnchorWindows(directory windows.Handle) (windows.Handle, error) {
	for range 32 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return windows.InvalidHandle, err
		}
		name := fmt.Sprintf(".schedule-move-anchor-%x", random[:])
		objectName, err := windows.NewNTUnicodeString(name)
		if err != nil {
			return windows.InvalidHandle, err
		}
		attributes := &windows.OBJECT_ATTRIBUTES{
			RootDirectory: directory,
			ObjectName:    objectName,
			Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
		}
		attributes.Length = uint32(unsafe.Sizeof(*attributes))
		var handle windows.Handle
		err = windows.NtCreateFile(
			&handle,
			windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.DELETE,
			attributes,
			&windows.IO_STATUS_BLOCK{},
			nil,
			windows.FILE_ATTRIBUTE_HIDDEN,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
			windows.FILE_CREATE,
			windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|
				windows.FILE_OPEN_REPARSE_POINT|windows.FILE_DELETE_ON_CLOSE|windows.FILE_WRITE_THROUGH,
			0,
			0,
		)
		runtime.KeepAlive(objectName)
		if status, ok := err.(windows.NTStatus); ok && status == windows.STATUS_OBJECT_NAME_COLLISION {
			continue
		}
		if status, ok := err.(windows.NTStatus); ok {
			err = status.Errno()
		}
		if err != nil {
			return windows.InvalidHandle, err
		}
		return handle, nil
	}
	return windows.InvalidHandle, errors.New("could not allocate a private schedule destination anchor")
}

func scheduleWindowsHandleID(handle windows.Handle) (scheduleWindowsFileID, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return scheduleWindowsFileID{}, err
	}
	if info.VolumeSerialNumber == 0 || (info.FileIndexHigh == 0 && info.FileIndexLow == 0) {
		return scheduleWindowsFileID{}, errors.New("Windows filesystem returned an unstable schedule file identity")
	}
	return scheduleWindowsFileID{
		volume: info.VolumeSerialNumber,
		index:  uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
	}, nil
}

func sameScheduleFileWindows(root *os.Root, name string, opened *os.File) bool {
	linked, err := root.Open(name)
	if err != nil {
		return false
	}
	defer linked.Close()
	openedInfo, openedErr := opened.Stat()
	linkedInfo, linkedErr := linked.Stat()
	return openedErr == nil && linkedErr == nil && openedInfo.Mode().IsRegular() && linkedInfo.Mode().IsRegular() && os.SameFile(openedInfo, linkedInfo)
}
