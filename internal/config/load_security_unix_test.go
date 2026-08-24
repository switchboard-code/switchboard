//go:build unix

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func makeLegacyBroadConfigForTest(path string) error {
	return os.Chmod(path, 0o644)
}

func TestLoadFileFIFOIsNonblocking(t *testing.T) {
	path := write(t, configSecurityFixture)
	displaced := filepath.Join(filepath.Dir(path), "displaced.toml")
	var hookErr error
	loadFileAfterInitialInspection = func() {
		loadFileAfterInitialInspection = nil
		if hookErr = os.Rename(path, displaced); hookErr != nil {
			return
		}
		hookErr = unix.Mkfifo(path, 0o600)
	}
	t.Cleanup(func() { loadFileAfterInitialInspection = nil })
	done := make(chan error, 1)
	go func() {
		_, err := LoadFile(path)
		done <- err
	}()
	select {
	case err := <-done:
		if hookErr != nil {
			t.Fatal(hookErr)
		}
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "not regular") {
			t.Fatalf("FIFO config error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("LoadFile blocked opening a FIFO")
	}
}

func TestLoadFileRejectsForeignOwnerWhenTestCanCreateOne(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing a fixture owner requires root")
	}
	path := write(t, configSecurityFixture)
	if err := os.Chown(path, 65534, 65534); err != nil {
		t.Skipf("cannot assign the nobody uid: %v", err)
	}
	if _, err := LoadFile(path); err == nil || !strings.Contains(err.Error(), "not owned by the current user") {
		t.Fatalf("foreign-owned config error = %v", err)
	}
}

func TestLoadFileRejectsUnrepairableBroadPermissions(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can open a mode-0444 fixture for repair")
	}
	path := write(t, configSecurityFixture)
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err == nil || !strings.Contains(err.Error(), "could not be opened for repair") {
		t.Fatalf("unrepairable config error = %v", err)
	}
}
