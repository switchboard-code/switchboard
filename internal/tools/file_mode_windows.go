//go:build windows

package tools

import "io/fs"

// Windows exposes regular files as either read-only (0444) or writable
// (0666). Owner/group distinctions and execute/special bits cannot be
// restored through os.Chmod, so normalize to the state the filesystem can
// actually report before comparing or checkpointing a publication.
func restorableFileMode(mode fs.FileMode) fs.FileMode {
	if mode == 0 {
		return 0
	}
	if mode.Perm()&0o222 == 0 {
		return 0o444
	}
	return 0o666
}
