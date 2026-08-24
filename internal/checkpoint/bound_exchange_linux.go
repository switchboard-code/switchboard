//go:build linux

package checkpoint

import (
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

func exchangeBoundNames(root *os.Root, left, right string) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	err = unix.Renameat2(int(directory.Fd()), left, int(directory.Fd()), right, unix.RENAME_EXCHANGE)
	runtime.KeepAlive(directory)
	return err
}

func moveNoReplaceBoundNames(root *os.Root, from, to string) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	err = unix.Renameat2(int(directory.Fd()), from, int(directory.Fd()), to, unix.RENAME_NOREPLACE)
	runtime.KeepAlive(directory)
	return err
}
