//go:build unix

package bisect

import (
	"os"

	"golang.org/x/sys/unix"
)

func openBisectRead(root *os.Root, path string) (*os.File, error) {
	return root.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
}
