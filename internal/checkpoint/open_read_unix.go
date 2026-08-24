//go:build unix

package checkpoint

import (
	"os"

	"golang.org/x/sys/unix"
)

// Checkpoint reads sit on compare-and-swap and recovery paths. A workspace
// writer must not be able to turn their Lstat/open seam into a FIFO wait.
func openCheckpointPathRead(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
}

func openCheckpointRootRead(root *os.Root, relative string) (*os.File, error) {
	return root.OpenFile(relative, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
}
