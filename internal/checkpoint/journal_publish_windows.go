//go:build windows

package checkpoint

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// MOVEFILE_WRITE_THROUGH is Windows' durability boundary for the directory
// entry created by a rename. Omitting REPLACE_EXISTING preserves the journal's
// single-owner/no-clobber contract.
func publishDurableUndoJournal(file *os.File, from, to string) (durableUndoPublication, error) {
	scope, err := openRestoreScope(filepath.Dir(to))
	if err != nil {
		return durableUndoPublication{}, err
	}
	defer scope.close()
	published, renameErr := renameBoundOpenFile(scope.root, file, nil, filepath.Base(from), filepath.Base(to), false)
	outcome := durableUndoPublication{published: published}
	if !published {
		return outcome, renameErr
	}
	opened, statErr := file.Stat()
	linked, linkErr := scope.root.Lstat(filepath.Base(to))
	if statErr != nil || linkErr != nil || !linked.Mode().IsRegular() || !os.SameFile(opened, linked) {
		return outcome, errors.Join(renameErr, statErr, linkErr,
			fmt.Errorf("%w: published retry journal is not the selected inode", ErrStale))
	}
	outcome.exactAtDestination = true
	return outcome, renameErr
}
