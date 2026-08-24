//go:build !unix && !windows

package rootedfs

import (
	"fmt"
	"os"
	"runtime"
)

func openRootedRead(*os.Root, string) (*os.File, error) {
	return nil, fmt.Errorf("secure rooted reads are unsupported on %s", runtime.GOOS)
}
