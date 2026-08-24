//go:build windows

package checkpoint

import "os"

func openCheckpointPathRead(path string) (*os.File, error) {
	return os.Open(path)
}

// Windows' os.Root opens descendants handle-relatively, refuses reserved
// devices, and does not have Unix FIFO open semantics.
func openCheckpointRootRead(root *os.Root, relative string) (*os.File, error) {
	return root.Open(relative)
}
