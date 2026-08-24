//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package checkpoint

import (
	"errors"
	"os"
)

// A no-op lock would let recovery race a live writer. Unsupported platforms
// therefore refuse durable retry instead of weakening its single-owner rule.
func acquireJournalLock(*os.File) error {
	return errors.New("durable retry locking is unsupported on this platform")
}
