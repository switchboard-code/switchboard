//go:build unix

package main

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

const nativeMCPStateCrashHelper = "SB_NATIVE_MCP_STATE_CRASH_HELPER"

func TestNativeMCPStateFIFOFilesRefuseWithoutBlocking(t *testing.T) {
	for _, test := range []struct {
		name string
		leaf string
	}{
		{name: "lock", leaf: nativeMCPStateFileName + ".lock"},
		{name: "state", leaf: nativeMCPStateFileName},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, nativeMCPStateFileName)
			if err := unix.Mkfifo(filepath.Join(directory, test.leaf), 0o600); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() {
				_, err := openNativeMCPActivationStateFile(path)
				result <- err
			}()
			select {
			case err := <-result:
				if err == nil || !strings.Contains(err.Error(), "regular") {
					t.Fatalf("FIFO %s error = %v", test.name, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("opening FIFO %s blocked", test.name)
			}
		})
	}
}

func TestNativeMCPStateParentRetargetCannotRedirectPublication(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	workspace := filepath.Join(base, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	writeNativeMCPConfig(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {
    "x": {"command":"x"},
    "y": {"command":"y"}
  }
}`)
	x := nativeMCPRequest(t, home, workspace, "claude:x")
	y := nativeMCPRequest(t, home, workspace, "claude:y")
	directory := filepath.Join(base, "state")
	path := filepath.Join(directory, nativeMCPStateFileName)
	state, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.enable(x); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(base, "retained-state")
	foreign := filepath.Join(base, "foreign-state")
	const sentinel = "foreign replacement must survive\n"
	var hookErr error
	nativeMCPStateBeforePublication = func() {
		if err := os.Rename(directory, moved); err != nil {
			hookErr = err
			return
		}
		if err := os.Mkdir(directory, 0o700); err != nil {
			hookErr = err
			return
		}
		if err := os.WriteFile(path, []byte(sentinel), 0o600); err != nil {
			hookErr = err
		}
	}
	t.Cleanup(func() { nativeMCPStateBeforePublication = nil })
	mutationErr := state.enable(y)
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if mutationErr == nil || !strings.Contains(mutationErr.Error(), "no longer") &&
		!strings.Contains(mutationErr.Error(), "changed identity") {
		t.Fatalf("retargeted parent mutation error = %v", mutationErr)
	}
	if state.NativeMCPActivated(x) || state.NativeMCPActivated(y) {
		t.Fatal("retargeted publication left cached authority usable")
	}
	if err := os.Rename(directory, foreign); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, directory); err != nil {
		t.Fatal(err)
	}
	nativeMCPStateBeforePublication = nil
	foreignRaw, err := os.ReadFile(filepath.Join(foreign, nativeMCPStateFileName))
	if err != nil || string(foreignRaw) != sentinel {
		t.Fatalf("foreign replacement = %q, %v", foreignRaw, err)
	}
	reopened, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.NativeMCPActivated(x) || reopened.NativeMCPActivated(y) {
		t.Fatal("retargeted parent changed the retained activation state")
	}
	assertNoNativeMCPStatePublicationArtifacts(t, directory)
}

func TestNativeMCPStateCrashRecoveryRetiresPreparedPublication(t *testing.T) {
	base := t.TempDir()
	ready := filepath.Join(base, "ready")
	command := exec.Command(os.Args[0], "-test.run=^TestNativeMCPStateCrashHelper$")
	command.Env = append(os.Environ(),
		nativeMCPStateCrashHelper+"=1",
		"SB_NATIVE_MCP_STATE_ROOT="+base,
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatal("native MCP crash helper did not reach the prepared publication seam")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("native MCP crash helper exited successfully")
	}

	home := filepath.Join(base, "home")
	workspace := filepath.Join(base, "workspace")
	path := filepath.Join(base, "state", nativeMCPStateFileName)
	x := nativeMCPRequest(t, home, workspace, "claude:x")
	y := nativeMCPRequest(t, home, workspace, "claude:y")
	reopened, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.NativeMCPActivated(x) || reopened.NativeMCPActivated(y) {
		t.Fatal("crash recovery changed the last durable activation image")
	}
	assertNoNativeMCPStatePublicationArtifacts(t, filepath.Dir(path))
}

func TestNativeMCPStateCrashHelper(t *testing.T) {
	if os.Getenv(nativeMCPStateCrashHelper) != "1" {
		return
	}
	base := os.Getenv("SB_NATIVE_MCP_STATE_ROOT")
	home := filepath.Join(base, "home")
	workspace := filepath.Join(base, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	writeNativeMCPConfig(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {
    "x": {"command":"x"},
    "y": {"command":"y"}
  }
}`)
	x := nativeMCPRequest(t, home, workspace, "claude:x")
	y := nativeMCPRequest(t, home, workspace, "claude:y")
	state, err := openNativeMCPActivationStateFile(filepath.Join(base, "state", nativeMCPStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.enable(x); err != nil {
		t.Fatal(err)
	}
	nativeMCPStateBeforePublication = func() {
		markNativeMCPStateCrashBoundary(t, filepath.Join(base, "ready"))
		select {}
	}
	if err := state.enableWithRequiredContext(context.Background(), y, false); err != nil {
		t.Fatal(err)
	}
	t.Fatal("native MCP crash helper completed without reaching the publication seam")
}

func markNativeMCPStateCrashBoundary(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_SYNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertNoNativeMCPStatePublicationArtifacts(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == nativeMCPStateRecoveryDirName && entry.IsDir() {
			continue
		}
		if entry.Name() != nativeMCPStateFileName && entry.Name() != nativeMCPStateFileName+".lock" {
			t.Fatalf("native MCP state publication artifact remains: %q", entry.Name())
		}
	}
	recoveryEntries, err := os.ReadDir(filepath.Join(directory, nativeMCPStateRecoveryDirName))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range recoveryEntries {
		if strings.HasPrefix(entry.Name(), ".switchboard-quarantine-") {
			info, err := entry.Info()
			if err != nil {
				t.Fatal(err)
			}
			if !info.Mode().IsRegular() || info.Size() != 0 || info.Mode().Perm() != 0o600 {
				t.Fatalf("native MCP state quarantine retained bytes or weak permissions: %q (%v)", entry.Name(), info)
			}
			continue
		}
		t.Fatalf("native MCP state recovery artifact remains: %q", entry.Name())
	}
}
