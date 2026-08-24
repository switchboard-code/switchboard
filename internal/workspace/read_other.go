//go:build !unix && !windows

package workspace

import (
	"fmt"
	"os"
	"runtime"
)

// Unknown platforms fail closed until they have a handle-based containment
// implementation. Falling back to os.Open would restore the check/read race.
func openWorkspaceReadFile(_ string, _ os.FileInfo, _ string) (*os.File, error) {
	return nil, fmt.Errorf("%w on %s", ErrSecureReadUnsupported, runtime.GOOS)
}
