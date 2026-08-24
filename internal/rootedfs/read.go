// Package rootedfs provides bounded, descriptor-rooted reads for configuration
// and declaration files. Callers choose the authority root; every descendant
// component is then resolved beneath one os.Root handle for the whole read.
package rootedfs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrNotRegular = errors.New("file is not regular")
	ErrTooLarge   = errors.New("file exceeds the bounded read limit")
	ErrChanged    = errors.New("file changed while it was read")
)

// ReadFile reads one complete regular file beneath root. It rejects final
// symlinks and special files, follows no parent symlink outside root, and
// refuses a changing pathname or an incomplete over-limit prefix.
func ReadFile(root, name string, limit int64) ([]byte, error) {
	return ReadFileWithHook(root, name, limit, nil)
}

// ReadFileWithHook is ReadFile with a deterministic test seam immediately
// before descriptor acquisition.
func ReadFileWithHook(root, name string, limit int64, beforeOpen func()) ([]byte, error) {
	data, _, err := ReadFileInfoWithHook(root, name, limit, beforeOpen)
	return data, err
}

// ReadFileInfo is ReadFile plus the descriptor identity that produced the
// bytes, for callers that later compare-and-remove the same persisted image.
func ReadFileInfo(root, name string, limit int64) ([]byte, os.FileInfo, error) {
	return ReadFileInfoWithHook(root, name, limit, nil)
}

// ReadFileInfoWithHook is ReadFileInfo with the deterministic acquisition
// seam used by race regressions.
func ReadFileInfoWithHook(root, name string, limit int64, beforeOpen func()) ([]byte, os.FileInfo, error) {
	if limit <= 0 {
		return nil, nil, fmt.Errorf("bounded read limit must be positive")
	}
	rooted, relative, err := openRoot(root, name)
	if err != nil {
		return nil, nil, err
	}
	defer rooted.Close()

	before, err := rooted.Lstat(relative)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%w: %s", ErrNotRegular, name)
	}
	if before.Size() > limit {
		return nil, nil, fmt.Errorf("%w: %s is %d bytes (limit %d)", ErrTooLarge, name, before.Size(), limit)
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	file, err := openRootedRead(rooted, relative)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, nil, fmt.Errorf("%w: %s changed identity while it was opened", ErrChanged, name)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > limit {
		return nil, nil, fmt.Errorf("%w: %s grew beyond %d bytes", ErrTooLarge, name, limit)
	}
	finished, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	linked, linkErr := rooted.Lstat(relative)
	if linkErr != nil || !linked.Mode().IsRegular() ||
		!os.SameFile(opened, finished) || !os.SameFile(finished, linked) ||
		opened.Size() != finished.Size() || finished.Size() != int64(len(data)) ||
		!opened.ModTime().Equal(finished.ModTime()) {
		return nil, nil, errors.Join(linkErr, fmt.Errorf("%w: %s", ErrChanged, name))
	}
	return data, finished, nil
}

func openRoot(root, name string) (*os.Root, string, error) {
	if strings.TrimSpace(root) == "" {
		return nil, "", errors.New("root is required")
	}
	relative := filepath.Clean(filepath.FromSlash(name))
	if relative == "." || filepath.IsAbs(relative) || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, "", fmt.Errorf("path is outside the read root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, "", err
	}
	before, err := os.Lstat(real)
	if err != nil {
		return nil, "", err
	}
	if !before.IsDir() {
		return nil, "", fmt.Errorf("read root is not a directory")
	}
	rooted, err := OpenRoot(real)
	if err != nil {
		return nil, "", err
	}
	opened, err := rooted.Stat(".")
	if err != nil {
		_ = rooted.Close()
		return nil, "", err
	}
	if !os.SameFile(before, opened) {
		_ = rooted.Close()
		return nil, "", fmt.Errorf("read root changed while it was opened")
	}
	return rooted, relative, nil
}
