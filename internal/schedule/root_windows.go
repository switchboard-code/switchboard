//go:build windows

package schedule

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

// os.OpenRoot opens an absolute Windows path through CreateFile, whose legacy
// sharing mask omits FILE_SHARE_DELETE. Binding a physical leaf through its
// parent instead uses Root.OpenRoot's NtCreateFile path and admits namespace
// renames. That matters here: the retained root must keep authority over the
// original directory if its old spelling is removed or replaced, rather than
// making the replacement impossible and hiding the race we are meant to
// handle. A final symlink still uses the absolute opener so its target is
// followed once; bindScheduleDirectory proves the resulting object against
// the pre-open identity before it is accepted.
func openScheduleRoot(path string) (*os.Root, error) {
	linked, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if linked.Mode()&os.ModeSymlink != 0 {
		return rootedfs.OpenRoot(path)
	}
	parentPath := filepath.Dir(path)
	leaf := filepath.Base(path)
	if parentPath == path || leaf == "." || leaf == string(os.PathSeparator) {
		return rootedfs.OpenRoot(path)
	}
	parent, err := rootedfs.OpenRoot(parentPath)
	if err != nil {
		return nil, err
	}
	root, openErr := rootedfs.OpenRootAt(parent, leaf)
	closeErr := parent.Close()
	if openErr != nil || closeErr != nil {
		if root != nil {
			_ = root.Close()
		}
		return nil, errors.Join(openErr, closeErr)
	}
	return root, nil
}
