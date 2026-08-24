//go:build windows

package agent

import "os"

// Windows' os.Root uses handle-relative NtCreateFile calls with
// O_NOFOLLOW_ANY and rejects reserved device names in the standard library.
func openInstructionReadFile(root *os.Root, relative string) (*os.File, error) {
	return root.Open(relative)
}
