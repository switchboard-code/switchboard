//go:build aix || illumos || solaris

package extensions

import (
	"errors"
	"os"
)

func tryPluginStateFileLock(*os.File) (bool, error) {
	return false, errors.New("plugin state locking requires open-file-description locks unavailable on this platform")
}

func unlockPluginStateFile(*os.File) error { return nil }
