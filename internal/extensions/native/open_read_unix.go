//go:build unix

package native

import (
	"os"

	"golang.org/x/sys/unix"
)

func openNativePathRead(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
}

func openNativeRootRead(root *os.Root, relative string) (*os.File, error) {
	return root.OpenFile(relative, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
}
