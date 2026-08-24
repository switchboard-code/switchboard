//go:build unix

package delegate

import (
	"os"

	"golang.org/x/sys/unix"
)

// openRootedRead keeps the acquisition rooted and nonblocking. The latter is
// essential because a writable checkout can replace a previously inspected
// file or queued directory with a FIFO before the descriptor is acquired.
func openRootedRead(root *os.Root, relative string) (*os.File, error) {
	return root.OpenFile(relative, os.O_RDONLY|unix.O_NONBLOCK, 0)
}
