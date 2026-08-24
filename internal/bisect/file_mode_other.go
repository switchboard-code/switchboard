//go:build !unix && !windows

package bisect

import "io/fs"

func bisectRestorableMode(mode fs.FileMode) fs.FileMode {
	return mode & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}
