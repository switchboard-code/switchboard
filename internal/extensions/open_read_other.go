//go:build !unix && !windows

package extensions

import (
	"fmt"
	"os"
	"runtime"
)

func openExtensionRootRead(*os.Root, string) (*os.File, error) {
	return nil, fmt.Errorf("secure extension reads are unsupported on %s", runtime.GOOS)
}
