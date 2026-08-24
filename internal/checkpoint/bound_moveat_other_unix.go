//go:build unix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package checkpoint

import (
	"errors"
	"os"
	"runtime"
)

func moveBoundNameTo(*os.Root, *os.Root, string, string) error {
	return errors.New("secure cross-directory checkpoint retirement is unavailable on " + runtime.GOOS)
}
