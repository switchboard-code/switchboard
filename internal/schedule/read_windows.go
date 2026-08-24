//go:build windows

package schedule

import "os"

func openScheduleRead(root *os.Root, name string) (*os.File, error) {
	return root.Open(name)
}
