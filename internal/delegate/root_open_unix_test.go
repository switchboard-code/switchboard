//go:build unix

package delegate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type anchoredReadOutcome struct {
	data []byte
	err  error
}

func awaitAnchoredRead(t *testing.T, result <-chan anchoredReadOutcome) anchoredReadOutcome {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("anchored definition read blocked on a FIFO")
		return anchoredReadOutcome{}
	}
}

func TestAnchoredDefinitionRejectsFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "definition.md")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	result := make(chan anchoredReadOutcome, 1)
	go func() {
		data, _, err := readAnchoredDefinition(root, "definition.md", path, maxAgentDefinitionBytes)
		result <- anchoredReadOutcome{data: data, err: err}
	}()
	got := awaitAnchoredRead(t, result)
	if got.err == nil || len(got.data) != 0 || !strings.Contains(got.err.Error(), "not a regular file") {
		t.Fatalf("FIFO definition read = %q, %v", got.data, got.err)
	}
}

func TestAnchoredDefinitionSwapToFIFOIsNonblocking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "definition.md")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	var swapErr error
	result := make(chan anchoredReadOutcome, 1)
	go func() {
		data, _, err := readAnchoredDefinitionWithHook(root, "definition.md", path, maxWorkflowDefinitionBytes, func() {
			if err := os.Remove(path); err != nil {
				swapErr = err
				return
			}
			swapErr = unix.Mkfifo(path, 0o600)
		})
		result <- anchoredReadOutcome{data: data, err: err}
	}()
	got := awaitAnchoredRead(t, result)
	if swapErr != nil {
		t.Fatal(swapErr)
	}
	if got.err == nil || len(got.data) != 0 {
		t.Fatalf("swapped FIFO definition read = %q, %v; want a prompt refusal", got.data, got.err)
	}
}
