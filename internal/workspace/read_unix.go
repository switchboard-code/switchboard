//go:build unix

package workspace

import (
	"os"

	"golang.org/x/sys/unix"
)

// os.Root walks descendants from its stable directory descriptor with
// no-follow opens and bounded symlink resolution. O_NONBLOCK keeps a FIFO or
// device swapped into the final component from hanging before the common
// regular-file check.
func openWorkspaceReadFile(rootPath string, identity os.FileInfo, relative string) (*os.File, error) {
	rooted, err := openVerifiedWorkspaceRoot(rootPath, identity)
	if err != nil {
		return nil, err
	}
	file, openErr := rooted.OpenFile(relative, os.O_RDONLY|unix.O_NONBLOCK, 0)
	closeErr := rooted.Close()
	if openErr != nil {
		return nil, openErr
	}
	if closeErr != nil {
		_ = file.Close()
		return nil, closeErr
	}
	return file, nil
}
