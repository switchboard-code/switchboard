//go:build unix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package checkpoint

import (
	"errors"
	"os"
	"runtime"
)

func linkBoundNames(*os.Root, string, string) error {
	return errors.New("secure no-replace checkpoint publication is unavailable on " + runtime.GOOS)
}
