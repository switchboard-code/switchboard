//go:build !unix && !windows

package tools

import (
	"fmt"
	"os"
	"runtime"
)

func openWorkspaceRead(*os.Root, string) (*os.File, error) {
	return nil, fmt.Errorf("secure workspace reads are unsupported on %s", runtime.GOOS)
}
