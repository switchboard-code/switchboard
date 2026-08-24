//go:build !windows

package tools

import "os"

func replaceMutationPath(parent *os.Root, from, to string) error {
	return parent.Rename(from, to)
}

func syncMutationDirectory(parent *os.Root) error {
	dir, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
