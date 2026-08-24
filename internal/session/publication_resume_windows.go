//go:build windows

package session

import "os"

func openRootedPublicationMarkerDescriptor(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDWR, 0)
}
