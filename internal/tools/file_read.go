package tools

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// This is the shared ceiling already used by grep, drift, and checkpoint
// capture. A write/edit preimage must be complete, never a truncated prefix.
const maxWorkspaceFileBytes = int64(4 << 20)

func openRegularWorkspaceFile(root *os.Root, relative, display string, before fs.FileInfo, beforeOpen func()) (*os.File, fs.FileInfo, error) {
	if beforeOpen != nil {
		beforeOpen()
	}
	file, err := openWorkspaceRead(root, relative)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = file.Close()
		return nil, nil, fmt.Errorf("%s changed identity while it was opened", display)
	}
	return file, opened, nil
}

func validateRegularWorkspaceFile(root *os.Root, relative, display string, file *os.File, opened fs.FileInfo, bytesRead int64) error {
	finished, err := file.Stat()
	if err != nil {
		return err
	}
	linked, linkErr := root.Lstat(relative)
	if linkErr != nil || !linked.Mode().IsRegular() ||
		!os.SameFile(opened, finished) || !os.SameFile(finished, linked) ||
		opened.Size() != finished.Size() || opened.Size() != bytesRead ||
		!opened.ModTime().Equal(finished.ModTime()) {
		return errors.Join(linkErr, fmt.Errorf("%s changed while it was read", display))
	}
	return nil
}

func readRegularWorkspaceFile(root *os.Root, relative, display string, maxBytes int64, beforeOpen func()) ([]byte, fs.FileInfo, error) {
	before, err := root.Lstat(relative)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is not a regular file", display)
	}
	if before.Size() > maxBytes {
		return nil, nil, fmt.Errorf("%s is %d bytes; file limit is %d", display, before.Size(), maxBytes)
	}
	file, opened, err := openRegularWorkspaceFile(root, relative, display, before, beforeOpen)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, nil, fmt.Errorf("%s grew beyond the %d-byte file limit", display, maxBytes)
	}
	if err := validateRegularWorkspaceFile(root, relative, display, file, opened, int64(len(data))); err != nil {
		return nil, nil, err
	}
	return data, opened, nil
}
