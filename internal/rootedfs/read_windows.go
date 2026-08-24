//go:build windows

package rootedfs

import "os"

func openRootedRead(root *os.Root, name string) (*os.File, error) { return root.Open(name) }
