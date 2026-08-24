package fileprivacy

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

func validateRootLeaf(root *os.Root, name string) error {
	if root == nil {
		return errors.New("owner-private file has no directory capability")
	}
	if name == "" || !filepath.IsLocal(name) || filepath.Base(name) != name || strings.ContainsAny(name, `/\`) {
		return errors.New("owner-private file has an invalid leaf name")
	}
	return nil
}

// EnsurePrivateDirInRoot creates or secures one literal directory beneath a
// retained parent capability and returns a retained capability for that exact
// directory. Security is applied through the opened directory descriptor, so
// a parent rename or final-component substitution cannot redirect it.
func EnsurePrivateDirInRoot(root *os.Root, name string) (*os.Root, error) {
	if err := validateRootLeaf(root, name); err != nil {
		return nil, err
	}
	if err := root.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	before, err := root.Lstat(name)
	if err != nil || before.Mode()&fs.ModeSymlink != 0 || !before.IsDir() {
		return nil, errors.Join(errors.New("owner-private directory is not a physical directory"), err)
	}
	directory, err := openPrivateDirectoryInRoot(root, name)
	if err != nil {
		return nil, err
	}
	opened, statErr := directory.Stat()
	if statErr != nil || !opened.IsDir() || !os.SameFile(before, opened) {
		_ = directory.Close()
		return nil, errors.Join(errors.New("owner-private directory changed while it was opened"), statErr)
	}
	if err := SecureDirectory(directory); err != nil {
		_ = directory.Close()
		return nil, err
	}
	if err := directory.Close(); err != nil {
		return nil, err
	}
	child, err := rootedfs.OpenRootAt(root, name)
	if err != nil {
		return nil, err
	}
	linked, linkErr := root.Lstat(name)
	bound, boundErr := child.Stat(".")
	if linkErr != nil || boundErr != nil || !linked.IsDir() || !bound.IsDir() ||
		!os.SameFile(opened, linked) || !os.SameFile(linked, bound) {
		return nil, errors.Join(errors.New("owner-private directory changed while it was retained"), linkErr, boundErr, child.Close())
	}
	return child, nil
}
