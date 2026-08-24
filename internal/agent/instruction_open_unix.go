//go:build unix

package agent

import (
	"os"

	"golang.org/x/sys/unix"
)

// os.Root keeps the open confined to the anchored instruction tree.
// O_NONBLOCK closes the Lstat/open race in which a checkout replaces the
// inspected regular file with a FIFO or device and otherwise hangs startup
// before the descriptor-level regular-file check can reject it.
func openInstructionReadFile(root *os.Root, relative string) (*os.File, error) {
	return root.OpenFile(relative, os.O_RDONLY|unix.O_NONBLOCK, 0)
}
