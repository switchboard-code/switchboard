package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/mcpnative"
)

func TestNativeMCPCLIRequiresIndependentActivationAndTracksChanges(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	configPath := filepath.Join(home, ".codex", "config.toml")
	writeNativeMCPConfig(t, configPath, `[mcp_servers.docs]
command = "secret-command-value"
args = ["secret-argument-value"]
`)
	state, err := openNativeMCPActivationStateFile(filepath.Join(t.TempDir(), nativeMCPStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	open := func() *nativeMCPInventory {
		return discoverNativeMCP(nativeMCPTestOptions(t, home, workspace), state)
	}

	var output bytes.Buffer
	if err := runNativeMCPAction(&output, workspace, open(), []string{"list"}); err != nil {
		t.Fatal(err)
	}
	if text := output.String(); !strings.Contains(text, "codex:docs\tdisabled\tenabled\tstdio") ||
		strings.Contains(text, "secret-command-value") || strings.Contains(text, "secret-argument-value") {
		t.Fatalf("unactivated/redacted list = %q", text)
	}

	output.Reset()
	if err := runNativeMCPAction(&output, workspace, open(), []string{"enable", "codex:docs"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "enabled codex:docs") {
		t.Fatalf("enable output = %q", output.String())
	}
	output.Reset()
	if err := runNativeMCPAction(&output, workspace, open(), []string{"list"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "codex:docs\tenabled\tenabled\tstdio") {
		t.Fatalf("activated list = %q", output.String())
	}

	writeNativeMCPConfig(t, configPath, `[mcp_servers.docs]
command = "different-secret-command"
`)
	output.Reset()
	if err := runNativeMCPAction(&output, workspace, open(), []string{"list"}); err != nil {
		t.Fatal(err)
	}
	if text := output.String(); !strings.Contains(text, "codex:docs\tchanged\tenabled\tstdio") || strings.Contains(text, "different-secret-command") {
		t.Fatalf("changed list = %q", text)
	}
	if err := runNativeMCPAction(&output, workspace, open(), []string{"disable", "codex:docs"}); err != nil {
		t.Fatal(err)
	}
}

func TestNativeMCPActionCancellationPreventsActivationMutation(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	writeNativeMCPConfig(t, filepath.Join(home, ".claude.json"), `{"mcpServers":{"docs":{"command":"docs"}}}`)
	state, err := openNativeMCPActivationStateFile(filepath.Join(t.TempDir(), nativeMCPStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	inv := discoverNativeMCP(nativeMCPTestOptions(t, home, workspace), state)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = runNativeMCPActionContext(ctx, &bytes.Buffer{}, workspace, inv, []string{"enable", "claude:docs"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled enable = %v", err)
	}
	if state.hasDialect(mcpnative.DialectClaude) {
		t.Fatal("cancelled enable persisted activation state")
	}
}

func TestNativeMCPCLIUsesDialectQualifiedIDsForAmbiguousNames(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	writeNativeMCPConfig(t, filepath.Join(home, ".codex", "config.toml"), `[mcp_servers.same]
command = "codex-server"
`)
	writeNativeMCPConfig(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {"same": {"command":"claude-server"}}
}`)
	state, err := openNativeMCPActivationStateFile(filepath.Join(t.TempDir(), nativeMCPStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	inv := discoverNativeMCP(nativeMCPTestOptions(t, home, workspace), state)
	var output bytes.Buffer
	if err := runNativeMCPAction(&output, workspace, inv, []string{"enable", "same"}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("short-name ambiguity = %v", err)
	}
	if err := runNativeMCPAction(&output, workspace, inv, []string{"enable", "claude:same"}); err != nil {
		t.Fatal(err)
	}
}

func TestNativeMCPCLIRecoversRemovedOptionalCodexActivationWithoutBinary(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	isolateTestHome(t, home)
	t.Setenv("PATH", filepath.Join(home, "no-binaries"))
	t.Setenv("CODEX_HOME", "")
	configPath := filepath.Join(home, ".codex", "config.toml")
	writeNativeMCPConfig(t, configPath, `[mcp_servers.docs]
command = "secret-docs-command"
`)
	state, err := openNativeMCPActivationStateFile(filepath.Join(home, ".switchboard", nativeMCPStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	initial := discoverNativeMCP(nativeMCPTestOptions(t, home, workspace), state)
	var output bytes.Buffer
	if err := runNativeMCPAction(&output, workspace, initial, []string{"enable", "codex:docs"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	canonicalConfigPath, err := filepath.EvalSymlinks(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	canonicalConfigPath = filepath.Join(canonicalConfigPath, filepath.Base(configPath))

	// A missing Codex binary cannot prove the saved definition still exists.
	// Explicitly optional activations stay off without bricking unrelated MCP
	// assembly, while the saved non-secret identity remains recoverable.
	inv := openNativeMCPInventory(context.Background(), workspace, false)
	if inv.codexSnapshotErr != nil {
		t.Fatalf("optional stale activation blocked startup: %v", inv.codexSnapshotErr)
	}
	if specs, _, err := activatedNativeMCPSpecs(inv, nil, nativeMCPAssemblyPolicy{}); err != nil || len(specs) != 0 {
		t.Fatalf("optional stale assembly: specs=%#v err=%v", specs, err)
	}
	output.Reset()
	if err := runNativeMCPAction(&output, workspace, inv, []string{"list"}); err != nil {
		t.Fatal(err)
	}
	listed := output.String()
	if !strings.Contains(listed, "codex:docs\tsaved-unavailable\tunknown\t-\t"+canonicalConfigPath) ||
		strings.Contains(listed, "secret-docs-command") || strings.Contains(listed, "digest") {
		t.Fatalf("stale recovery list = %q", listed)
	}

	output.Reset()
	if err := runMCPCLI(&output, workspace, []string{"disable", "codex:docs"}); err != nil {
		t.Fatal(err)
	}
	if text := output.String(); !strings.Contains(text, "disabled codex:docs") || !strings.Contains(text, "next Switchboard run") {
		t.Fatalf("stale disable output = %q", text)
	}
	reopened, err := openNativeMCPActivationStateFile(filepath.Join(home, ".switchboard", nativeMCPStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if reopened.hasDialect(mcpnative.DialectCodex) {
		t.Fatal("stale Codex activation survived exact disable")
	}
	after := openNativeMCPInventory(context.Background(), workspace, false)
	for _, note := range after.notes {
		if strings.Contains(note.text, "Codex MCP snapshot is unavailable") {
			t.Fatalf("cleared activation still forced Codex app-server: %#v", after.notes)
		}
	}
}

func TestNativeMCPMissingSnapshotStillFailsRequiredActivation(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	isolateTestHome(t, home)
	t.Setenv("PATH", filepath.Join(home, "no-binaries"))
	t.Setenv("CODEX_HOME", "")
	configPath := filepath.Join(home, ".codex", "config.toml")
	writeNativeMCPConfig(t, configPath, `[mcp_servers.required_docs]
command = "required-docs-command"
required = true
`)
	state, err := openNativeMCPActivationStateFile(filepath.Join(home, ".switchboard", nativeMCPStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	initial := discoverNativeMCP(nativeMCPTestOptions(t, home, workspace), state)
	if err := runNativeMCPAction(&bytes.Buffer{}, workspace, initial, []string{"enable", "codex:required_docs"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	inv := openNativeMCPInventory(context.Background(), workspace, false)
	if inv.codexSnapshotErr == nil {
		t.Fatal("required activation did not preserve fail-closed snapshot semantics")
	}
	if _, _, err := activatedNativeMCPSpecs(inv, nil, nativeMCPAssemblyPolicy{}); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("required stale assembly error = %v", err)
	}
}

func TestNativeMCPRequiredProjectActivationDoesNotApplyToAnotherWorkspace(t *testing.T) {
	home := t.TempDir()
	workspaceA, workspaceB := t.TempDir(), t.TempDir()
	isolateTestHome(t, home)
	t.Setenv("PATH", filepath.Join(home, "no-binaries"))
	t.Setenv("CODEX_HOME", "")
	statePath := filepath.Join(home, ".switchboard", nativeMCPStateFileName)
	state, err := openNativeMCPActivationStateFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	canonicalA, ok := canonicalNativeMCPWorkspace(workspaceA)
	if !ok {
		t.Fatal("workspace A did not resolve")
	}
	required := true
	record := nativeMCPActivationRecord{
		ID: "codex:project_docs", RealPath: filepath.Join(home, ".codex", "config.toml"),
		TrustRoot: canonicalA, Digest: strings.Repeat("a", 64), Required: &required,
	}
	if err := state.mutate(context.Background(), func(latest *nativeMCPActivationState) (bool, error) {
		latest.key = bytes.Repeat([]byte{1}, 32)
		latest.records[nativeMCPActivationKey(record.identity())] = record
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	referencesA := state.references(workspaceA)
	if len(referencesA) != 1 {
		t.Fatalf("workspace A references = %#v", referencesA)
	}
	token := referencesA[0].RecoveryToken

	invB := openNativeMCPInventory(context.Background(), workspaceB, false)
	if invB.codexSnapshotErr != nil {
		t.Fatalf("workspace A required activation blocked workspace B: %v", invB.codexSnapshotErr)
	}
	for _, note := range invB.notes {
		if strings.Contains(note.text, "Codex MCP snapshot") {
			t.Fatalf("workspace B attempted Codex snapshot for workspace A state: %#v", invB.notes)
		}
	}
	var output bytes.Buffer
	writeNativeMCPList(&output, invB)
	if strings.Contains(output.String(), token) || strings.Contains(output.String(), "codex:project_docs") {
		t.Fatalf("workspace B listed workspace A recovery state: %q", output.String())
	}
	if err := runNativeMCPAction(&bytes.Buffer{}, workspaceB, invB, []string{"disable", token}); err == nil {
		t.Fatal("workspace B disabled workspace A activation by recovery token")
	}
	if len(state.references(workspaceA)) != 1 {
		t.Fatal("workspace B mutation removed workspace A activation")
	}

	invA := openNativeMCPInventory(context.Background(), workspaceA, false)
	if invA.codexSnapshotErr == nil {
		t.Fatal("required workspace A activation did not preserve snapshot failure")
	}
	output.Reset()
	writeNativeMCPList(&output, invA)
	if !strings.Contains(output.String(), token) || !strings.Contains(output.String(), "codex:project_docs") {
		t.Fatalf("workspace A recovery state was not listed: %q", output.String())
	}
	if err := runNativeMCPAction(&bytes.Buffer{}, workspaceA, invA, []string{"disable", token}); err != nil {
		t.Fatal(err)
	}
	reopened, err := openNativeMCPActivationStateFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.references(workspaceA)) != 0 {
		t.Fatal("workspace A exact recovery disable did not remove its activation")
	}
}

func TestNativeMCPStaleRecoveryIsScopedToCurrentWorkspace(t *testing.T) {
	state, err := openNativeMCPActivationStateFile(filepath.Join(t.TempDir(), nativeMCPStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	firstWorkspace, secondWorkspace := t.TempDir(), t.TempDir()
	claudeState := map[string]any{
		"mcpServers": map[string]any{"global": map[string]any{"command": "global"}},
		"projects": map[string]any{
			firstWorkspace: map[string]any{"mcpServers": map[string]any{
				"docs": map[string]any{"command": "docs"},
			}},
			secondWorkspace: map[string]any{"mcpServers": map[string]any{
				"docs": map[string]any{"command": "docs"},
			}},
		}}
	encoded, err := json.Marshal(claudeState)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(home, ".claude.json")
	writeNativeMCPConfig(t, statePath, string(encoded))
	for _, workspace := range []string{firstWorkspace, secondWorkspace} {
		if err := state.enableWithRequired(nativeMCPRequest(t, home, workspace, "claude:docs"), false); err != nil {
			t.Fatal(err)
		}
	}
	if err := state.enableWithRequired(nativeMCPRequest(t, home, firstWorkspace, "claude:global"), false); err != nil {
		t.Fatal(err)
	}
	firstReferences := state.references(firstWorkspace)
	secondReferences := state.references(secondWorkspace)
	if len(firstReferences) != 2 || len(secondReferences) != 2 {
		t.Fatalf("workspace references: first=%#v second=%#v", firstReferences, secondReferences)
	}
	var firstProjectToken, secondProjectToken string
	for _, reference := range firstReferences {
		if reference.TrustRoot != "" {
			firstProjectToken = reference.RecoveryToken
		}
	}
	for _, reference := range secondReferences {
		if reference.TrustRoot != "" {
			secondProjectToken = reference.RecoveryToken
		}
	}
	if firstProjectToken == "" || secondProjectToken == "" || firstProjectToken == secondProjectToken {
		t.Fatalf("workspace recovery tokens: first=%q second=%q", firstProjectToken, secondProjectToken)
	}

	inv := &nativeMCPInventory{state: state, workspace: firstWorkspace}
	var output bytes.Buffer
	writeNativeMCPList(&output, inv)
	if !strings.Contains(output.String(), firstProjectToken) || strings.Contains(output.String(), secondProjectToken) {
		t.Fatalf("workspace A recovery list crossed workspace boundary: %q", output.String())
	}
	if err := runNativeMCPAction(&bytes.Buffer{}, firstWorkspace, inv, []string{"disable", "claude:docs"}); err != nil {
		t.Fatal(err)
	}
	if remaining := state.references(firstWorkspace); len(remaining) != 1 || remaining[0].TrustRoot != "" {
		t.Fatalf("workspace A disable left %#v", remaining)
	}
	if remaining := state.references(secondWorkspace); len(remaining) != 2 {
		t.Fatalf("workspace A disable affected workspace B: %#v", remaining)
	}
}

func TestNativeMCPInspectReportsOnlyNamesAndSemantics(t *testing.T) {
	home := t.TempDir()
	writeNativeMCPConfig(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {"remote": {
    "type":"http",
    "url":"https://secret-host.example/mcp",
    "headers":{"Authorization":"secret-header", "X-Tenant":"secret-tenant"}
  }}
}`)
	state, err := openNativeMCPActivationStateFile(filepath.Join(t.TempDir(), nativeMCPStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	inv := discoverNativeMCP(nativeMCPTestOptions(t, home, ""), state)
	var output bytes.Buffer
	if err := runNativeMCPAction(&output, "", inv, []string{"inspect", "claude:remote"}); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, absent := range []string{"secret-host", "secret-header", "secret-tenant"} {
		if strings.Contains(text, absent) {
			t.Fatalf("inspect leaked %q: %s", absent, text)
		}
	}
	if !strings.Contains(text, "header names: Authorization, X-Tenant") {
		t.Fatalf("inspect omitted safe header inventory: %s", text)
	}
}

func TestNativeClaudeManagedMCPPaths(t *testing.T) {
	if got := nativeClaudeManagedMCPPath("darwin", ""); got != "/Library/Application Support/ClaudeCode/managed-mcp.json" {
		t.Fatalf("darwin managed path = %q", got)
	}
	if got := nativeClaudeManagedMCPPath("linux", ""); got != "/etc/claude-code/managed-mcp.json" {
		t.Fatalf("linux managed path = %q", got)
	}
	if got := nativeClaudeManagedMCPPath("windows", `D:\Programs`); got != filepath.Join(`D:\Programs`, "ClaudeCode", "managed-mcp.json") {
		t.Fatalf("windows managed path = %q", got)
	}
}
