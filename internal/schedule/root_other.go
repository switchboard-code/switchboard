//go:build !windows

package schedule

import (
	"os"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

func openScheduleRoot(path string) (*os.Root, error) {
	return rootedfs.OpenRoot(path)
}
