//go:build unix

package mcp

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLoadSpecsRootedRejectsFIFOAndParentEscape(t *testing.T) {
	t.Run("FIFO", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, ".switchboard"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mkfifo(filepath.Join(root, ".switchboard", SpecFileName), 0o600); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			_, err := LoadSpecsRooted(root, filepath.Join(".switchboard", SpecFileName))
			result <- err
		}()
		select {
		case err := <-result:
			if err == nil {
				t.Fatal("FIFO declaration was accepted")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("MCP declaration read blocked on FIFO")
		}
	})

	t.Run("parent swap", func(t *testing.T) {
		root := t.TempDir()
		external := t.TempDir()
		parent := filepath.Join(root, ".switchboard")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(parent, SpecFileName), []byte("[mcp.inside]\ncommand='inside'\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(external, SpecFileName), []byte("[mcp.outside]\ncommand='outside'\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var swapErr error
		specs, err := loadSpecsRootedWithHook(root, filepath.Join(".switchboard", SpecFileName), func() {
			swapErr = os.Rename(parent, parent+"-old")
			if swapErr == nil {
				swapErr = os.Symlink(external, parent)
			}
		})
		if swapErr != nil {
			t.Fatal(swapErr)
		}
		if err == nil || len(specs) != 0 {
			t.Fatalf("parent swap loaded outside declaration: specs=%+v err=%v", specs, err)
		}
	})
}
