//go:build !unix && !windows

package session

import (
	"fmt"
	"os"
	"runtime"
)

func openRootedPublicationMarkerDescriptor(*os.Root, string) (*os.File, error) {
	return nil, fmt.Errorf("secure publication marker open is unsupported on %s", runtime.GOOS)
}
