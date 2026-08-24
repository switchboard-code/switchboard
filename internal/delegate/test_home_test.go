package delegate

import (
	"path/filepath"
	"strings"
	"testing"
)

func isolateTestHome(t *testing.T, home string) {
	t.Helper()
	volume := filepath.VolumeName(home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOMEDRIVE", volume)
	t.Setenv("HOMEPATH", strings.TrimPrefix(home, volume))
}
