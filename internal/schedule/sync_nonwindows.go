//go:build !windows

package schedule

import (
	"errors"
	"os"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

func syncScheduleDirectory(path string) error {
	root, err := rootedfs.OpenRoot(path)
	if err != nil {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return errors.Join(err, root.Close())
	}
	return errors.Join(dir.Sync(), dir.Close(), root.Close())
}

func syncScheduleRoot(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
