package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/switchboard-code/switchboard/internal/fileprivacy"
	"github.com/switchboard-code/switchboard/internal/mcpnative"
)

func TestNativeMCPActivationBindsWholeDefinitionAndPersists(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	configPath := filepath.Join(home, ".codex", "config.toml")
	writeNativeMCPConfig(t, configPath, `[mcp_servers.docs]
command = "docs-server"
args = ["--mode", "safe"]
`)
	request := nativeMCPRequest(t, home, workspace, "codex:docs")
	path := filepath.Join(t.TempDir(), nativeMCPStateFileName)
	state, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if state.NativeMCPActivated(request) {
		t.Fatal("native declaration activated itself")
	}
	if err := state.enable(request); err != nil {
		t.Fatal(err)
	}
	if status := state.status(request); !status.Enabled || status.Changed {
		t.Fatalf("activation status = %#v", status)
	}
	reopened, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.NativeMCPActivated(request) {
		t.Fatal("activation did not survive restart")
	}

	writeNativeMCPConfig(t, configPath, `[mcp_servers.docs]
command = "different-server"
args = ["--mode", "safe"]
`)
	changed := nativeMCPRequest(t, home, workspace, "codex:docs")
	if status := reopened.status(changed); status.Enabled || !status.Changed {
		t.Fatalf("changed definition retained authority: %#v", status)
	}
	if err := reopened.enable(changed); err != nil {
		t.Fatal(err)
	}
	if !reopened.NativeMCPActivated(changed) || reopened.NativeMCPActivated(request) {
		t.Fatal("reapproval did not replace the exact definition identity")
	}
	if err := reopened.disable(changed); err != nil {
		t.Fatal(err)
	}
	if reopened.NativeMCPActivated(changed) {
		t.Fatal("disabled definition remained active")
	}
}

func TestNativeMCPActivationStateFailsClosedOnUnsafeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), nativeMCPStateFileName)
	writePrivateNativeMCPState(t, path, `{"version":1,"version":1,"key":"x","activations":[]}`)
	if _, err := openNativeMCPActivationStateFile(path); err == nil || !strings.Contains(err.Error(), "duplicate JSON key") {
		t.Fatalf("duplicate state error = %v", err)
	}

	if runtime.GOOS != "windows" {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		writePrivateNativeMCPState(t, path, `{"version":1,"key":"x","activations":[]}`)
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := openNativeMCPActivationStateFile(path); err == nil || !strings.Contains(err.Error(), "owner-only") {
			t.Fatalf("loose-permission error = %v", err)
		}

	}

	target := filepath.Join(t.TempDir(), "target.json")
	writePrivateNativeMCPState(t, target, `{"version":1,"key":"x","activations":[]}`)
	link := filepath.Join(t.TempDir(), nativeMCPStateFileName)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := openNativeMCPActivationStateFile(link); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlink state error = %v", err)
	}
}

