//go:build !windows

package checkpoint

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// publishDurableUndoJournal gives the complete temporary inode its fixed
// recovery name without replacement. Linux renameat2(RENAME_NOREPLACE) and
// Darwin renameatx_np(RENAME_EXCL) move the one exact preparation name rather
// than creating a second hardlink that would defeat safe scrubbing at final
// retirement. Other Unix targets fail closed in moveNoReplaceBoundNames.
func publishDurableUndoJournal(file *os.File, from, to string) (durableUndoPublication, error) {
	scope, err := openRestoreScope(filepath.Dir(to))
	if err != nil {
		return durableUndoPublication{}, err
	}
	defer scope.close()
	if err := moveNoReplaceBoundNames(scope.root, filepath.Base(from), filepath.Base(to)); err != nil {
		return durableUndoPublication{}, err
	}
	outcome := durableUndoPublication{published: true}
	opened, err := file.Stat()
	linked, linkErr := scope.root.Lstat(filepath.Base(to))
	if err != nil || linkErr != nil || !linked.Mode().IsRegular() || !os.SameFile(opened, linked) {
		return outcome, errors.Join(err, linkErr,
			fmt.Errorf("%w: published retry journal is not the selected inode", ErrStale))
	}
	outcome.exactAtDestination = true
	return outcome, syncBoundDirectory(scope.root)
}
