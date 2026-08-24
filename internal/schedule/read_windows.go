//go:build windows

package schedule

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

func openScheduleRead(root *os.Root, name string) (*os.File, error) {
	if root == nil || name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return nil, errors.New("schedule mutation read requires a literal rooted file name")
	}
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		RootDirectory: windows.Handle(directory.Fd()),
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	attributes.Length = uint32(unsafe.Sizeof(*attributes))
	var handle windows.Handle
	err = windows.NtCreateFile(
		&handle,
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.READ_CONTROL|windows.DELETE,
		attributes,
		&windows.IO_STATUS_BLOCK{},
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|
			windows.FILE_OPEN_REPARSE_POINT|windows.FILE_WRITE_THROUGH,
		0,
		0,
	)
	runtime.KeepAlive(directory)
	runtime.KeepAlive(objectName)
	if status, ok := err.(windows.NTStatus); ok {
		err = status.Errno()
	}
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(handle), name)
	if f == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("converting schedule mutation handle")
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		info.NumberOfLinks != 1 {
		if err == nil {
			err = errors.New("schedule mutation image is not a regular single-link file")
		}
		return nil, errors.Join(err, f.Close())
	}
	if _, err := scheduleWindowsHandleID(handle); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}
