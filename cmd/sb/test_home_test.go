package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// isolateTestHome keeps home-directory discovery away from the developer's
// real configuration on every supported platform. Windows' os.UserHomeDir
// prefers USERPROFILE and can fall back to HOMEDRIVE+HOMEPATH.
func isolateTestHome(t *testing.T, home string) {
	t.Helper()
	volume := filepath.VolumeName(home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOMEDRIVE", volume)
	t.Setenv("HOMEPATH", strings.TrimPrefix(home, volume))
}
