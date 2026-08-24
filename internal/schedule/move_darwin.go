//go:build darwin

package schedule

import (
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

func moveScheduleNoReplace(source, destination *os.Root, from, to string, _ *os.File) (bool, error) {
	sourceDir, err := source.Open(".")
	if err != nil {
		return false, err
	}
	defer sourceDir.Close()
	destinationDir, err := destination.Open(".")
	if err != nil {
		return false, err
	}
	defer destinationDir.Close()
	err = unix.RenameatxNp(
		int(sourceDir.Fd()), from,
		int(destinationDir.Fd()), to,
		unix.RENAME_EXCL,
	)
	runtime.KeepAlive(sourceDir)
	runtime.KeepAlive(destinationDir)
	return err == nil, err
}
