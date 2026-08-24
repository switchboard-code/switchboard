//go:build !unix && !windows

package bisect

import (
	"errors"
	"os"
)

func acquireBisectLock(*os.File) error {
	return errors.New("cross-process bisect locking is unsupported on this platform")
}

func releaseBisectLock(*os.File) error { return nil }
