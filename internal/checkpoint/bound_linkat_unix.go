//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package checkpoint

import (
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

func linkBoundNames(root *os.Root, from, to string) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	err = unix.Linkat(int(directory.Fd()), from, int(directory.Fd()), to, 0)
	runtime.KeepAlive(directory)
	return err
}
