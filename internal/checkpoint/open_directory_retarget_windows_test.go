//go:build windows

package checkpoint

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// Windows keeps the directory capability behind os.Root open without delete
// sharing. A pathname retarget that Unix permits is therefore rejected by the
// kernel before it can race a bound operation. Prove both the refusal and the
// retained identity so cross-platform retarget tests exercise the stronger
// Windows invariant instead of treating its setup failure as a product bug.
func attemptOpenDirectoryRetarget(root *os.Root, path, moved string) (bool, error) {
	if root == nil {
		return false, errors.New("open-directory retarget test requires a bound root")
	}
	before, err := root.Stat(".")
	if err != nil {
		return false, fmt.Errorf("stating bound directory before retarget: %w", err)
	}
	linkedBefore, err := os.Stat(path)
	if err != nil || !os.SameFile(before, linkedBefore) {
		return false, errors.Join(err, errors.New("bound directory did not match its path before retarget"))
	}

	renameErr := os.Rename(path, moved)
	if renameErr == nil {
		return false, nil
	}
	if !openDirectoryRetargetPrevented(renameErr) {
		return false, renameErr
	}
	after, rootErr := root.Stat(".")
	linkedAfter, pathErr := os.Stat(path)
	_, movedErr := os.Lstat(moved)
	if rootErr != nil || pathErr != nil || !errors.Is(movedErr, fs.ErrNotExist) ||
		!os.SameFile(before, after) || !os.SameFile(after, linkedAfter) {
		return false, errors.Join(rootErr, pathErr, movedErr,
			errors.New("Windows refused a directory retarget without retaining the bound identity"))
	}
	return true, nil
}

func openDirectoryRetargetPrevented(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
