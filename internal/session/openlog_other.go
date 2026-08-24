//go:build !unix && !windows

package session

import (
	"fmt"
	"os"
	"runtime"
)

func openSessionLogDescriptor(path string, writable bool) (*os.File, error) {
	return nil, fmt.Errorf("secure session log open is unsupported on %s", runtime.GOOS)
}

func sessionLogLinkCount(f *os.File) (uint64, error) {
	return 0, fmt.Errorf("secure session log link-count checks are unsupported on %s", runtime.GOOS)
}
