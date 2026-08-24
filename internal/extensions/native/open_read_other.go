//go:build !unix && !windows

package native

import (
	"fmt"
	"os"
	"runtime"
)

func openNativePathRead(string) (*os.File, error) {
	return nil, fmt.Errorf("secure native extension reads are unsupported on %s", runtime.GOOS)
}

func openNativeRootRead(*os.Root, string) (*os.File, error) {
	return nil, fmt.Errorf("secure native extension reads are unsupported on %s", runtime.GOOS)
}
