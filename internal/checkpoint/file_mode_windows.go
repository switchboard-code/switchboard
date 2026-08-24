//go:build windows

package checkpoint

import "io/fs"

func restorableMode(mode fs.FileMode) fs.FileMode {
	if mode == 0 {
		return 0
	}
	if mode.Perm()&0o222 == 0 {
		return 0o444
	}
	return 0o666
}

// Before Windows mode normalization, durable journals could contain an
// otherwise valid Unix permission mask such as 0644. Accept that legacy
// representation while normalizing it to what Windows can restore.
func validStoredFileMode(mode uint32) bool {
	return mode&^uint32(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) == 0
}
