//go:build !unix && !windows

package skills

import (
	"fmt"
	"os"
	"runtime"
)

func openRootedRead(_ *os.Root, _ string) (*os.File, error) {
	return nil, fmt.Errorf("secure skill reads are unsupported on %s", runtime.GOOS)
}

func openSkillPathRead(string) (*os.File, error) {
	return nil, fmt.Errorf("secure skill reads are unsupported on %s", runtime.GOOS)
}