func TestNativeMCPActivationMutationReloadFailsClosedOnUnsafeFile(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	writeNativeMCPConfig(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {"docs": {"command":"docs"}}
}`)
	request := nativeMCPRequest(t, home, workspace, "claude:docs")
	directory := t.TempDir()
	path := filepath.Join(directory, nativeMCPStateFileName)
	state, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(directory, "target.json")
	target, err := openNativeMCPActivationStateFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.enable(request); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetPath, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err = state.enable(request)
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("mutation through unsafe state error = %v", err)
	}
	if state.NativeMCPActivated(request) || len(state.references(workspace)) != 0 {
		t.Fatal("unsafe reload left cached activation authority usable")
	}
	reopened, err := openNativeMCPActivationStateFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.NativeMCPActivated(request) {
		t.Fatal("unsafe mutation changed the symlink target")
	}
}

func TestNativeMCPGlobalActivationAppliesWhenWorkspaceCannotResolve(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	writeNativeMCPConfig(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {"global": {"command":"global"}}
}`)
	request := nativeMCPRequest(t, home, workspace, "claude:global")
	state, err := openNativeMCPActivationStateFile(filepath.Join(t.TempDir(), nativeMCPStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.enableWithRequired(request, true); err != nil {
		t.Fatal(err)
	}
	missingWorkspace := filepath.Join(t.TempDir(), "missing")
	if !state.hasDialect(mcpnative.DialectClaude, missingWorkspace) {
		t.Fatal("unresolvable workspace hid a global activation")
	}
	if !state.snapshotFailureRequired(mcpnative.DialectClaude, missingWorkspace) {
		t.Fatal("unresolvable workspace hid global required semantics")
	}
	if references := state.references(missingWorkspace); len(references) != 1 || references[0].TrustRoot != "" {
		t.Fatalf("global references = %#v", references)
	}
}

func TestNativeMCPActivationMutationReloadsLatestAcrossOpenHandles(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	writeNativeMCPConfig(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {
    "x": {"command":"x"},
    "y": {"command":"y"}
  }
}`)
	x := nativeMCPRequest(t, home, workspace, "claude:x")
	y := nativeMCPRequest(t, home, workspace, "claude:y")
	path := filepath.Join(t.TempDir(), nativeMCPStateFileName)
	first, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.enable(x); err != nil {
		t.Fatal(err)
	}
	if err := first.disable(x); err != nil {
		t.Fatal(err)
	}
	if err := stale.enable(y); err != nil {
		t.Fatal(err)
	}

	reopened, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.NativeMCPActivated(x) {
		t.Fatal("stale handle resurrected the activation another handle disabled")
	}
	if !reopened.NativeMCPActivated(y) {
		t.Fatal("stale handle's requested mutation was not persisted")
	}
	if references := reopened.references(workspace); len(references) != 1 || references[0].ID != "claude:y" {
		t.Fatalf("persisted references = %#v", references)
	}
}

func TestNativeMCPActivationConcurrentHandlesDoNotLoseUpdates(t *testing.T) {
	for _, writers := range []int{32, 64} {
		t.Run(fmt.Sprintf("%d writers", writers), func(t *testing.T) {
			testNativeMCPActivationConcurrentHandles(t, writers)
		})
	}
}

func testNativeMCPActivationConcurrentHandles(t *testing.T, count int) {
	t.Helper()
	home := t.TempDir()
	workspace := t.TempDir()
	servers := make(map[string]any)
	for index := range count {
		name := fmt.Sprintf("server_%02d", index)
		servers[name] = map[string]any{"command": name}
	}
	raw, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		t.Fatal(err)
	}
	writeNativeMCPConfig(t, filepath.Join(home, ".claude.json"), string(raw))
	discovered := mcpnative.Discover(nativeMCPTestOptions(t, home, workspace))
	requests := make([]mcpnative.ActivationRequest, count)
	for index := range count {
		requests[index], err = discovered.ActivationRequest(fmt.Sprintf("claude:server_%02d", index))
		if err != nil {
			t.Fatal(err)
		}
	}

	path := filepath.Join(t.TempDir(), nativeMCPStateFileName)
	handles := make([]*nativeMCPActivationState, count)
	for index := range handles {
		handles[index], err = openNativeMCPActivationStateFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	runConcurrentNativeMCPMutations(t, count, func(index int) error {
		return handles[index].enableWithRequired(requests[index], index%3 == 0)
	})

	disableHandles := make([]*nativeMCPActivationState, count/2)
	for index := range disableHandles {
		disableHandles[index], err = openNativeMCPActivationStateFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	runConcurrentNativeMCPMutations(t, len(disableHandles), func(index int) error {
		return disableHandles[index].disable(requests[index*2])
	})

	reopened, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for index, request := range requests {
		if got, want := reopened.NativeMCPActivated(request), index%2 == 1; got != want {
			t.Errorf("activation %d enabled = %t, want %t", index, got, want)
		}
	}
	if references := reopened.references(workspace); len(references) != count/2 {
		t.Fatalf("remaining references = %d, want %d", len(references), count/2)
	}
}

func TestNativeMCPStateLockContextPreservesCallerDeadline(t *testing.T) {
	deadline := time.Now().Add(time.Hour).Round(0)
	parent, parentCancel := context.WithDeadline(context.Background(), deadline)
	defer parentCancel()
	waitCtx, cancel := nativeMCPStateLockContext(parent)
	defer cancel()
	got, ok := waitCtx.Deadline()
	if !ok || !got.Equal(deadline) {
		t.Fatalf("wait deadline = %v, %t; want exact caller deadline %v", got, ok, deadline)
	}

	beforeFallback := time.Now()
	fallback, fallbackCancel := nativeMCPStateLockContext(context.Background())
	afterFallback := time.Now()
	defer fallbackCancel()
	got, ok = fallback.Deadline()
	if !ok {
		t.Fatal("deadline-free caller received no finite lock fallback")
	}
	minDeadline := beforeFallback.Add(nativeMCPStateLockWait)
	maxDeadline := afterFallback.Add(nativeMCPStateLockWait)
	if got.Before(minDeadline) || got.After(maxDeadline) {
		t.Fatalf("fallback deadline = %v, want within [%v, %v]", got, minDeadline, maxDeadline)
	}
}

func TestNativeMCPStateLockHonorsCallerDeadlineWhileHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), nativeMCPStateFileName)
	held, err := acquireNativeMCPStateFileLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	const wait = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	started := time.Now()
	second, err := acquireNativeMCPStateFileLock(ctx, path)
	if second != nil {
		_ = second.Close()
		t.Fatal("contending lock acquired while the first lock remained held")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contending lock error = %v, want caller deadline", err)
	}
	elapsed := time.Since(started)
	if elapsed < wait/2 || elapsed > 2*time.Second {
		t.Fatalf("caller deadline returned after %v, want approximately %v", elapsed, wait)
	}
}

func TestNativeMCPStateLockCannotAcquireAfterCancellationAtPollBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), nativeMCPStateFileName)
	held, err := acquireNativeMCPStateFileLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var releaseErr error
	nativeMCPStateLockAfterPoll = func() {
		cancel()
		releaseErr = held.Close()
		nativeMCPStateLockAfterPoll = nil
	}
	t.Cleanup(func() { nativeMCPStateLockAfterPoll = nil })

	second, err := acquireNativeMCPStateFileLock(ctx, path)
	if second != nil {
		_ = second.Close()
		t.Fatal("lock returned authority after cancellation at the poll boundary")
	}
	if releaseErr != nil {
		t.Fatalf("releasing held lock at poll boundary: %v", releaseErr)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("poll-boundary cancellation error = %v, want context.Canceled", err)
	}
}

func TestNativeMCPStateLockReleasesAcquisitionCanceledAtSuccessBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), nativeMCPStateFileName)
	ctx, cancel := context.WithCancel(context.Background())
	nativeMCPStateLockAfterAcquire = func() {
		cancel()
		nativeMCPStateLockAfterAcquire = nil
	}
	t.Cleanup(func() {
		nativeMCPStateLockAfterAcquire = nil
		cancel()
	})

	lock, err := acquireNativeMCPStateFileLock(ctx, path)
	if lock != nil {
		_ = lock.Close()
		t.Fatal("lock returned authority after cancellation at the acquisition boundary")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("acquisition-boundary cancellation error = %v, want context.Canceled", err)
	}

	// The canceled acquisition must have unlocked and closed its descriptor,
	// leaving the permanent sidecar immediately available to a fresh caller.
	reopened, err := acquireNativeMCPStateFileLock(context.Background(), path)
	if err != nil {
		t.Fatalf("canceled acquisition retained the lock: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func runConcurrentNativeMCPMutations(t *testing.T, count int, mutation func(int) error) {
	t.Helper()
	errorsByIndex := make([]error, count)
	var wait sync.WaitGroup
	for index := range count {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsByIndex[index] = mutation(index)
		}()
	}
	wait.Wait()
	for index, err := range errorsByIndex {
		if err != nil {
			t.Fatalf("mutation %d: %v", index, err)
		}
	}
}

func TestNativeMCPActivationCancellationWhileLockHeldDoesNotCommit(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	writeNativeMCPConfig(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {
    "x": {"command":"x"},
    "y": {"command":"y"}
  }
}`)
	x := nativeMCPRequest(t, home, workspace, "claude:x")
	y := nativeMCPRequest(t, home, workspace, "claude:y")
	path := filepath.Join(t.TempDir(), nativeMCPStateFileName)
	state, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.enable(x); err != nil {
		t.Fatal(err)
	}
	held, err := acquireNativeMCPStateFileLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- state.enableWithRequiredContext(ctx, y, false) }()
	deadline := time.Now().Add(time.Second)
	for state.mu.TryLock() {
		state.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("mutation never began waiting for the interprocess lock")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled mutation error = %v", err)
	}
	if state.NativeMCPActivated(x) || state.NativeMCPActivated(y) {
		t.Fatal("ambiguous lock wait left cached activation authority usable")
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.NativeMCPActivated(x) || reopened.NativeMCPActivated(y) {
		t.Fatal("canceled lock wait changed persisted activation state")
	}
}

