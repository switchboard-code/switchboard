//go:build windows

package tools

import "os"

func replaceMutationPath(parent *os.Root, from, to string) error {
	return parent.Rename(from, to)
}

func syncMutationDirectory(*os.Root) error { return nil }
