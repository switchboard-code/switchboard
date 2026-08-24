//go:build windows

package extensions

import "os"

func openExtensionRootRead(root *os.Root, relative string) (*os.File, error) {
	return root.Open(relative)
}
