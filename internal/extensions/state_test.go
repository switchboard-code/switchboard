package extensions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
)

func testPlugin(t *testing.T, root, seed string, executable bool) Plugin {
	t.Helper()
	return testPluginNamed(t, root, DialectClaude, "review-tools", ScopeUser, seed, executable)
}

func testPluginNamed(t *testing.T, root string, dialect Dialect, name string, scope Scope, seed string, executable bool) Plugin {
	t.Helper()
	directory := ".codex-plugin"
	if dialect == DialectClaude {
		directory = ".claude-plugin"
	}
	if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := []byte(fmt.Sprintf(`{"name":%q}`, name))
	if err := os.WriteFile(filepath.Join(root, directory, "plugin.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "content"), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if executable {
		if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return discoverInstallPlugin(t, root, scope, dialect)
}

func testActivationCandidate(t *testing.T, plugin Plugin) *ActivationCandidate {
	t.Helper()
	candidate, err := newActivationCandidate(plugin)
	if err != nil {
		t.Fatal(err)
	}
	return candidate
}

func TestActivationSeparatesEnableFromExecutableTrust(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", StateFileName)
	state, err := OpenStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plugin := testPlugin(t, filepath.Join(t.TempDir(), "review-tools"), strings.Repeat("a", 64), true)
	candidate := testActivationCandidate(t, plugin)

	if got := state.Status(plugin, ""); got != (ActivationStatus{}) {
		t.Fatalf("initial status = %+v", got)
	}
	if err := state.Enable(candidate, ""); err != nil {
		t.Fatal(err)
	}
	if got := state.Status(plugin, ""); !got.Enabled || got.ExecutableTrusted || got.Changed {
		t.Fatalf("enabled status = %+v", got)
	}
	if err := state.TrustExecutable(candidate, ""); err != nil {
		t.Fatal(err)
	}
	if got := state.Status(plugin, ""); !got.Enabled || !got.ExecutableTrusted || got.Changed {
		t.Fatalf("trusted status = %+v", got)
	}

	if err := os.WriteFile(filepath.Join(plugin.RealPath, "content"), []byte(strings.Repeat("b", 64)), 0o600); err != nil {
		t.Fatal(err)
	}
	updated := discoverInstallPlugin(t, plugin.RealPath, plugin.Scope, plugin.Dialect)
	updatedCandidate := testActivationCandidate(t, updated)
	if got := state.Status(updated, ""); !got.Enabled || got.ExecutableTrusted || !got.Changed {
		t.Fatalf("updated status = %+v", got)
	}
	if err := state.TrustExecutable(updatedCandidate, ""); err != nil {
		t.Fatal(err)
	}
	if got := state.Status(updated, ""); !got.ExecutableTrusted || got.Changed {
		t.Fatalf("retrusted status = %+v", got)
	}
}

func TestActivationIsBoundToResolvedPath(t *testing.T) {
	state, err := OpenStateFile(filepath.Join(t.TempDir(), StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("c", 64)
	first := testPlugin(t, filepath.Join(t.TempDir(), "first"), digest, true)
	second := testPlugin(t, filepath.Join(t.TempDir(), "second"), digest, true)
	firstCandidate := testActivationCandidate(t, first)
	if err := state.Enable(firstCandidate, ""); err != nil {
		t.Fatal(err)
	}
	if err := state.TrustExecutable(firstCandidate, ""); err != nil {
		t.Fatal(err)
	}
	if got := state.Status(second, ""); got != (ActivationStatus{}) {
		t.Fatalf("equal ID at another root inherited state: %+v", got)
	}
}

func TestActivationCandidateFailsClosedAfterRootChanges(t *testing.T) {
	state, err := OpenStateFile(filepath.Join(t.TempDir(), StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	plugin := testPlugin(t, filepath.Join(t.TempDir(), "plugin"), "before", true)
	candidate := testActivationCandidate(t, plugin)
	if err := os.WriteFile(filepath.Join(plugin.RealPath, "content"), []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := state.Enable(candidate, ""); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("Enable() error = %v, want changed-candidate rejection", err)
	}
	if got := state.Activations(); len(got) != 0 {
		t.Fatalf("stale candidate changed activation state: %+v", got)
	}
	if err := state.Enable(nil, ""); err == nil || !strings.Contains(err.Error(), "eligibility") {
		t.Fatalf("Enable(nil) error = %v", err)
	}
	if err := state.Enable(&ActivationCandidate{}, ""); err == nil {
		t.Fatal("Enable accepted a zero-value activation candidate")
	}
}

func TestActivationCandidateReturnsDefensivePluginCopy(t *testing.T) {
	plugin := testPlugin(t, filepath.Join(t.TempDir(), "plugin"), "copy", true)
	candidate := testActivationCandidate(t, plugin)
	copy := candidate.Plugin()
	if len(copy.Components) == 0 {
		t.Fatal("test plugin has no components")
	}
	copy.Components[0].DeclaredPath = "tampered"
	if got := candidate.Plugin().Components[0].DeclaredPath; got == "tampered" {
		t.Fatal("ActivationCandidate.Plugin exposed its internal component slice")
	}
}

func TestDisableAndRevokeExecutable(t *testing.T) {
	state, err := OpenStateFile(filepath.Join(t.TempDir(), StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	plugin := testPlugin(t, filepath.Join(t.TempDir(), "plugin"), strings.Repeat("d", 64), true)
	candidate := testActivationCandidate(t, plugin)
	if err := state.Enable(candidate, ""); err != nil {
		t.Fatal(err)
	}
	if err := state.TrustExecutable(candidate, ""); err != nil {
		t.Fatal(err)
	}
	if err := state.RevokeExecutable(plugin, ""); err != nil {
		t.Fatal(err)
	}
	if got := state.Status(plugin, ""); !got.Enabled || got.ExecutableTrusted || got.Changed {
		t.Fatalf("revoked status = %+v", got)
	}
	if err := state.Disable(plugin, ""); err != nil {
		t.Fatal(err)
	}
	if got := state.Status(plugin, ""); got != (ActivationStatus{}) {
		t.Fatalf("disabled status = %+v", got)
	}
}

func TestStatePersistsDeterministicallyAndPrivately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", StateFileName)
	state, err := OpenStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plugins := []Plugin{
		testPluginNamed(t, filepath.Join(t.TempDir(), "z"), DialectCodex, "z", ScopeUser, strings.Repeat("e", 64), false),
		testPluginNamed(t, filepath.Join(t.TempDir(), "a"), DialectCodex, "a", ScopeUser, strings.Repeat("f", 64), false),
	}
	for _, plugin := range plugins {
		if err := state.Enable(testActivationCandidate(t, plugin), ""); err != nil {
			t.Fatal(err)
		}
	}

	f, err := fileprivacy.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ownerOnly, ownerErr := fileprivacy.IsOwnerOnly(f)
	closeErr := f.Close()
	if ownerErr != nil || closeErr != nil || !ownerOnly {
		t.Fatalf("owner-only=%v, check=%v, close=%v", ownerOnly, ownerErr, closeErr)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(string(raw), `"id": "codex:a"`) > strings.Index(string(raw), `"id": "codex:z"`) {
		t.Fatalf("activations are not sorted:\n%s", raw)
	}

	reopened, err := OpenStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Activations(); len(got) != 2 || got[0].ID != "codex:a" || got[1].ID != "codex:z" {
		t.Fatalf("reopened activations = %+v", got)
	}
}

func TestStateParsingFailsClosed(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "plugin")
	digest := strings.Repeat("a", 64)
	cases := map[string]string{
		"unknown field":        `{"version":1,"surprise":true,"activations":[]}`,
		"duplicate field":      `{"version":1,"version":1,"activations":[]}`,
		"duplicate item field": `{"version":1,"activations":[{"id":"codex:p","id":"codex:q","dialect":"codex","scope":"user","real_path":"` + abs + `","enabled":true}]}`,
		"trailing value":       `{"version":1,"activations":[]} {}`,
		"wrong version":        `{"version":2,"activations":[]}`,
		"duplicate":            `{"version":1,"activations":[{"id":"codex:p","dialect":"codex","scope":"user","real_path":"` + abs + `","enabled":true},{"id":"codex:p","dialect":"codex","scope":"user","real_path":"` + abs + `","enabled":true}]}`,
		"trust while disabled": `{"version":1,"activations":[{"id":"codex:p","dialect":"codex","scope":"user","real_path":"` + abs + `","enabled":false,"executable_trust_digest":"` + digest + `"}]}`,
		"invalid digest":       `{"version":1,"activations":[{"id":"codex:p","dialect":"codex","scope":"user","real_path":"` + abs + `","enabled":true,"executable_trust_digest":"nope"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), StateFileName)
			writePrivateStateFile(t, path, []byte(body))
			if _, err := OpenStateFile(path); err == nil {
				t.Fatal("malformed state loaded")
			}
		})
	}
}

func TestActivationRecoverySelectorsAreKeyedStableAndVersionOneMigrates(t *testing.T) {
	plugin := testPlugin(t, filepath.Join(t.TempDir(), "plugin"), "recovery", true)
	activationJSON := fmt.Sprintf(`{"version":1,"activations":[{"id":%q,"dialect":%q,"scope":%q,"real_path":%q,"enabled":true}]}`,
		plugin.ID, plugin.Dialect, plugin.Scope, plugin.RealPath)
	firstPath := filepath.Join(t.TempDir(), StateFileName)
	secondPath := filepath.Join(t.TempDir(), StateFileName)
	for _, path := range []string{firstPath, secondPath} {
		writePrivateStateFile(t, path, []byte(activationJSON))
	}
	first, err := OpenStateFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := OpenStateFile(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	firstRefs, err := first.ActivationReferencesFor(t.TempDir())
	if err != nil || len(firstRefs) != 1 {
		t.Fatalf("first references = %#v, %v", firstRefs, err)
	}
	secondRefs, err := second.ActivationReferencesFor(t.TempDir())
	if err != nil || len(secondRefs) != 1 {
		t.Fatalf("second references = %#v, %v", secondRefs, err)
	}
	if firstRefs[0].RecoveryToken == secondRefs[0].RecoveryToken {
		t.Fatal("separate state keys produced the same recovery selector")
	}
	if strings.Contains(firstRefs[0].RecoveryToken, plugin.ID) || strings.Contains(firstRefs[0].RecoveryToken, plugin.RealPath) {
		t.Fatalf("recovery selector exposed identity: %q", firstRefs[0].RecoveryToken)
	}
	reopened, err := OpenStateFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	reopenedRefs, err := reopened.ActivationReferencesFor(t.TempDir())
	if err != nil || len(reopenedRefs) != 1 || reopenedRefs[0].RecoveryToken != firstRefs[0].RecoveryToken {
		t.Fatalf("migrated selector was not stable: before=%#v after=%#v err=%v", firstRefs, reopenedRefs, err)
	}
	raw, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"version": 2`) || !strings.Contains(string(raw), `"key":`) {
		t.Fatalf("version-one state was not migrated with a key: %s", raw)
	}
}

func TestStateMutationReloadsLatestAcrossIndependentHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), StateFileName)
	seed, err := OpenStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	x := testPluginNamed(t, filepath.Join(t.TempDir(), "x"), DialectClaude, "x", ScopeUser, "x", false)
	y := testPluginNamed(t, filepath.Join(t.TempDir(), "y"), DialectClaude, "y", ScopeUser, "y", false)
	if err := seed.Enable(testActivationCandidate(t, x), ""); err != nil {
		t.Fatal(err)
	}
	a, err := OpenStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := OpenStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Disable(x, ""); err != nil {
		t.Fatal(err)
	}
	if err := b.Enable(testActivationCandidate(t, y), ""); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	activations := reopened.Activations()
	if len(activations) != 1 || activations[0].ID != y.ID {
		t.Fatalf("stale handle resurrected removed authority: %#v", activations)
	}
}

func TestConcurrentIndependentStateHandlesPreserveEveryActivation(t *testing.T) {
	const writers = 8
	path := filepath.Join(t.TempDir(), StateFileName)
	handles := make([]*State, writers)
	candidates := make([]*ActivationCandidate, writers)
	for i := range writers {
		state, err := OpenStateFile(path)
		if err != nil {
			t.Fatal(err)
		}
		handles[i] = state
		plugin := testPluginNamed(
			t,
			filepath.Join(t.TempDir(), fmt.Sprintf("plugin-%d", i)),
			DialectClaude,
			fmt.Sprintf("concurrent-%d", i),
			ScopeUser,
			fmt.Sprintf("content-%d", i),
			false,
		)
		candidates[i] = testActivationCandidate(t, plugin)
	}

	results := make(chan error, writers)
	for i := range writers {
		go func() {
			results <- handles[i].Enable(candidates[i], "")
		}()
	}
	for range writers {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}

	reopened, err := OpenStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if activations := reopened.Activations(); len(activations) != writers {
		t.Fatalf("concurrent activations = %d, want %d: %#v", len(activations), writers, activations)
	}
}

func TestCancelledMutationWaitingForFileLockDoesNotCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), StateFileName)
	state, err := OpenStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plugin := testPluginNamed(t, filepath.Join(t.TempDir(), "held"), DialectClaude, "held", ScopeUser, "held", false)
	if err := state.Enable(testActivationCandidate(t, plugin), ""); err != nil {
		t.Fatal(err)
	}
	held, err := acquirePluginStateLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- state.DisableContext(ctx, plugin, "") }()
	select {
	case err := <-result:
		held.Close()
		t.Fatalf("mutation did not wait for held lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled mutation error = %v", err)
	}
	reopened, err := OpenStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if activations := reopened.Activations(); len(activations) != 1 || activations[0].ID != plugin.ID {
		t.Fatalf("cancelled mutation committed: %#v", activations)
	}
}

func TestStateRejectsSymlinkAndLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	body := []byte(`{"version":1,"activations":[]}`)
	writePrivateStateFile(t, real, body)
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := OpenStateFile(link); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlink error = %v", err)
	}
	if os.PathSeparator == '\\' {
		return
	}
	if err := os.Chmod(real, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStateFile(real); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("permission error = %v", err)
	}
}

func TestFailedSaveRollsBackInMemoryActivation(t *testing.T) {
	parentFile := filepath.Join(t.TempDir(), "state-parent")
	if err := os.Mkdir(parentFile, 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := OpenStateFile(filepath.Join(parentFile, StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(parentFile, pluginStateRecoveryDirectory)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(parentFile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parentFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	plugin := testPlugin(t, filepath.Join(t.TempDir(), "plugin"), strings.Repeat("a", 64), true)
	if err := state.Enable(testActivationCandidate(t, plugin), ""); err == nil {
		t.Fatal("enable unexpectedly saved")
	}
	if got := state.Status(plugin, ""); got != (ActivationStatus{}) {
		t.Fatalf("failed enable remained live: %+v", got)
	}
}

func TestStateParsingBoundsInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), StateFileName)
	writePrivateStateFile(t, path, make([]byte, maxStateBytes+1))
	if _, err := OpenStateFile(path); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v", err)
	}
}

