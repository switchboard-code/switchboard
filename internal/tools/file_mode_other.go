//go:build !unix && !windows

package tools

import "io/fs"

func restorableFileMode(mode fs.FileMode) fs.FileMode {
	return mode & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}
