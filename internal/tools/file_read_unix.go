//go:build unix

package tools

import (
	"os"

	"golang.org/x/sys/unix"
)

func openWorkspaceRead(root *os.Root, relative string) (*os.File, error) {
	return root.OpenFile(relative, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
}