func TestStateMutationRejectsOversizeBeforePublicationArtifacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", StateFileName)
	state, err := OpenStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plugin := testPlugin(t, filepath.Join(t.TempDir(), "plugin"), "oversize", false)
	candidate := testActivationCandidate(t, plugin)
	hookCalled := false
	pluginStateBeforePublicationTestHook = func() { hookCalled = true }
	t.Cleanup(func() { pluginStateBeforePublicationTestHook = nil })
	err = state.EnableWithNativeIDs(candidate, "", []string{strings.Repeat("n", maxStateBytes)})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize mutation error = %v", err)
	}
	if hookCalled {
		t.Fatal("oversize mutation reached the publication seam")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("oversize mutation created state: %v", err)
	}
	if got := state.Status(plugin, ""); got != (ActivationStatus{}) {
		t.Fatalf("oversize mutation remained active in memory: %+v", got)
	}
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(path), pluginStateRecoveryDirectory))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("oversize mutation left publication artifacts: %v", entries)
	}
}

func TestPluginStateRecoveryDoesNotInspectAnotherAuthorityNamespace(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(parent, StateFileName)
	state, err := OpenStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	foreignDir := filepath.Join(parent, ".native-mcp-state-recovery")
	if err := fileprivacy.EnsurePrivateDir(foreignDir); err != nil {
		t.Fatal(err)
	}
	foreignLedger := filepath.Join(foreignDir, ".switchboard-undo-cleanup-"+strings.Repeat("a", 32))
	foreignBytes := []byte("deliberately not a plugin-state cleanup ledger\n")
	foreign, err := fileprivacy.Create(foreignLedger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.Write(foreignBytes); err != nil {
		_ = foreign.Close()
		t.Fatal(err)
	}
	if err := foreign.Close(); err != nil {
		t.Fatal(err)
	}

	plugin := testPlugin(t, filepath.Join(t.TempDir(), "plugin"), "isolated-recovery", false)
	if err := state.Enable(testActivationCandidate(t, plugin), ""); err != nil {
		t.Fatalf("plugin mutation inspected another authority's ledger: %v", err)
	}
	got, err := os.ReadFile(foreignLedger)
	if err != nil || string(got) != string(foreignBytes) {
		t.Fatalf("foreign recovery ledger = %q, %v", got, err)
	}
}

func writePrivateStateFile(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := fileprivacy.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExecutableTrustRequiresEnabledExecutablePlugin(t *testing.T) {
	state, err := OpenStateFile(filepath.Join(t.TempDir(), StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	plugin := testPlugin(t, filepath.Join(t.TempDir(), "plugin"), strings.Repeat("a", 64), true)
	candidate := testActivationCandidate(t, plugin)
	if err := state.TrustExecutable(candidate, ""); err == nil {
		t.Fatal("trusted a disabled plugin")
	}
	promptOnly := testPlugin(t, filepath.Join(t.TempDir(), "prompt-only"), strings.Repeat("b", 64), false)
	promptCandidate := testActivationCandidate(t, promptOnly)
	if err := state.Enable(promptCandidate, ""); err != nil {
		t.Fatal(err)
	}
	if err := state.TrustExecutable(promptCandidate, ""); err == nil {
		t.Fatal("trusted a prompt-only plugin")
	}
	plugin.Digest = "short"
	if _, err := newActivationCandidate(plugin); err == nil {
		t.Fatal("created an activation candidate with an invalid plugin digest")
	}
}

func TestProjectActivationIsBoundToExactWorkspace(t *testing.T) {
	state, err := OpenStateFile(filepath.Join(t.TempDir(), StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	firstWorkspace := t.TempDir()
	secondWorkspace := t.TempDir()
	plugin := testPluginNamed(t, filepath.Join(t.TempDir(), "plugin"), DialectClaude, "review-tools", ScopeWorkspace, strings.Repeat("b", 64), true)
	candidate := testActivationCandidate(t, plugin)

	if err := state.Enable(candidate, firstWorkspace); err != nil {
		t.Fatal(err)
	}
	if err := state.TrustExecutable(candidate, firstWorkspace); err != nil {
		t.Fatal(err)
	}
	if got := state.Status(plugin, firstWorkspace); !got.Enabled || !got.ExecutableTrusted {
		t.Fatalf("first workspace status = %+v", got)
	}
	if got := state.Status(plugin, secondWorkspace); got != (ActivationStatus{}) {
		t.Fatalf("second workspace inherited project activation: %+v", got)
	}
	if got := state.Status(plugin, ""); got != (ActivationStatus{}) {
		t.Fatalf("missing workspace inherited project activation: %+v", got)
	}
}

func TestProjectActivationRequiresResolvableWorkspace(t *testing.T) {
	state, err := OpenStateFile(filepath.Join(t.TempDir(), StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	plugin := testPluginNamed(t, filepath.Join(t.TempDir(), "plugin"), DialectClaude, "review-tools", ScopeLocal, strings.Repeat("c", 64), false)
	candidate := testActivationCandidate(t, plugin)
	if err := state.Enable(candidate, ""); err == nil {
		t.Fatal("enabled a project-scoped plugin without a workspace")
	}
	if err := state.Enable(candidate, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("enabled a project-scoped plugin for a missing workspace")
	}
}

func TestActivationsForFiltersOtherWorkspacesButKeepsGlobal(t *testing.T) {
	state, err := OpenStateFile(filepath.Join(t.TempDir(), StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	firstWorkspace := t.TempDir()
	secondWorkspace := t.TempDir()
	global := testPlugin(t, filepath.Join(t.TempDir(), "global"), strings.Repeat("d", 64), false)
	project := testPluginNamed(t, filepath.Join(t.TempDir(), "project"), DialectClaude, "project", ScopeWorkspace, strings.Repeat("e", 64), false)
	other := testPluginNamed(t, filepath.Join(t.TempDir(), "other"), DialectClaude, "other", ScopeLocal, strings.Repeat("f", 64), false)
	if err := state.Enable(testActivationCandidate(t, global), ""); err != nil {
		t.Fatal(err)
	}
	if err := state.Enable(testActivationCandidate(t, project), firstWorkspace); err != nil {
		t.Fatal(err)
	}
	if err := state.Enable(testActivationCandidate(t, other), secondWorkspace); err != nil {
		t.Fatal(err)
	}

	got, err := state.ActivationsFor(firstWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "claude:project" || got[1].ID != "claude:review-tools" {
		t.Fatalf("first workspace activations = %+v", got)
	}
}
