//go:build !unix && !windows

package checkpoint

import "testing"

func TestCheckpointRestoreFailsClosedWithoutHandleRelativeFilesystemAPIs(t *testing.T) {
	if err := validateBoundRestorePlatform(); err == nil {
		t.Fatal("unsupported platform claimed secure checkpoint restore support")
	}
}
