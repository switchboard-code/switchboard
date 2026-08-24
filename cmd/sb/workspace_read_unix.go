//go:build unix

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func openWorkspaceBoundedRead(root *os.Root, path string) (*os.File, error) {
	return root.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0)
}
