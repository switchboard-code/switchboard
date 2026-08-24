//go:build unix && !aix && !android && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris

package extensions

import (
	"errors"
	"os"
)

func tryPluginStateFileLock(*os.File) (bool, error) {
	return false, errors.New("plugin state locking is unsupported on this platform")
}

func unlockPluginStateFile(*os.File) error { return nil }
