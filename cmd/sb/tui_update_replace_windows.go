//go:build windows

package main

import (
	"golang.org/x/sys/windows"
)

func replaceExecutable(exe, staged string) error {
	return replaceExecutableWithBackup(exe, staged, moveUpdateFileWindows)
}

func moveUpdateFileWindows(source, destination string) error {
	source16, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destination16, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(source16, destination16, windows.MOVEFILE_WRITE_THROUGH)
}
