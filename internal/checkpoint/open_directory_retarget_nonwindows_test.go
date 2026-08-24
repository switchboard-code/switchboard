//go:build !windows

package checkpoint

import "os"

func attemptOpenDirectoryRetarget(_ *os.Root, path, moved string) (bool, error) {
	return false, os.Rename(path, moved)
}

func openDirectoryRetargetPrevented(error) bool { return false }
