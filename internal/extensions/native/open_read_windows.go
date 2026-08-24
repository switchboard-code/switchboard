//go:build windows

package native

import "os"

func openNativePathRead(path string) (*os.File, error) {
	return os.Open(path)
}

func openNativeRootRead(root *os.Root, relative string) (*os.File, error) {
	return root.Open(relative)
}
