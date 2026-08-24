//go:build unix

package native

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func awaitNativeExtensionRead(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("native extension discovery blocked on a FIFO")
		return nil
	}
}

func replaceNativePathWithFIFO(path string) error {
	if err := os.Remove(path); err != nil {
		return err
	}
	return unix.Mkfifo(path, 0o600)
}

func TestNativeExtensionReadsRejectDirectAndSwappedFIFOsWithoutBlocking(t *testing.T) {
	t.Run("direct config", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := unix.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			_, err := readBoundedFile(path, maxConfigBytes)
			result <- err
		}()
		if err := awaitNativeExtensionRead(t, result); err == nil {
			t.Fatal("direct FIFO was accepted as native extension config")
		}
	})

	t.Run("swapped config", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		var swapErr error
		result := make(chan error, 1)
		go func() {
			_, err := readBoundedFileWithHook(path, maxConfigBytes, func() {
				swapErr = replaceNativePathWithFIFO(path)
			})
			result <- err
		}()
		if err := awaitNativeExtensionRead(t, result); err == nil {
			t.Fatal("FIFO replacement was accepted as native extension config")
		}
		if swapErr != nil {
			t.Fatal(swapErr)
		}
	})

	t.Run("swapped rooted manifest", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "plugin.json")
		if err := os.WriteFile(path, []byte(`{"name":"safe"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		var swapErr error
		result := make(chan error, 1)
		go func() {
			_, err := readWithinWithHook(dir, "plugin.json", maxConfigBytes, func() {
				swapErr = replaceNativePathWithFIFO(path)
			})
			result <- err
		}()
		if err := awaitNativeExtensionRead(t, result); err == nil {
			t.Fatal("FIFO replacement was accepted as a native plugin manifest")
		}
		if swapErr != nil {
			t.Fatal(swapErr)
		}
	})
}