func TestNativeMCPActivationUnpublishedCancellationKeepsBaselineAuthority(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	writeNativeMCPConfig(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {
    "x": {"command":"x"},
    "y": {"command":"y"}
  }
}`)
	x := nativeMCPRequest(t, home, workspace, "claude:x")
	y := nativeMCPRequest(t, home, workspace, "claude:y")
	path := filepath.Join(t.TempDir(), nativeMCPStateFileName)
	state, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.enable(x); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	nativeMCPStateBeforePublication = cancel
	t.Cleanup(func() { nativeMCPStateBeforePublication = nil })
	if err := state.enableWithRequiredContext(ctx, y, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled unpublished mutation error = %v", err)
	}
	if !state.NativeMCPActivated(x) || state.NativeMCPActivated(y) {
		t.Fatal("unpublished cancellation discarded or changed the validated baseline")
	}
	state.mu.Lock()
	poisoned := state.poisoned
	state.mu.Unlock()
	if poisoned != nil {
		t.Fatalf("unpublished cancellation poisoned the validated baseline: %v", poisoned)
	}
	reopened, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.NativeMCPActivated(x) || reopened.NativeMCPActivated(y) {
		t.Fatal("unpublished cancellation changed persisted activation state")
	}
}

func TestNativeMCPActivationPublishedFailurePoisonsButDoesNotRollBack(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	writeNativeMCPConfig(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {
    "x": {"command":"x"},
    "y": {"command":"y"}
  }
}`)
	x := nativeMCPRequest(t, home, workspace, "claude:x")
	y := nativeMCPRequest(t, home, workspace, "claude:y")
	path := filepath.Join(t.TempDir(), nativeMCPStateFileName)
	state, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.enable(x); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected post-publication state failure")
	nativeMCPStateAfterPublication = func(published bool, publicationErr error) error {
		if !published || publicationErr != nil {
			t.Fatalf("publication seam = published %v, err %v", published, publicationErr)
		}
		return injected
	}
	t.Cleanup(func() { nativeMCPStateAfterPublication = nil })
	if err := state.enable(y); !errors.Is(err, injected) {
		t.Fatalf("published failure error = %v", err)
	}
	if state.NativeMCPActivated(x) || state.NativeMCPActivated(y) {
		t.Fatal("published failure left cached activation authority usable")
	}
	nativeMCPStateAfterPublication = nil
	reopened, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.NativeMCPActivated(x) || !reopened.NativeMCPActivated(y) {
		t.Fatal("published failure was incorrectly rolled back")
	}
}

