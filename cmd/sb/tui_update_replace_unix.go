//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

func replaceExecutable(exe, staged string) error {
	if err := os.Rename(staged, exe); err != nil {
		return fmt.Errorf("publishing staged update: %w", err)
	}
	root, err := rootedfs.OpenRoot(filepath.Dir(exe))
	if err != nil {
		return fmt.Errorf("updated executable is visible but its directory could not be opened for sync: %w", err)
	}
	dir, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("updated executable is visible but its directory could not be opened for sync: %w", errors.Join(err, root.Close()))
	}
	if err := errors.Join(dir.Sync(), dir.Close(), root.Close()); err != nil {
		return fmt.Errorf("updated executable is visible but its directory could not be synced: %w", err)
	}
	return nil
}
