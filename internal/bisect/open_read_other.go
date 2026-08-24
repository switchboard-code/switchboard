//go:build !unix && !windows

package bisect

import (
	"fmt"
	"os"
	"runtime"
)

func openBisectRead(*os.Root, string) (*os.File, error) {
	return nil, fmt.Errorf("secure bisect reads are unsupported on %s", runtime.GOOS)
}
