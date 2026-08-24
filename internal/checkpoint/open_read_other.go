//go:build !unix && !windows

package checkpoint

import (
	"fmt"
	"os"
	"runtime"
)

func openCheckpointPathRead(string) (*os.File, error) {
	return nil, fmt.Errorf("secure checkpoint reads are unsupported on %s", runtime.GOOS)
}

func openCheckpointRootRead(*os.Root, string) (*os.File, error) {
	return nil, fmt.Errorf("secure checkpoint reads are unsupported on %s", runtime.GOOS)
}
