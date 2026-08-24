//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package checkpoint

import (
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

func moveBoundNameTo(source, destination *os.Root, from, to string) error {
	sourceDirectory, err := source.Open(".")
	if err != nil {
		return err
	}
	defer sourceDirectory.Close()
	destinationDirectory, err := destination.Open(".")
	if err != nil {
		return err
	}
	defer destinationDirectory.Close()
	err = unix.Renameat(int(sourceDirectory.Fd()), from, int(destinationDirectory.Fd()), to)
	runtime.KeepAlive(sourceDirectory)
	runtime.KeepAlive(destinationDirectory)
	return err
}
