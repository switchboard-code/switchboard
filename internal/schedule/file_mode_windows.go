//go:build windows

package schedule

import "io/fs"

// Windows exposes only its read-only attribute through FileMode. Keep schedule
// snapshots in the same canonical form as checkpoint's CAS layer so a file
// created with requested mode 0600 and observed as 0666 is one image, not a
// false concurrent edit.
func scheduleFileMode(mode fs.FileMode) fs.FileMode {
	if mode == 0 {
		return 0
	}
	if mode.Perm()&0o222 == 0 {
		return 0o444
	}
	return 0o666
}
