//go:build windows

package delegate

import "os"

// Windows' os.Root opens descendants handle-relatively, rejects reserved
// devices, and does not have Unix FIFO open semantics.
func openRootedRead(root *os.Root, relative string) (*os.File, error) {
	return root.Open(relative)
}
