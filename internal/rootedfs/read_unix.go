//go:build unix

package rootedfs

import (
	"os"

	"golang.org/x/sys/unix"
)

func openRootedRead(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
}
