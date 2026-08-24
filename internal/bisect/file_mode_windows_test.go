//go:build windows

package bisect

import (
	"io/fs"
	"testing"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
)

func TestWindowsNormalizeStateAcceptsLegacyPermissionMask(t *testing.T) {
	state, err := normalizeState(checkpoint.FileState{
		Existed: true,
		Mode:    0o644,
		Content: []byte("before"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.Mode != 0o666 {
		t.Fatalf("legacy mode normalized to %o, want 666", state.Mode)
	}
	if _, err := normalizeState(checkpoint.FileState{
		Existed: true,
		Mode:    fs.ModeDir | 0o644,
		Content: []byte("before"),
	}); err == nil {
		t.Fatal("normalizeState accepted file-type bits")
	}
}
