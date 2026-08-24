//go:build !darwin && !linux && !windows

package schedule

import (
	"fmt"
	"os"
	"runtime"
)

func moveScheduleNoReplace(*os.Root, *os.Root, string, string, *os.File) (bool, error) {
	return false, fmt.Errorf("atomic no-replace schedule cleanup is unsupported on %s", runtime.GOOS)
}
