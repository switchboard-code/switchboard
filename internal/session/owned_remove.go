package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/switchboard-code/switchboard/internal/rootedfs"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
)

var ownedRemoveBeforeRestoreTestHook func(string)
var ownedRemoveBeforeCandidateOpenTestHook func(string)

// removePathIfSame removes a pathname only after atomically moving the entry
// selected at removal time into a private sibling directory and comparing its
// identity with the descriptor-derived identity the caller owns. A pathname
// replacement can therefore make cleanup fail, but it cannot make cleanup
// delete the replacement.
func removePathIfSame(path string, expected os.FileInfo) error {
	if expected == nil || !expected.Mode().IsRegular() {
		return fmt.Errorf("refusing to remove %s without a regular opened identity", path)
	}
	quarantine, err := createPrivateSessionTempDir(filepath.Dir(path), ".session-remove-")
	if err != nil {
		return fmt.Errorf("preparing owned removal for %s: %w", path, err)
	}
	movedPath := filepath.Join(quarantine, "entry")
	cleanupDir := true
	defer func() {
		if cleanupDir {
			_ = os.Remove(quarantine)
		}
	}()
	parentPath := filepath.Dir(path)
	parent, err := rootedfs.OpenRoot(parentPath)
	if err != nil {
		return fmt.Errorf("binding parent for owned removal of %s: %w", path, err)
	}
	defer parent.Close()
	from := filepath.Join(filepath.Base(quarantine), "entry")
	to := filepath.Base(path)

	if err := parent.Rename(to, from); err != nil {
		return fmt.Errorf("moving %s for identity-checked removal: %w", path, err)
	}
	moved, statErr := os.Lstat(movedPath)
	if statErr != nil || !moved.Mode().IsRegular() || !os.SameFile(expected, moved) {
		identityErr := fmt.Errorf("%s changed before identity-checked removal", path)
		if statErr != nil {
			identityErr = errors.Join(identityErr, statErr)
		}
		// Put a replacement back when the original name is still free. If a
		// concurrent writer has already occupied it, retain the moved entry in
		// quarantine rather than overwriting or deleting either file.
		if _, currentErr := os.Lstat(path); errors.Is(currentErr, os.ErrNotExist) {
			if ownedRemoveBeforeCandidateOpenTestHook != nil {
				ownedRemoveBeforeCandidateOpenTestHook(movedPath)
			}
			movedFile, openErr := openSessionLogDescriptor(movedPath, false)
			if openErr != nil {
				cleanupDir = false
				return errors.Join(identityErr, fmt.Errorf("replacement retained at %s: %w", movedPath, openErr))
			}
			opened, statErr := movedFile.Stat()
			currentMoved, linkedErr := os.Lstat(movedPath)
			if statErr != nil || linkedErr != nil || !opened.Mode().IsRegular() ||
				!currentMoved.Mode().IsRegular() || !os.SameFile(moved, opened) || !os.SameFile(opened, currentMoved) {
				cleanupDir = false
				return errors.Join(identityErr, statErr, linkedErr, movedFile.Close(),
					fmt.Errorf("replacement retained at %s because its recovery candidate changed identity", movedPath))
			}
			if ownedRemoveBeforeRestoreTestHook != nil {
				ownedRemoveBeforeRestoreTestHook(path)
			}
			outcome, restoreErr := checkpoint.MoveOpenFileNoReplace(parent, movedFile, from, to)
			closeErr := movedFile.Close()
			if !outcome.Published {
				cleanupDir = false
				if restoreErr == nil {
					restoreErr = errors.New("atomic no-replace restore did not publish")
				}
				return errors.Join(identityErr,
					fmt.Errorf("replacement retained at %s after restore refusal: %w", movedPath, restoreErr), closeErr)
			}
			if restoreErr != nil || closeErr != nil {
				if restoreErr != nil {
					restoreErr = fmt.Errorf("replacement was restored but its durability could not be confirmed: %w", restoreErr)
				}
				if closeErr != nil {
					closeErr = fmt.Errorf("closing restored replacement: %w", closeErr)
				}
				return errors.Join(identityErr, restoreErr, closeErr)
			}
		} else {
			cleanupDir = false
			if currentErr != nil {
				identityErr = errors.Join(identityErr, currentErr)
			}
			return errors.Join(identityErr, fmt.Errorf("replacement retained at %s", movedPath))
		}
		return identityErr
	}
	if err := os.Remove(movedPath); err != nil {
		cleanupDir = false
		return fmt.Errorf("removing owned %s retained at %s: %w", path, movedPath, err)
	}
	if err := os.Remove(quarantine); err != nil {
		cleanupDir = false
		return fmt.Errorf("removing owned-path quarantine %s: %w", quarantine, err)
	}
	cleanupDir = false
	if err := syncSessionDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("syncing removal of %s: %w", path, err)
	}
	return nil
}
