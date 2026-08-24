//go:build !windows

package session

import (
	"errors"
	"os"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

func syncSessionDirectory(path string) error {
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

func syncOpenedSessionDirectory(dir *os.File) error { return dir.Sync() }
