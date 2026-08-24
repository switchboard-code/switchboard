//go:build windows

package safeexec

import (
	"errors"
	"fmt"
	"os"
)

func stableExecutableInfo(path string) (os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("executable is not a regular file: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	after, err := os.Lstat(path)
	if err != nil || !opened.Mode().IsRegular() ||
		!sameExecutable(before, opened) || !sameExecutable(opened, after) {
		return nil, errors.Join(err, fmt.Errorf("%w: executable changed while it was bound", ErrChanged))
	}
	return opened, nil
}
