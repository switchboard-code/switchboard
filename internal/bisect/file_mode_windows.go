//go:build windows

package bisect

import "io/fs"

func bisectRestorableMode(mode fs.FileMode) fs.FileMode {
	if mode == 0 {
		return 0
	}
	if mode.Perm()&0o222 == 0 {
		return 0o444
	}
	return 0o666
}
