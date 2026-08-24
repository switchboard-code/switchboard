//go:build !unix && !windows

package main

import (
	"fmt"
	"os"
	"runtime"
)

func openWorkspaceBoundedRead(*os.Root, string) (*os.File, error) {
	return nil, fmt.Errorf("secure workspace reads are unsupported on %s", runtime.GOOS)
}
