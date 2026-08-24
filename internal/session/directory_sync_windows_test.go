//go:build windows

package session

import "testing"

func TestSyncSessionDirectoryUsesSupportedWindowsSemantics(t *testing.T) {
	// The Windows seam is deliberately independent of opening a directory:
	// os.File.Sync on such a handle can return ERROR_INVALID_FUNCTION.
	if err := syncSessionDirectory(`Z:\path\does\not\need\to\exist`); err != nil {
		t.Fatalf("syncSessionDirectory on Windows = %v, want supported no-op", err)
	}
}