func TestNativeMCPActivationWriteBoundPrecedesPublicationArtifacts(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, nativeMCPStateFileName)
	state, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	err = state.mutate(context.Background(), func(latest *nativeMCPActivationState) (bool, error) {
		latest.key = make([]byte, 32)
		latest.records["oversize"] = nativeMCPActivationRecord{
			ID: "claude:oversize", RealPath: "/" + strings.Repeat("x", maxNativeMCPStateBytes),
			Digest: strings.Repeat("a", 64),
		}
		return true, nil
	})
	if err == nil || !strings.Contains(err.Error(), "write bound") {
		t.Fatalf("oversized state error = %v", err)
	}
	state.mu.Lock()
	poisoned := state.poisoned
	state.mu.Unlock()
	if poisoned != nil {
		t.Fatalf("deterministic write-bound refusal poisoned the baseline: %v", poisoned)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("oversized state was published: %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != nativeMCPStateFileName+".lock" && entry.Name() != nativeMCPStateRecoveryDirName {
			t.Fatalf("oversized state created publication artifact %q", entry.Name())
		}
	}
	recoveryEntries, err := os.ReadDir(filepath.Join(directory, nativeMCPStateRecoveryDirName))
	if err != nil {
		t.Fatal(err)
	}
	if len(recoveryEntries) != 0 {
		t.Fatalf("oversized state created %d recovery artifacts", len(recoveryEntries))
	}
}

