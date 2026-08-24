//go:build unix && !darwin && !linux

package checkpoint

import (
	"errors"
	"os"
	"runtime"
)

func exchangeBoundNames(*os.Root, string, string) error {
	return errors.New("secure checkpoint exchange is unavailable on " + runtime.GOOS)
}

func moveNoReplaceBoundNames(*os.Root, string, string) error {
	return errors.New("secure no-replace checkpoint move is unavailable on " + runtime.GOOS)
}
