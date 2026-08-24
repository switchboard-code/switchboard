//go:build unix

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestShellHighVolumeOutputStaysBounded(t *testing.T) {
	m, msg := runShellForTest(t, `yes switchboard | head -c 5000000`)
	if msg.err != nil || msg.result.kind != shellSucceeded {
		t.Fatalf("high-volume command failed: err=%v result=%+v", msg.err, msg.result)
	}
	marker := "[truncated at " + strconv.Itoa(shellOutputCap) + "-byte limit]"
	if len(msg.output) > shellOutputCap+len(marker)+2 || !strings.Contains(msg.output, marker) {
		t.Fatalf("display output was not bounded: bytes=%d suffix=%q", len(msg.output), msg.output[max(0, len(msg.output)-80):])
	}
	if len(msg.contextOutput) > shellOutputCap+len(marker)+2 || !strings.Contains(msg.contextOutput, marker) {
		t.Fatalf("context output was not bounded: bytes=%d", len(msg.contextOutput))
	}
	m.onShellDone(msg)
	if len(m.pendingShell) != 1 || len(m.pendingShell[0]) > shellOutputCap+512 {
		t.Fatalf("high-volume output escaped the pending-context bound: entries=%d bytes=%d", len(m.pendingShell), len(m.pendingShell[0]))
	}
}

func TestShellCancellationKillsDescendantHoldingOutputAndReleasesPrompt(t *testing.T) {
	m := testModel(t)
	m.app.workspace = t.TempDir()
	m.workspaceRuntime = newWorkspaceRuntime(m.app.workspace)
	t.Setenv("SHELL", "/bin/sh")

	// sleep inherits both output descriptors. Killing only /bin/sh leaves sleep
	// alive and os/exec.Wait blocked on those pipes even though the shell itself
	// is gone, which in turn leaves the TUI operation owning the prompt.
	cmd := m.runShell(`sleep 30 & printf '%s\n' "$!" > child.pid; wait`)
	if cmd == nil {
		t.Fatal("shell command did not launch")
	}
	done := make(chan shellDoneMsg, 1)
	go func() { done <- cmd().(shellDoneMsg) }()

	pidPath := filepath.Join(m.app.workspace, "child.pid")
	pid := waitForShellChildPID(t, pidPath, 2*time.Second)
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		if m.turnCancel != nil {
			m.turnCancel()
		}
	})

	started := time.Now()
	m.interrupt()
	if !m.operationCancelling {
		t.Fatal("ctrl-c did not mark the shell operation as cancelling")
	}

	var msg shellDoneMsg
	select {
	case msg = <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("shell cancellation did not return the prompt while a descendant retained its output pipes")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("shell cancellation took %s; cleanup is not bounded tightly enough for the prompt", elapsed)
	}
	if msg.result.kind != shellCancelled {
		t.Fatalf("shell result = %+v, want cancellation", msg.result)
	}
	m.onShellDone(msg)
	if m.busy || m.operationActive || m.turnCancel != nil {
		t.Fatalf("cancelled shell retained prompt ownership: busy=%v operation=%v cancel=%v",
			m.busy, m.operationActive, m.turnCancel != nil)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !shellTestProcessRunning(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("background child %d survived shell cancellation", pid)
}

func waitForShellChildPID(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(body)))
			if parseErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("background child did not publish its pid to %s", path)
	return 0
}

func shellTestProcessRunning(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	if runtime.GOOS != "linux" {
		return true
	}

	// A correctly killed descendant may remain as a zombie when a container has
	// no init process. Signal 0 sees that pid; /proc distinguishes it from work
	// that survived cancellation.
	body, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	end := strings.LastIndexByte(string(body), ')')
	if end < 0 {
		return true
	}
	rest := strings.TrimLeft(string(body[end+1:]), " ")
	return rest == "" || rest[0] != 'Z'
}
