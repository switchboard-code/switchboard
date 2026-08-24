//go:build unix

package session

import (
	"os"

	"golang.org/x/sys/unix"
)

func openRootedPublicationMarkerDescriptor(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDWR|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
}
