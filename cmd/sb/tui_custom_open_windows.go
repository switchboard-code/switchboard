//go:build windows

package main

import "os"

// os.Root uses handle-relative NtCreateFile on Windows and refuses reserved
// device names. The common reader binds the result to the preceding Lstat.
func openCustomCommandFile(root *os.Root, name string) (*os.File, error) {
	return root.Open(name)
}
