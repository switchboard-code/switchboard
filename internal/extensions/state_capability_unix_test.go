//go:build unix

package extensions

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/fileprivacy"
	"golang.org/x/sys/unix"
)

func TestPluginStateLockRegularToFIFOReplacementDoesNotBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", StateFileName)
	state, err := OpenStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := path + ".lock"
	lock, err := fileprivacy.Create(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	var swapErr error
	pluginStateLockBeforeOpenTestHook = func() {
		pluginStateLockBeforeOpenTestHook = nil
		swapErr = os.Remove(lockPath)
		if swapErr == nil {
			swapErr = unix.Mkfifo(lockPath, 0o600)
		}
	}
	t.Cleanup(func() { pluginStateLockBeforeOpenTestHook = nil })
	missingPath := filepath.Join(t.TempDir(), "missing")
	result := make(chan error, 1)
	go func() {
		result <- state.Disable(Plugin{
			ID: "claude:missing", Dialect: DialectClaude, Scope: ScopeUser,
			RealPath: missingPath,
		}, "")
	}()
	select {
	case err := <-result:
		if swapErr != nil {
			t.Fatal(swapErr)
		}
		if err == nil {
			t.Fatal("FIFO replacement was accepted as the plugin state lock")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("plugin state lock blocked on a FIFO replacement")
	}
}

func TestPluginStateLockRejectsPathPartitionAfterOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", StateFileName)
	state, err := OpenStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := path + ".lock"
	movedLock := lockPath + ".moved"
	var hookErr error
	pluginStateLockAfterOpenTestHook = func() {
		pluginStateLockAfterOpenTestHook = nil
		hookErr = os.Rename(lockPath, movedLock)
		if hookErr != nil {
			return
		}
		replacement, err := fileprivacy.Create(lockPath)
		if err != nil {
			hookErr = err
			return
		}
		hookErr = replacement.Close()
	}
	t.Cleanup(func() { pluginStateLockAfterOpenTestHook = nil })
	missing := Plugin{
		ID: "claude:missing", Dialect: DialectClaude, Scope: ScopeUser,
		RealPath: filepath.Join(t.TempDir(), "missing"),
	}
	if err := state.Disable(missing, ""); err == nil {
		t.Fatal("lock path partition was accepted")
	}
	if hookErr != nil {
		t.Fatal(hookErr)
	}
}

func TestPluginStateReadRejectsPermissionChangeAfterContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), StateFileName)
	writePrivateStateFile(t, path, []byte(`{"version":1,"activations":[]}`))
	var hookErr error
	pluginStateAfterReadTestHook = func() {
		pluginStateAfterReadTestHook = nil
		hookErr = os.Chmod(path, 0o644)
	}
	t.Cleanup(func() { pluginStateAfterReadTestHook = nil })
	if _, err := OpenStateFile(path); err == nil || !strings.Contains(err.Error(), "owner-only") {
		t.Fatalf("permission change after read = %v", err)
	}
	if hookErr != nil {
		t.Fatal(hookErr)
	}
}

func TestPluginStatePublicationNeverFollowsRetargetedParent(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "state")
	moved := filepath.Join(base, "state-moved")
	path := filepath.Join(parent, StateFileName)
	state, err := OpenStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plugin := testPlugin(t, filepath.Join(t.TempDir(), "plugin"), "parent-race", false)
	candidate := testActivationCandidate(t, plugin)
	outside := []byte(`{"version":1,"activations":[]}`)
	var hookErr error
	pluginStateBeforePublicationTestHook = func() {
		pluginStateBeforePublicationTestHook = nil
		hookErr = os.Rename(parent, moved)
		if hookErr != nil {
			return
		}
		hookErr = fileprivacy.EnsurePrivateDir(parent)
		if hookErr != nil {
			return
		}
		file, err := fileprivacy.Create(path)
		if err != nil {
			hookErr = err
			return
		}
		_, writeErr := file.Write(outside)
		hookErr = errors.Join(writeErr, file.Close())
	}
	t.Cleanup(func() { pluginStateBeforePublicationTestHook = nil })
	if err := state.Enable(candidate, ""); err == nil {
		t.Fatal("publication succeeded after its retained parent was retargeted")
	}
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != string(outside) {
		t.Fatalf("replacement parent state = %q, %v", raw, err)
	}
	if _, err := os.Lstat(filepath.Join(moved, StateFileName)); !os.IsNotExist(err) {
		t.Fatalf("rolled-back state remains in retained parent: %v", err)
	}
	if got := state.Status(plugin, ""); got != (ActivationStatus{}) {
		t.Fatalf("failed publication remained active in memory: %+v", got)
	}
}

func TestPluginStateAdoptsPublishedStateWhenCleanupFails(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(parent, StateFileName)
	state, err := OpenStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plugin := testPlugin(t, filepath.Join(t.TempDir(), "plugin"), "published-cleanup-error", false)
	candidate := testActivationCandidate(t, plugin)
	journal := filepath.Join(parent, pluginStateRecoveryDirectory)
	movedJournal := filepath.Join(parent, ".plugin-state-recovery-moved")
	var hookErr error
	pluginStateBeforePublicationTestHook = func() {
		pluginStateBeforePublicationTestHook = nil
		hookErr = os.Rename(journal, movedJournal)
	}
	t.Cleanup(func() { pluginStateBeforePublicationTestHook = nil })
	if err := state.Enable(candidate, ""); err == nil {
		t.Fatal("publication with unavailable cleanup ledger returned no error")
	}
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if got := state.Status(plugin, ""); !got.Enabled {
		t.Fatalf("published activation was rolled back in memory: %+v", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), `"id": "claude:review-tools"`) {
		t.Fatalf("published activation bytes = %q, %v", raw, err)
	}

	if err := os.Rename(movedJournal, journal); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.RecoverFilePublicationCleanup(journal, parent); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(journal)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".switchboard-undo-") {
			t.Fatalf("cleanup recovery left publication evidence: %s", entry.Name())
		}
	}
}
