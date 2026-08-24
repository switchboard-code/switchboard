//go:build unix

package skills

import (
	"os"

	"golang.org/x/sys/unix"
)

// openRootedRead keeps both skill/command files and command directories from
// blocking when a writable tree swaps a checked path to a FIFO.
func openRootedRead(root *os.Root, relative string) (*os.File, error) {
	return root.OpenFile(relative, os.O_RDONLY|unix.O_NONBLOCK, 0)
}

func openSkillPathRead(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
}
