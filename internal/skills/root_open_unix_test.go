//go:build unix

package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type rootedReadOutcome struct {
	data []byte
	err  error
}

func awaitRootedRead(t *testing.T, result <-chan rootedReadOutcome) rootedReadOutcome {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("rooted skill read blocked on a FIFO")
		return rootedReadOutcome{}
	}
}

func TestSkillReadRejectsFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	result := make(chan rootedReadOutcome, 1)
	go func() {
		data, err := readFileFromRoot(root, "SKILL.md", maxDefinitionBytes)
		result <- rootedReadOutcome{data: data, err: err}
	}()
	got := awaitRootedRead(t, result)
	if got.err == nil || len(got.data) != 0 || !strings.Contains(got.err.Error(), "not a regular file") {
		t.Fatalf("FIFO skill read = %q, %v", got.data, got.err)
	}
}

func TestSkillReadSwapToFIFOIsNonblocking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	var swapErr error
	result := make(chan rootedReadOutcome, 1)
	go func() {
		data, err := readFileFromRootWithHook(root, "SKILL.md", maxDefinitionBytes, func() {
			if err := os.Remove(path); err != nil {
				swapErr = err
				return
			}
			swapErr = unix.Mkfifo(path, 0o600)
		})
		result <- rootedReadOutcome{data: data, err: err}
	}()
	got := awaitRootedRead(t, result)
	if swapErr != nil {
		t.Fatal(swapErr)
	}
	if got.err == nil || len(got.data) != 0 {
		t.Fatalf("swapped FIFO skill read = %q, %v; want a prompt refusal", got.data, got.err)
	}
}

func TestClaudeCommandDefinitionAndDirectoryFIFOsDoNotBlock(t *testing.T) {
	setTestHome(t, t.TempDir())
	workspace := t.TempDir()
	commandRoot := filepath.Join(workspace, ".claude", "commands")
	path := writeClaudeCommand(t, commandRoot, "review.md", "review")
	src := claudeCommandSources(workspace)[0]
	candidates, notes, err := discoverClaudeCommandCandidates(&src)
	if err != nil || len(notes) != 0 || len(candidates) != 1 {
		t.Fatalf("command discovery = %+v, notes %v, err %v", candidates, notes, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}

	result := make(chan rootedReadOutcome, 1)
	go func() {
		data, err := readClaudeCommandDefinition(src, candidates[0])
		result <- rootedReadOutcome{data: data, err: err}
	}()
	got := awaitRootedRead(t, result)
	if got.err == nil || len(got.data) != 0 {
		t.Fatalf("FIFO Claude definition read = %q, %v; want a prompt refusal", got.data, got.err)
	}

	directoryFIFO := filepath.Join(commandRoot, "queue")
	if err := unix.Mkfifo(directoryFIFO, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(commandRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	remaining := maxClaudeCommandEntries
	directoryResult := make(chan error, 1)
	go func() {
		_, err := readCommandDir(root, "queue", &remaining)
		directoryResult <- err
	}()
	select {
	case err := <-directoryResult:
		if err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("FIFO command directory error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Claude command directory read blocked on a FIFO")
	}
}