func TestNativeMCPStateRecoveryIgnoresOtherAuthorityLedgers(t *testing.T) {
	directory := t.TempDir()
	foreignName := ".switchboard-undo-cleanup-" + strings.Repeat("a", 32)
	foreignPath := filepath.Join(directory, foreignName)
	const foreign = "another authority owns this ledger\n"
	if err := os.WriteFile(foreignPath, []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openNativeMCPActivationStateFile(filepath.Join(directory, nativeMCPStateFileName)); err != nil {
		t.Fatalf("foreign authority ledger affected native MCP recovery: %v", err)
	}
	raw, err := os.ReadFile(foreignPath)
	if err != nil || string(raw) != foreign {
		t.Fatalf("foreign authority ledger = %q, %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(directory, nativeMCPStateRecoveryDirName)); err != nil {
		t.Fatalf("native MCP recovery namespace was not created separately: %v", err)
	}
}

func nativeMCPRequest(t *testing.T, home, workspace, id string) mcpnative.ActivationRequest {
	t.Helper()
	result := mcpnative.Discover(nativeMCPTestOptions(t, home, workspace))
	request, err := result.ActivationRequest(id)
	if err != nil {
		t.Fatalf("activation request %s: %v; servers=%#v diagnostics=%#v", id, err, result.Servers, result.Diagnostics)
	}
	return request
}

// nativeMCPTestOptions seals the test's user Codex config into the same
// authoritative config/read shape production must obtain from Codex
// app-server. Tests must not accidentally exercise the quarantined fallback
// inventory when they intend to verify executable behavior.
func nativeMCPTestOptions(t *testing.T, home, workspace string) mcpnative.Options {
	t.Helper()
	options := mcpnative.Options{HomeDir: home, Workspace: workspace}
	configPath := filepath.Join(home, ".codex", "config.toml")
	if _, err := os.Stat(configPath); err != nil {
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return options
	}
	var config map[string]any
	if _, err := toml.DecodeFile(configPath, &config); err != nil {
		t.Fatal(err)
	}
	realPath, err := filepath.EvalSymlinks(configPath)
	if err != nil {
		t.Fatal(err)
	}
	name := map[string]any{"type": "user", "file": filepath.Clean(realPath), "profile": nil}
	version := "cmd-test-user-v1"
	origins := map[string]any{}
	if servers, ok := config["mcp_servers"].(map[string]any); ok {
		for serverName, raw := range servers {
			server, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("test Codex server %q is not a table", serverName)
			}
			field := "command"
			if _, exists := server[field]; !exists {
				field = "url"
			}
			origins["mcp_servers."+serverName+"."+field] = map[string]any{"name": name, "version": version}
		}
	}
	result := map[string]any{
		"config": config, "origins": origins,
		"layers": []any{map[string]any{"name": name, "version": version, "config": config}},
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	cwd := workspace
	if cwd == "" {
		cwd = home
	}
	snapshot, err := mcpnative.NewCodexSnapshot(encoded, cwd)
	if err != nil {
		t.Fatalf("test Codex snapshot: %v", err)
	}
	options.CodexSnapshot = snapshot
	return options
}

func writeNativeMCPConfig(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writePrivateNativeMCPState(t *testing.T, path, contents string) {
	t.Helper()
	if err := fileprivacy.EnsurePrivateDir(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	f, err := fileprivacy.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(contents); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
