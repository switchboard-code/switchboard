//go:build !unix && !windows

package checkpoint

import "io/fs"

func restorableMode(mode fs.FileMode) fs.FileMode {
	return mode & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}

func validStoredFileMode(mode uint32) bool {
	return uint32(restorableMode(fs.FileMode(mode))) == mode
}
