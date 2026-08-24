//go:build unix

package extensions

import (
	"os"

	"golang.org/x/sys/unix"
)

func openExtensionRootRead(root *os.Root, relative string) (*os.File, error) {
	return root.OpenFile(relative, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
}
