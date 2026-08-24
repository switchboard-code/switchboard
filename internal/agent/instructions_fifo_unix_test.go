//go:build unix

package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	instructionFIFOHelperEnv  = "SWITCHBOARD_TEST_INSTRUCTION_FIFO_HELPER"
	instructionFIFOHelperRoot = "SWITCHBOARD_TEST_INSTRUCTION_FIFO_ROOT"
)

func TestInstructionFIFOReplacementHelperProcess(t *testing.T) {
	if os.Getenv(instructionFIFOHelperEnv) != "1" {
		return
	}
	root := os.Getenv(instructionFIFOHelperRoot)
	path := filepath.Join(root, "AGENTS.md")
	reader, err := openInstructionReader(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.close()
	reader.beforeOpen = func() {
		reader.beforeOpen = nil
		if err := os.Rename(path, filepath.Join(root, "held.md")); err != nil {
			t.Fatalf("hold inspected instruction file: %v", err)
		}
		if err := unix.Mkfifo(path, 0o600); err != nil {
			t.Fatalf("replace instruction file with FIFO: %v", err)
		}
	}

	data, err := reader.read(path)
	if err == nil || len(data) != 0 {
		t.Fatalf("FIFO replacement returned data=%q err=%v", data, err)
	}
	if !strings.Contains(err.Error(), "changed while it was opened") {
		t.Fatalf("FIFO replacement refusal = %v", err)
	}
}

func TestInstructionFIFOReplacementCannotBlockStartup(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), "inspected regular instructions")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInstructionFIFOReplacementHelperProcess$")
	cmd.Env = append(os.Environ(),
		instructionFIFOHelperEnv+"=1",
		instructionFIFOHelperRoot+"="+root,
	)
	output, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("instruction discovery blocked on a FIFO replacement: %s", output)
	}
	if err != nil {
		t.Fatalf("FIFO replacement helper: %v\n%s", err, output)
	}
}
