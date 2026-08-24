//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func awaitCLISourceRead(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("CLI workspace read blocked on a FIFO")
		return nil
	}
}

func replaceCLISourceWithFIFO(path string) error {
	if err := os.Remove(path); err != nil {
		return err
	}
	return unix.Mkfifo(path, 0o600)
}

func TestCLIWorkspaceReadersRejectFIFOAndOversizeWithoutBlocking(t *testing.T) {
	t.Run("direct FIFO", func(t *testing.T) {
		workspace := t.TempDir()
		path := filepath.Join(workspace, "source")
		if err := unix.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			_, err := readWorkspaceFileBounded(workspace, path, maxBlameFileBytes, nil)
			result <- err
		}()
		if err := awaitCLISourceRead(t, result); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("direct FIFO error = %v", err)
		}
	})

	t.Run("swap to FIFO", func(t *testing.T) {
		workspace := t.TempDir()
		path := filepath.Join(workspace, "source")
		if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		var swapErr error
		result := make(chan error, 1)
		go func() {
			_, err := readWorkspaceFileBounded(workspace, path, maxBlameFileBytes, func() {
				swapErr = replaceCLISourceWithFIFO(path)
			})
			result <- err
		}()
		if err := awaitCLISourceRead(t, result); err == nil {
			t.Fatal("workspace reader accepted FIFO replacement")
		}
		if swapErr != nil {
			t.Fatal(swapErr)
		}
	})

	t.Run("oversize", func(t *testing.T) {
		workspace := t.TempDir()
		path := filepath.Join(workspace, "source")
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(maxBlameFileBytes + 1); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := readBlameFile(workspace, path); err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("oversize blame read error = %v", err)
		}
	})
}

func TestRuleAndWatchReadersDoNotBlockOnFIFOReplacement(t *testing.T) {
	t.Run("rule file", func(t *testing.T) {
		workspace := t.TempDir()
		path := filepath.Join(workspace, "rule.md")
		if err := os.WriteFile(path, []byte("---\npaths: src/**\n---\nbody"), 0o600); err != nil {
			t.Fatal(err)
		}
		var swapErr error
		result := make(chan error, 1)
		go func() {
			_, err := readRuleWithHook(workspace, path, func() { swapErr = replaceCLISourceWithFIFO(path) })
			result <- err
		}()
		if err := awaitCLISourceRead(t, result); err == nil {
			t.Fatal("rule reader accepted FIFO replacement")
		}
		if swapErr != nil {
			t.Fatal(swapErr)
		}
	})

	t.Run("bare watch inference", func(t *testing.T) {
		workspace := t.TempDir()
		path := filepath.Join(workspace, "Makefile")
		if err := os.WriteFile(path, []byte("test:\n\tgo test ./..."), 0o600); err != nil {
			t.Fatal(err)
		}
		var swapErr error
		result := make(chan error, 1)
		go func() {
			got := suggestVerifierWithHook(workspace, func(name string) {
				if name == "Makefile" {
					swapErr = replaceCLISourceWithFIFO(path)
				}
			})
			if got != "" {
				result <- os.ErrInvalid
				return
			}
			result <- nil
		}()
		if err := awaitCLISourceRead(t, result); err != nil {
			t.Fatal(err)
		}
		if swapErr != nil {
			t.Fatal(swapErr)
		}
	})

	t.Run("rules directory", func(t *testing.T) {
		workspace := t.TempDir()
		path := filepath.Join(workspace, ".switchboard", "rules")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			set, notes := loadRules(workspace)
			if len(set.rules) != 0 || len(notes) == 0 {
				result <- os.ErrInvalid
				return
			}
			result <- nil
		}()
		if err := awaitCLISourceRead(t, result); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCLIWorkspaceDirectorySwapToFIFOIsNonblocking(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "rules")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	var swapErr error
	result := make(chan error, 1)
	go func() {
		_, err := readWorkspaceDirectoryBounded(workspace, path, maxRuleDirectoryEntries, func() {
			swapErr = replaceCLISourceWithFIFO(path)
		})
		result <- err
	}()
	if err := awaitCLISourceRead(t, result); err == nil {
		t.Fatal("directory reader accepted FIFO replacement")
	}
	if swapErr != nil {
		t.Fatal(swapErr)
	}
}

func TestCLIWorkspaceReadersRejectParentSwapOutsideRoot(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		workspace := t.TempDir()
		external := t.TempDir()
		insideParent := filepath.Join(workspace, "nested")
		if err := os.Mkdir(insideParent, 0o700); err != nil {
			t.Fatal(err)
		}
		inside := filepath.Join(insideParent, "source")
		outside := filepath.Join(external, "source")
		if err := os.WriteFile(inside, []byte("inside"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(outside, []byte("outside-secret"), 0o600); err != nil {
			t.Fatal(err)
		}

		var swapErr error
		data, err := readWorkspaceFileBounded(workspace, inside, maxBlameFileBytes, func() {
			swapErr = os.Rename(insideParent, insideParent+"-old")
			if swapErr == nil {
				swapErr = os.Symlink(external, insideParent)
			}
		})
		if swapErr != nil {
			t.Fatal(swapErr)
		}
		if err == nil || strings.Contains(string(data), "outside-secret") {
			t.Fatalf("parent swap escaped workspace: data=%q err=%v", data, err)
		}
	})

	t.Run("directory", func(t *testing.T) {
		workspace := t.TempDir()
		external := t.TempDir()
		insideParent := filepath.Join(workspace, ".switchboard")
		inside := filepath.Join(insideParent, "rules")
		outside := filepath.Join(external, "rules")
		if err := os.MkdirAll(inside, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(outside, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outside, "outside.md"), []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}

		var swapErr error
		entries, err := readWorkspaceDirectoryBounded(workspace, inside, maxRuleDirectoryEntries, func() {
			swapErr = os.Rename(insideParent, insideParent+"-old")
			if swapErr == nil {
				swapErr = os.Symlink(external, insideParent)
			}
		})
		if swapErr != nil {
			t.Fatal(swapErr)
		}
		if err == nil || len(entries) != 0 {
			t.Fatalf("parent swap enumerated outside workspace: entries=%v err=%v", entries, err)
		}
	})
}

func TestCLIWorkspaceReadersDiscardResultsWhenWorkspaceRootMovesAfterOpen(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(workspace, "source")
	if err := os.WriteFile(file, []byte("original-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	var moveErr error
	data, err := readWorkspaceFileBounded(workspace, file, maxBlameFileBytes, func() {
		moveErr = os.Rename(workspace, workspace+"-moved")
		if moveErr == nil {
			moveErr = os.Mkdir(workspace, 0o700)
		}
		if moveErr == nil {
			moveErr = os.WriteFile(filepath.Join(workspace, "source"), []byte("replacement-secret"), 0o600)
		}
	})
	if moveErr != nil {
		t.Fatal(moveErr)
	}
	if err == nil || len(data) != 0 {
		t.Fatalf("moved workspace returned bytes: data=%q err=%v", data, err)
	}
}
