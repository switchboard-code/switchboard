//go:build !unix && !windows

package delegate

import (
	"fmt"
	"os"
	"runtime"
)

func openRootedRead(_ *os.Root, _ string) (*os.File, error) {
	return nil, fmt.Errorf("secure definition reads are unsupported on %s", runtime.GOOS)
}
