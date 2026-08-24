//go:build windows

package bisect

import "os"

func openBisectRead(root *os.Root, path string) (*os.File, error) { return root.Open(path) }
