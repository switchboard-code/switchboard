//go:build unix

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// Nonblocking, no-follow open makes a file swapped to a FIFO, device, or
// symlink fail before it can stall discovery or expose the target's bytes.
func openCustomCommandFile(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
}
