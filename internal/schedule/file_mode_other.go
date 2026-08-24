//go:build !windows

package schedule

import "io/fs"

func scheduleFileMode(mode fs.FileMode) fs.FileMode {
	return mode & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}
