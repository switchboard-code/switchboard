package rootedfs

import (
	"errors"
	"os"
	"strings"
)

// OpenRoot opens name as a directory capability without first opening an
// attacker-controlled final component as an ordinary read-only file. The
// standard os.OpenRoot implementation verifies the object type only after its
// open; on Unix that can block forever when a checked directory is replaced by
// a FIFO. A literal directory-self suffix makes name an intermediate directory
// lookup, so the kernel rejects a FIFO before opening it.
func OpenRoot(name string) (*os.Root, error) {
	if name == "" {
		return nil, errors.New("directory path is empty")
	}
	return os.OpenRoot(directorySelf(name))
}

// OpenRootAt is the descendant form of OpenRoot.
func OpenRootAt(root *os.Root, name string) (*os.Root, error) {
	if root == nil {
		return nil, errors.New("directory root is nil")
	}
	if name == "" {
		return nil, errors.New("relative directory path is empty")
	}
	return root.OpenRoot(directorySelf(name))
}

func directorySelf(name string) string {
	separator := string(os.PathSeparator)
	if strings.HasSuffix(name, separator) {
		return name + "."
	}
	return name + separator + "."
}
