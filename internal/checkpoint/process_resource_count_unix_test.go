//go:build darwin || linux

package checkpoint

import (
	"errors"
	"os"
)

func checkpointProcessResourceCount() (int, error) {
	directory, err := os.Open("/dev/fd")
	if err != nil {
		return 0, err
	}
	names, readErr := directory.Readdirnames(-1)
	return len(names), errors.Join(readErr, directory.Close())
}
