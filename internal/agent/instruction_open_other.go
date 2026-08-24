//go:build !unix && !windows

package agent

import (
	"fmt"
	"os"
	"runtime"
)

// Unknown platforms fail closed until they have a handle-relative,
// nonblocking opener. Falling back to a pathname open would restore either
// the containment race or the FIFO startup hang this boundary prevents.
func openInstructionReadFile(_ *os.Root, _ string) (*os.File, error) {
	return nil, fmt.Errorf("secure instruction reads are unsupported on %s", runtime.GOOS)
}
