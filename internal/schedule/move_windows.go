//go:build windows

package schedule

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var reopenScheduleFileWindows = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile")

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
	mutation, err := reopenScheduleMutationWindows(opened)
	if err != nil {
		return false, fmt.Errorf("reopening schedule ledger for exact-handle quarantine: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(mutation)) }()
	destinationDir, err := destination.Open(".")
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, destinationDir.Close()) }()

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
	info.RootDirectory = windows.Handle(destinationDir.Fd())
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
	if status, ok := renameErr.(windows.NTStatus); ok {
		renameErr = status.Errno()
	}
	runtime.KeepAlive(destinationDir)
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

func reopenScheduleMutationWindows(file *os.File) (windows.Handle, error) {
	result, _, callErr := reopenScheduleFileWindows.Call(
		uintptr(file.Fd()),
		uintptr(uint32(windows.GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES|windows.DELETE)),
		uintptr(uint32(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)),
		uintptr(uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_WRITE_THROUGH)),
	)
	runtime.KeepAlive(file)
	handle := windows.Handle(result)
	if handle == windows.InvalidHandle {
		if callErr == nil || callErr == windows.ERROR_SUCCESS {
			callErr = windows.ERROR_INVALID_HANDLE
		}
		return windows.InvalidHandle, callErr
	}
	originalID, originalErr := scheduleWindowsHandleID(windows.Handle(file.Fd()))
	reopenedID, reopenedErr := scheduleWindowsHandleID(handle)
	if originalErr != nil || reopenedErr != nil || originalID != reopenedID {
		return windows.InvalidHandle, errors.Join(
			originalErr,
			reopenedErr,
			errors.New("ReOpenFile returned a different schedule ledger identity"),
			windows.CloseHandle(handle),
		)
	}
	return handle, nil
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
