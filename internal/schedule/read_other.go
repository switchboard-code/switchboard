//go:build !unix && !windows

package schedule

import (
	"fmt"
	"os"
	"runtime"
)

func openScheduleRead(*os.Root, string) (*os.File, error) {
	return nil, fmt.Errorf("secure schedule reads are unsupported on %s", runtime.GOOS)
}
