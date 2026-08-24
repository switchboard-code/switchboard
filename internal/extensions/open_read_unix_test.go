//go:build unix

package extensions

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func awaitExtensionRead(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("extension discovery blocked on a FIFO")
		return nil
	}
}

func TestExtensionInstallCopyDoesNotBlockOnFIFOReplacement(t *testing.T) {
	sourceDir := t.TempDir()
	destinationDir := t.TempDir()
	path := filepath.Join(sourceDir, "tool.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	expected, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.OpenRoot(sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	destination, err := os.OpenRoot(destinationDir)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()

	var swapErr error
	result := make(chan error, 1)
	go func() {
		_, err := copyInstallFileContextWithHook(context.Background(), source, destination,
			"tool.sh", "tool.sh", expected, maxDigestBytes, func() {
				swapErr = replaceExtensionPathWithFIFO(path)
			})
		result <- err
	}()
	if err := awaitExtensionRead(t, result); err == nil {
		t.Fatal("extension install accepted a FIFO replacement")
	}
	if swapErr != nil {
		t.Fatal(swapErr)
	}
}

func TestExtensionInstallSubdirectoryOpenDoesNotBlockOnFIFOReplacement(t *testing.T) {
	dir := t.TempDir()
	childPath := filepath.Join(dir, "components")
	if err := os.Mkdir(childPath, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := replaceExtensionPathWithFIFO(childPath); err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() {
		opened, err := openInstallSubdirectory(root, "components")
		if opened != nil {
			_ = opened.Close()
		}
		result <- err
	}()
	if err := awaitExtensionRead(t, result); err == nil {
		t.Fatal("extension install accepted a FIFO as a source subdirectory")
	}
}

func replaceExtensionPathWithFIFO(path string) error {
	if err := os.Remove(path); err != nil {
		return err
	}
	return unix.Mkfifo(path, 0o600)
}

func TestExtensionDiscoveryRejectsDirectAndSwappedFIFOsWithoutBlocking(t *testing.T) {
	t.Run("direct file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "plugin.json")
		if err := unix.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		result := make(chan error, 1)
		go func() {
			_, err := readBounded(root, "plugin.json", maxManifest)
			result <- err
		}()
		if err := awaitExtensionRead(t, result); err == nil {
			t.Fatal("direct FIFO was accepted as a plugin manifest")
		}
	})

	t.Run("swapped file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "plugin.json")
		if err := os.WriteFile(path, []byte(`{"name":"safe"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		var swapErr error
		result := make(chan error, 1)
		go func() {
			_, err := readBoundedWithHook(root, "plugin.json", maxManifest, func() {
				swapErr = replaceExtensionPathWithFIFO(path)
			})
			result <- err
		}()
		if err := awaitExtensionRead(t, result); err == nil {
			t.Fatal("FIFO replacement was accepted as a plugin manifest")
		}
		if swapErr != nil {
			t.Fatal(swapErr)
		}
	})

	t.Run("swapped digest directory", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "components")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		var swapErr error
		result := make(chan error, 1)
		go func() {
			_, err := readDigestDirectoryWithHook(root, "components", maxDigestEntries, func() {
				swapErr = replaceExtensionPathWithFIFO(path)
			})
			result <- err
		}()
		if err := awaitExtensionRead(t, result); err == nil {
			t.Fatal("FIFO replacement was accepted as a plugin directory")
		}
		if swapErr != nil {
			t.Fatal(swapErr)
		}
	})
}
