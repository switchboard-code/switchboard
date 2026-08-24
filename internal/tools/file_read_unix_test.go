//go:build unix

package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/execution"
	"golang.org/x/sys/unix"
)

func openTestWorkspacePath(path string) (*Registry, *os.Root, string, error) {
	registry, err := NewRegistry(filepath.Dir(path), execution.Capability{})
	if err != nil {
		return nil, nil, "", err
	}
	abs, err := registry.resolve(path)
	if err != nil {
		return nil, nil, "", err
	}
	root, relative, err := registry.openResolvedWorkspace(abs)
	return registry, root, relative, err
}

func awaitWorkspaceRead(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("workspace read blocked on a FIFO")
		return nil
	}
}

func replaceWorkspacePathWithFIFO(path string) error {
	if err := os.Remove(path); err != nil {
		return err
	}
	return unix.Mkfifo(path, 0o600)
}

func TestWorkspaceReadersRejectDirectFIFOAndOversize(t *testing.T) {
	t.Run("direct FIFO", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "source")
		if err := unix.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			_, root, relative, err := openTestWorkspacePath(path)
			if err == nil {
				defer root.Close()
				_, _, err = readRegularWorkspaceFile(root, relative, path, maxWorkspaceFileBytes, nil)
			}
			result <- err
		}()
		if err := awaitWorkspaceRead(t, result); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("direct FIFO error = %v", err)
		}
	})

	t.Run("oversize", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "large")
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(maxWorkspaceFileBytes + 1); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		_, root, relative, err := openTestWorkspacePath(path)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		if _, _, err := readRegularWorkspaceFile(root, relative, path, maxWorkspaceFileBytes, nil); err == nil || !strings.Contains(err.Error(), "file limit") {
			t.Fatalf("oversize read error = %v", err)
		}
		parent, _, err := bindWorkspaceParent(root, filepath.Dir(relative), false)
		if err != nil {
			t.Fatal(err)
		}
		defer parent.Close()
		if _, err := readDiskFile(parent, filepath.Base(relative), path, nil); err == nil || !strings.Contains(err.Error(), "mutation file limit") {
			t.Fatalf("oversize preimage error = %v", err)
		}
	})
}

func TestFirstPartyWorkspaceReadersDoNotBlockOnSwapToFIFO(t *testing.T) {
	tests := map[string]func(path string, beforeOpen func()) error{
		"read": func(path string, beforeOpen func()) error {
			registry, err := NewRegistry(filepath.Dir(path), execution.Capability{})
			if err != nil {
				return err
			}
			abs, err := registry.resolve(path)
			if err != nil {
				return err
			}
			result, err := (&readTool{r: registry}).readWithHook(abs, readInput{}, beforeOpen)
			if err != nil {
				return err
			}
			if !result.IsError {
				return fmt.Errorf("read accepted FIFO replacement: %+v", result)
			}
			return nil
		},
		"write/edit preimage": func(path string, beforeOpen func()) error {
			_, root, relative, err := openTestWorkspacePath(path)
			if err != nil {
				return err
			}
			defer root.Close()
			parent, _, err := bindWorkspaceParent(root, filepath.Dir(relative), false)
			if err != nil {
				return err
			}
			defer parent.Close()
			_, err = readDiskFile(parent, filepath.Base(relative), path, beforeOpen)
			if err == nil {
				return fmt.Errorf("mutation preimage accepted FIFO replacement")
			}
			return nil
		},
		"grep": func(path string, beforeOpen func()) error {
			registry, root, relative, err := openTestWorkspacePath(path)
			if err != nil {
				return err
			}
			defer root.Close()
			got := (&grepTool{r: registry}).scanFileWithHook(root, relative, regexp.MustCompile("original"), 10, beforeOpen)
			if got.count != 0 || len(got.lines) != 0 {
				return fmt.Errorf("grep accepted FIFO replacement: %+v", got)
			}
			return nil
		},
		"round-boundary drift": func(path string, beforeOpen func()) error {
			registry, err := NewRegistry(filepath.Dir(path), execution.Capability{})
			if err != nil {
				return err
			}
			abs, err := registry.resolve(path)
			if err != nil {
				return err
			}
			registry.versions.seen[abs] = hashContent([]byte("before"))
			registry.versions.stamps[abs] = readStamp{}
			if _, changed := registry.driftOfWithHook(abs, beforeOpen); changed {
				return fmt.Errorf("drift accepted FIFO replacement")
			}
			return nil
		},
	}

	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "source")
			if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			var swapErr error
			result := make(chan error, 1)
			go func() {
				result <- run(path, func() { swapErr = replaceWorkspacePathWithFIFO(path) })
			}()
			if err := awaitWorkspaceRead(t, result); err != nil {
				t.Fatal(err)
			}
			if swapErr != nil {
				t.Fatal(swapErr)
			}
		})
	}
}
