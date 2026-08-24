package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// installUpdateBinary stages bytes beside the executable, syncs their content
// and mode, then delegates the one platform-specific namespace transition.
// The same-directory staging is what makes the Unix publication atomic and
// the Windows backup rollback possible.
func installUpdateBinary(exe string, binary []byte) (err error) {
	return installUpdateBinaryWithReplace(exe, binary, replaceExecutable)
}

// installUpdateBinaryWithReplace keeps the staging cleanup and publication
// boundary testable without changing the platform implementation selected by
// the build. publish consumes the staged path only on success.
func installUpdateBinaryWithReplace(exe string, binary []byte, publish func(string, string) error) (err error) {
	if len(binary) == 0 {
		return errors.New("refusing to install an empty update binary")
	}
	if publish == nil {
		return errors.New("update publication is unavailable")
	}
	info, err := os.Lstat(exe)
	if err != nil {
		return fmt.Errorf("inspecting current executable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("current executable is not a regular file")
	}

	tmp, err := os.CreateTemp(filepath.Dir(exe), ".sb-update-*")
	if err != nil {
		return fmt.Errorf("creating staged update: %w", err)
	}
	tmpPath := tmp.Name()
	consumed := false
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, tmp.Close())
		}
		if !consumed {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		return fmt.Errorf("setting staged update mode: %w", err)
	}
	if _, err := tmp.Write(binary); err != nil {
		return fmt.Errorf("writing staged update: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("syncing staged update: %w", err)
	}
	closeErr := tmp.Close()
	closed = true
	if closeErr != nil {
		return fmt.Errorf("closing staged update: %w", closeErr)
	}
	if err := publish(exe, tmpPath); err != nil {
		return err
	}
	consumed = true
	return nil
}

// replaceExecutableWithBackup is the Windows namespace transaction separated
// from MoveFileEx so rollback is testable on every development platform. The
// backup name is never overwritten: a stale or foreign occupant makes the
// update fail before the current executable moves.
func replaceExecutableWithBackup(exe, staged string, move func(string, string) error) error {
	if move == nil {
		return errors.New("update backup move is unavailable")
	}
	old := exe + ".old"
	if _, err := os.Lstat(old); err == nil {
		return fmt.Errorf("previous update backup %s still exists", old)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspecting previous update backup: %w", err)
	}
	if err := move(exe, old); err != nil {
		return fmt.Errorf("moving current executable to its update backup: %w", err)
	}
	if err := move(staged, exe); err != nil {
		rollbackErr := move(old, exe)
		if rollbackErr != nil {
			return errors.Join(
				fmt.Errorf("publishing staged update: %w", err),
				fmt.Errorf("restoring current executable from %s: %w", old, rollbackErr),
			)
		}
		return fmt.Errorf("publishing staged update (current executable restored): %w", err)
	}
	return nil
}
