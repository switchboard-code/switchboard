package mcppolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/switchboard-code/switchboard/internal/mcpnative"
)

type capturedPolicyRequest struct {
	request mcpnative.PolicyRequest
}

func (capture *capturedPolicyRequest) NativeMCPAllowed(request mcpnative.PolicyRequest) (bool, error) {
	capture.request = request
	return false, nil
}

func requestFor(t *testing.T, result mcpnative.Result, id string) mcpnative.PolicyRequest {
	t.Helper()
	capture := &capturedPolicyRequest{}
	_, _ = result.Materialize(id, nil, capture, nil)
	if capture.request.ID == "" {
		t.Fatalf("policy request for %q was not captured", id)
	}
	return capture.request
}

func testPolicyOptions(t *testing.T) (Options, Paths) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "workspace")
	system := filepath.Join(root, "system")
	for _, directory := range []string{home, workspace, system, filepath.Join(home, ".codex"), filepath.Join(home, ".claude")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	paths := Paths{
		CodexRequirements:     filepath.Join(system, "requirements.toml"),
		CodexAuth:             filepath.Join(home, ".codex", "auth.json"),
		ClaudeManagedSettings: filepath.Join(system, "managed-settings.json"),
		ClaudeManagedDropIns:  filepath.Join(system, "managed-settings.d"),
		ClaudeManagedMCP:      filepath.Join(system, "managed-mcp.json"),
		ClaudeRemoteSettings:  filepath.Join(home, ".claude", "remote-settings.json"),
		ClaudeState:           filepath.Join(home, ".claude.json"),
		ClaudeUserSettings:    filepath.Join(home, ".claude", "settings.json"),
		ClaudeProjectSettings: filepath.Join(workspace, ".claude", "settings.json"),
		ClaudeLocalSettings:   filepath.Join(workspace, ".claude", "settings.local.json"),
	}
	return Options{
		HomeDir: home, Workspace: workspace, GOOS: runtime.GOOS,
		Paths: &paths, CloudRequirementsChecked: true, StartupEnv: []string{},
	}, paths
}

func writeTestFile(t *testing.T, name, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func discoverCodex(t *testing.T, options Options, config string) mcpnative.Result {
	t.Helper()
	configPath := filepath.Join(options.HomeDir, ".codex", "config.toml")
	writeTestFile(t, configPath, config)
	var effective map[string]any
	if _, err := toml.Decode(config, &effective); err != nil {
		t.Fatal(err)
	}
	realPath, err := filepath.EvalSymlinks(configPath)
	if err != nil {
		t.Fatal(err)
	}
	name := map[string]any{"type": "user", "file": filepath.Clean(realPath), "profile": nil}
	version := "mcppolicy-test-user-v1"
	origins := map[string]any{}
	if servers, ok := effective["mcp_servers"].(map[string]any); ok {
		for serverName, raw := range servers {
			server, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("test server %q is not a table", serverName)
			}
			field := "command"
			if _, exists := server[field]; !exists {
				field = "url"
			}
			origins["mcp_servers."+serverName+"."+field] = map[string]any{"name": name, "version": version}
		}
	}
	encoded, err := json.Marshal(map[string]any{
		"config": effective, "origins": origins,
		"layers": []any{map[string]any{"name": name, "version": version, "config": effective}},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := mcpnative.NewCodexSnapshot(encoded, options.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	return mcpnative.Discover(mcpnative.Options{
		HomeDir: options.HomeDir, Workspace: options.Workspace,
		CodexSnapshot: snapshot,
	})
}

func discoverClaude(t *testing.T, options Options, config string) mcpnative.Result {
	t.Helper()
	writeTestFile(t, filepath.Join(options.HomeDir, ".claude.json"), config)
	return mcpnative.Discover(mcpnative.Options{HomeDir: options.HomeDir, Workspace: options.Workspace})
}

func TestResolvePathsGolden(t *testing.T) {
	linux, err := ResolvePaths(Options{GOOS: "linux", HomeDir: "/home/alice", Workspace: "/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if linux.CodexRequirements != "/etc/codex/requirements.toml" ||
		linux.ClaudeState != "/home/alice/.claude.json" ||
		linux.ClaudeManagedSettings != "/etc/claude-code/managed-settings.json" ||
		linux.ClaudeManagedDropIns != "/etc/claude-code/managed-settings.d" ||
		linux.ClaudeManagedMCP != "/etc/claude-code/managed-mcp.json" {
		t.Fatalf("Linux paths: %#v", linux)
	}

	mac, err := ResolvePaths(Options{GOOS: "darwin", HomeDir: "/Users/alice", Workspace: "/repo", UserName: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if mac.ClaudeManagedSettings != "/Library/Application Support/ClaudeCode/managed-settings.json" ||
		len(mac.CodexMDM) != 2 || !strings.Contains(mac.CodexMDM[0], "/alice/com.openai.codex.plist") {
		t.Fatalf("macOS paths: %#v", mac)
	}

	windows, err := ResolvePaths(Options{
		GOOS: "windows", HomeDir: `C:\Users\alice`, Workspace: `D:\repo`,
		ProgramData: `C:\ProgramData`, ProgramFiles: `C:\Program Files`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if windows.CodexRequirements != `C:\ProgramData\OpenAI\Codex\requirements.toml` ||
		windows.ClaudeState != `C:\Users\alice\.claude.json` ||
		windows.ClaudeManagedSettings != `C:\Program Files\ClaudeCode\managed-settings.json` ||
		windows.ClaudeManagedMCP != `C:\Program Files\ClaudeCode\managed-mcp.json` {
		t.Fatalf("Windows paths: %#v", windows)
	}
	unc, err := ResolvePaths(Options{GOOS: "windows", HomeDir: `\\host\users\alice`, Workspace: `\\host\repos\one`})
	if err != nil || unc.CodexAuth != `\\host\users\alice\.codex\auth.json` {
		t.Fatalf("Windows UNC paths: paths=%#v err=%v", unc, err)
	}
}

func TestResolvePathsClaudeConfigDirState(t *testing.T) {
	paths, err := ResolvePaths(Options{
		GOOS: "linux", HomeDir: "/home/alice", Workspace: "/repo",
		ClaudeConfigDir: "/custom/claude",
	})
	if err != nil {
		t.Fatal(err)
	}
	if paths.ClaudeState != "/custom/claude/.claude.json" {
		t.Fatalf("Claude state path = %q", paths.ClaudeState)
	}
}

func TestEmptyManagedDropInDirectoryIsValid(t *testing.T) {
	options, paths := testPolicyOptions(t)
	if err := os.MkdirAll(paths.ClaudeManagedDropIns, 0o700); err != nil {
		t.Fatal(err)
	}
	_, diagnostics, err := Load(options)
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("empty drop-in directory: diagnostics=%v err=%v", diagnostics, err)
	}
}

func TestClaudePluginConstraintsComposeManagedDropInsAndQuarantineOpaqueSurfaces(t *testing.T) {
	options, paths := testPolicyOptions(t)
	writeTestFile(t, filepath.Join(paths.ClaudeManagedDropIns, "10-plugins.json"), `{
  "enabledPlugins":{"alpha@market":false,"beta@market":true}
}`)
	checker, diagnostics, err := Load(options)
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("drop-in load: diagnostics=%v err=%v", diagnostics, err)
	}
	constraints, err := checker.ClaudePluginConstraints()
	if err != nil || len(constraints) != 1 || constraints[0].NativeID != "alpha@market" || !constraints[0].Denied {
		t.Fatalf("drop-in plugin constraints = %#v, %v", constraints, err)
	}

	writeTestFile(t, paths.ClaudeRemoteSettings, `{"enabledPlugins":{"alpha@market":false}}`)
	checker, diagnostics, err = Load(options)
	if err != nil || len(diagnostics) == 0 {
		t.Fatalf("remote detection: diagnostics=%v err=%v", diagnostics, err)
	}
	if _, err := checker.ClaudePluginConstraints(); !errors.Is(err, ErrClaudePolicyUnavailable) {
		t.Fatalf("undecoded remote policy did not quarantine plugins: %v", err)
	}

	if err := os.Remove(paths.ClaudeRemoteSettings); err != nil {
		t.Fatal(err)
	}
	paths.ClaudeMDM = []string{filepath.Join(filepath.Dir(paths.ClaudeManagedSettings), "com.anthropic.claudecode.plist")}
	options.Paths = &paths
	writeTestFile(t, paths.ClaudeMDM[0], "opaque-managed-settings")
	checker, diagnostics, err = Load(options)
	if err != nil || len(diagnostics) == 0 {
		t.Fatalf("MDM detection: diagnostics=%v err=%v", diagnostics, err)
	}
	if _, err := checker.ClaudePluginConstraints(); !errors.Is(err, ErrClaudePolicyUnavailable) {
		t.Fatalf("undecoded MDM policy did not quarantine plugins: %v", err)
	}
}

func TestCodexRequirementsLayeringAndStructuredIdentity(t *testing.T) {
	options, paths := testPolicyOptions(t)
	writeTestFile(t, paths.CodexRequirements, `
[mcp_servers.tool.identity.command]
executable = "wrong"
args = [{ match = "exact", value = "serve" }]
`)
	options.CloudRequirements = []Document{NewDocument("cloud", []byte(`
[mcp_servers.tool.identity.command]
executable = "npx"
args = [
  { match = "exact", value = "serve" },
  { match = "prefix", value = "--workspace=" },
]
`))}
	checker, diagnostics, err := Load(options)
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("Load: diagnostics=%v err=%v", diagnostics, err)
	}
	result := discoverCodex(t, options, `
[mcp_servers.tool]
command = "npx"
args = ["serve", "--workspace=/repo"]
`)
	allowed, policyErr := checker.NativeMCPAllowed(requestFor(t, result, "codex:tool"))
	if policyErr != nil || !allowed {
		t.Fatalf("structured command should match: allowed=%v err=%v", allowed, policyErr)
	}

	result = discoverCodex(t, options, `
[mcp_servers.tool]
command = "npx"
args = ["serve", "workspace=/repo"]
`)
	allowed, policyErr = checker.NativeMCPAllowed(requestFor(t, result, "codex:tool"))
	if policyErr != nil || allowed {
		t.Fatalf("wrong ordered argument matched: allowed=%v err=%v", allowed, policyErr)
	}
}

func TestCodexEmptyAllowlistAndUnknownCloudFailClosed(t *testing.T) {
	options, paths := testPolicyOptions(t)
	writeTestFile(t, paths.CodexRequirements, "[mcp_servers]\n")
	checker, diagnostics, err := Load(options)
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("Load empty allowlist: diagnostics=%v err=%v", diagnostics, err)
	}
	result := discoverCodex(t, options, `[mcp_servers.anything]
command = "tool"
`)
	allowed, policyErr := checker.NativeMCPAllowed(requestFor(t, result, "codex:anything"))
	if policyErr != nil || allowed {
		t.Fatalf("empty allowlist: allowed=%v err=%v", allowed, policyErr)
	}

	options.CloudRequirementsChecked = false
	writeTestFile(t, paths.CodexAuth, `{"tokens":{"access_token":"never-render-this"}}`)
	checker, diagnostics, err = Load(options)
	if err != nil || len(diagnostics) == 0 {
		t.Fatalf("Load unknown cloud: diagnostics=%v err=%v", diagnostics, err)
	}
	allowed, policyErr = checker.NativeMCPAllowed(requestFor(t, result, "codex:anything"))
	if allowed || !errors.Is(policyErr, ErrCodexPolicyUnavailable) {
		t.Fatalf("unknown cloud did not fail closed: allowed=%v err=%v", allowed, policyErr)
	}
	if strings.Contains(strings.Join(diagnosticStrings(diagnostics), "\n"), "never-render-this") {
		t.Fatal("credential leaked through diagnostic")
	}
}

func TestClaudeManagedOnlyAndDenyPrecedence(t *testing.T) {
	options, paths := testPolicyOptions(t)
	writeTestFile(t, paths.ClaudeManagedSettings, `{
  "allowManagedMcpServersOnly": true,
  "allowedMcpServers": [{"serverCommand":["safe"]}]
}`)
	writeTestFile(t, paths.ClaudeUserSettings, `{
  "allowedMcpServers": [{"serverCommand":["unsafe"]}],
  "deniedMcpServers": [{"serverName":"safe-name"}]
}`)
	checker, diagnostics, err := Load(options)
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("Load: diagnostics=%v err=%v", diagnostics, err)
	}
	result := discoverClaude(t, options, `{
  "mcpServers": {
    "safe-name": {"command":"safe"},
    "unsafe-name": {"command":"unsafe"}
  }
}`)
	allowed, policyErr := checker.NativeMCPAllowed(requestFor(t, result, "claude:safe-name"))
	if policyErr != nil || allowed {
		t.Fatalf("deny did not override allow: allowed=%v err=%v", allowed, policyErr)
	}
	allowed, policyErr = checker.NativeMCPAllowed(requestFor(t, result, "claude:unsafe-name"))
	if policyErr != nil || allowed {
		t.Fatalf("user broadened managed-only allowlist: allowed=%v err=%v", allowed, policyErr)
	}
}

func TestClaudeTransportSpecificNameFallback(t *testing.T) {
	options, paths := testPolicyOptions(t)
	writeTestFile(t, paths.ClaudeManagedSettings, `{
  "allowedMcpServers": [
    {"serverCommand":["safe"]},
    {"serverName":"named"}
  ]
}`)
	checker, diagnostics, err := Load(options)
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("Load: diagnostics=%v err=%v", diagnostics, err)
	}
	result := discoverClaude(t, options, `{
  "mcpServers": {
    "named": {"command":"unsafe"},
    "remote": {"type":"http", "url":"https://example.test/mcp"}
  }
}`)
	allowed, policyErr := checker.NativeMCPAllowed(requestFor(t, result, "claude:named"))
	if policyErr != nil || allowed {
		t.Fatalf("stdio name bypassed command entry: allowed=%v err=%v", allowed, policyErr)
	}

	writeTestFile(t, filepath.Join(options.HomeDir, ".claude.json"), `{
  "mcpServers": {"named": {"type":"http", "url":"https://example.test/mcp"}}
}`)
	result = mcpnative.Discover(mcpnative.Options{HomeDir: options.HomeDir, Workspace: options.Workspace})
	allowed, policyErr = checker.NativeMCPAllowed(requestFor(t, result, "claude:named"))
	if policyErr != nil || !allowed {
		t.Fatalf("remote name fallback should apply without URL entries: allowed=%v err=%v", allowed, policyErr)
	}
}

func TestClaudePinnedEnvironmentExpansion(t *testing.T) {
	options, paths := testPolicyOptions(t)
	options.StartupEnv = []string{"BIN=/approved"}
	writeTestFile(t, paths.ClaudeManagedSettings, `{
  "allowedMcpServers": [{"serverCommand":["${BIN}/server"]}]
}`)
	writeTestFile(t, paths.ClaudeProjectSettings, `{"env":{"BIN":"/project-controlled"}}`)
	checker, diagnostics, err := Load(options)
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("Load: diagnostics=%v err=%v", diagnostics, err)
	}
	result := discoverClaude(t, options, `{
  "mcpServers":{"env-tool":{"command":"${BIN}/server"}}
}`)
	allowed, policyErr := checker.NativeMCPAllowed(requestFor(t, result, "claude:env-tool"))
	if policyErr != nil || allowed {
		t.Fatalf("project env changed pinned allow policy: allowed=%v err=%v", allowed, policyErr)
	}

	writeTestFile(t, paths.ClaudeManagedSettings, `{
  "env":{"BIN":"/managed"},
  "allowedMcpServers": [{"serverCommand":["${BIN}/server"]}]
}`)
	checker, diagnostics, err = Load(options)
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("Load managed env: diagnostics=%v err=%v", diagnostics, err)
	}
	allowed, policyErr = checker.NativeMCPAllowed(requestFor(t, result, "claude:env-tool"))
	if policyErr != nil || !allowed {
		t.Fatalf("managed env should pin both sides: allowed=%v err=%v", allowed, policyErr)
	}
}

func TestClaudeManagedEnvironmentMergesAcrossAdminSources(t *testing.T) {
	options, paths := testPolicyOptions(t)
	writeTestFile(t, paths.ClaudeManagedSettings, `{"env":{"BIN":"/file","FILE_ONLY":"one"}}`)
	remote := NewDocument("remote", []byte(`{
  "env":{"REMOTE_ONLY":"two"},
  "allowedMcpServers":[{"serverCommand":["${BIN}/server"]}]
}`))
	options.ClaudeRemoteSettings = &remote
	checker, diagnostics, err := Load(options)
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("Load: diagnostics=%v err=%v", diagnostics, err)
	}
	result := discoverClaude(t, options, `{
  "mcpServers":{"env-tool":{"command":"${BIN}/server"}}
}`)
	allowed, policyErr := checker.NativeMCPAllowed(requestFor(t, result, "claude:env-tool"))
	if policyErr != nil || !allowed {
		t.Fatalf("lower managed env did not fill remote source: allowed=%v err=%v", allowed, policyErr)
	}
}

func TestClaudeRuntimeExpansionUsesAuthorizedSnapshotForDirectAndPlugin(t *testing.T) {
	options, paths := testPolicyOptions(t)
	options.StartupEnv = []string{"BIN=/startup"}
	writeTestFile(t, paths.ClaudeManagedSettings, `{
  "allowedMcpServers":[{"serverCommand":["/approved/server"]}]
}`)
	writeTestFile(t, paths.ClaudeProjectSettings, `{"env":{"BIN":"/approved"}}`)
	checker, diagnostics, err := Load(options)
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("Load: diagnostics=%v err=%v", diagnostics, err)
	}

	// Mutating the process after Load reproduces the former policy/runtime
	// split: policy saw /approved while a live os.LookupEnv would see /evil.
	t.Setenv("BIN", "/evil")
	if live, _ := os.LookupEnv("BIN"); live != "/evil" {
		t.Fatalf("live environment = %q", live)
	}

	direct := discoverClaude(t, options, `{
  "mcpServers":{"worker":{"command":"${BIN}/server"}}
}`)
	allowed, policyErr := checker.NativeMCPAllowed(requestFor(t, direct, "claude:worker"))
	if policyErr != nil || !allowed {
		t.Fatalf("direct server policy decision: allowed=%v err=%v", allowed, policyErr)
	}

	pluginRoot := filepath.Join(options.Workspace, "expansion-plugin")
	writeTestFile(t, filepath.Join(pluginRoot, ".mcp.json"), `{"worker":{"command":"${BIN}/server"}}`)
	plugin := mcpnative.ParsePluginMCP(mcpnative.PluginMCPOptions{
		Dialect: mcpnative.DialectClaude, PluginID: "expansion", PluginRoot: pluginRoot,
		Path: filepath.Join(pluginRoot, ".mcp.json"), Shape: mcpnative.PluginMCPDirect,
	})
	allowed, policyErr = checker.NativeMCPAllowed(requestFor(t, plugin, "claude:plugin:expansion:worker"))
	if policyErr != nil || !allowed {
		t.Fatalf("plugin server policy decision: allowed=%v err=%v", allowed, policyErr)
	}

	expansion, err := checker.ClaudeRuntimeExpansion()
	if err != nil {
		t.Fatal(err)
	}
	got, err := expansion.Expand("${BIN}/server")
	if err != nil || got != "/approved/server" {
		t.Fatalf("authorized runtime expansion = %q, %v", got, err)
	}
}

func TestClaudeRuntimeExpansionIsStrictAndRedacted(t *testing.T) {
	options, _ := testPolicyOptions(t)
	secret := "runtime-expansion-secret"
	options.StartupEnv = []string{"TOKEN=" + secret, "EMPTY="}
	checker, diagnostics, err := Load(options)
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("Load: diagnostics=%v err=%v", diagnostics, err)
	}
	expansion, err := checker.ClaudeRuntimeExpansion()
	if err != nil {
		t.Fatal(err)
	}
	if _, expandErr := (RuntimeExpansion{}).Expand("literal"); !errors.Is(expandErr, ErrClaudeRuntimeExpansion) {
		t.Fatalf("zero expansion capability was usable: %v", expandErr)
	}
	if got, expandErr := expansion.Expand("prefix-${TOKEN}"); expandErr != nil || got != "prefix-"+secret {
		t.Fatalf("secret expansion = %q, %v", got, expandErr)
	}
	if got, expandErr := expansion.Expand("${EMPTY:-fallback}"); expandErr != nil || got != "fallback" {
		t.Fatalf("empty fallback expansion = %q, %v", got, expandErr)
	}
	for _, invalid := range []string{"${MISSING}", "${1INVALID}", "${OPEN"} {
		if _, expandErr := expansion.Expand(invalid); !errors.Is(expandErr, ErrClaudeRuntimeExpansion) {
			t.Fatalf("invalid expansion %q: %v", invalid, expandErr)
		}
	}
	encoded, err := json.Marshal(expansion)
	if err != nil {
		t.Fatal(err)
	}
	for _, rendered := range []string{fmt.Sprint(expansion), fmt.Sprintf("%#v", expansion), string(encoded)} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("runtime environment rendered: %q", rendered)
		}
	}
}

func TestClaudeManagedMCPExcludesPluginServers(t *testing.T) {
	options, paths := testPolicyOptions(t)
	writeTestFile(t, paths.ClaudeManagedMCP, `{"mcpServers":{}}`)
	checker, diagnostics, err := Load(options)
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("Load: diagnostics=%v err=%v", diagnostics, err)
	}
	if !checker.ClaudeManagedExclusive() {
		t.Fatal("managed-mcp.json presence was not exposed")
	}
	pluginRoot := filepath.Join(options.Workspace, "plugin")
	writeTestFile(t, filepath.Join(pluginRoot, ".mcp.json"), `{"tool":{"command":"safe"}}`)
	result := mcpnative.ParsePluginMCP(mcpnative.PluginMCPOptions{
		Dialect: mcpnative.DialectClaude, PluginID: "example", PluginRoot: pluginRoot,
		Path: filepath.Join(pluginRoot, ".mcp.json"), Shape: mcpnative.PluginMCPDirect,
	})
	allowed, policyErr := checker.NativeMCPAllowed(requestFor(t, result, "claude:plugin:example:tool"))
	if policyErr != nil || allowed {
		t.Fatalf("managed MCP did not suppress plugin server: allowed=%v err=%v", allowed, policyErr)
	}
}

func TestClaudeProjectStateDeniesPluginAndProjectByScope(t *testing.T) {
	options, paths := testPolicyOptions(t)
	state, err := json.Marshal(map[string]any{
		"projects": map[string]any{
			options.Workspace: map[string]any{
				"disabledMcpServers":     []string{"plugin-off"},
				"disabledMcpjsonServers": []string{"project-off"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, paths.ClaudeState, string(state))
	checker, diagnostics, err := Load(options)
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("Load: diagnostics=%v err=%v", diagnostics, err)
	}

	pluginRoot := filepath.Join(options.Workspace, "state-plugin")
	writeTestFile(t, filepath.Join(pluginRoot, ".mcp.json"), `{
  "plugin-off":{"command":"safe"},
  "project-off":{"command":"safe"}
}`)
	result := mcpnative.ParsePluginMCP(mcpnative.PluginMCPOptions{
		Dialect: mcpnative.DialectClaude, PluginID: "state", PluginRoot: pluginRoot,
		Path: filepath.Join(pluginRoot, ".mcp.json"), Shape: mcpnative.PluginMCPDirect,
	})
	allowed, policyErr := checker.NativeMCPAllowed(requestFor(t, result, "claude:plugin:state:plugin-off"))
	if policyErr != nil || allowed {
		t.Fatalf("disabledMcpServers did not deny plugin MCP: allowed=%v err=%v", allowed, policyErr)
	}
	allowed, policyErr = checker.NativeMCPAllowed(requestFor(t, result, "claude:plugin:state:project-off"))
	if policyErr != nil || !allowed {
		t.Fatalf("project-only deny incorrectly denied plugin MCP: allowed=%v err=%v", allowed, policyErr)
	}
	allowed, policyErr = checker.NativeMCPAllowed(mcpnative.PolicyRequest{
		Name: "project-off", Dialect: mcpnative.DialectClaude,
		Scope: mcpnative.ScopeProject, Source: mcpnative.SourceClaudeProject,
		Transport: mcpnative.TransportStdio,
	})
	if policyErr != nil || allowed {
		t.Fatalf("disabledMcpjsonServers did not deny project MCP: allowed=%v err=%v", allowed, policyErr)
	}
}

func TestMalformedClaudeProjectStateQuarantinesPluginPolicy(t *testing.T) {
	options, paths := testPolicyOptions(t)
	writeTestFile(t, paths.ClaudeState, `{
  "projects": {"`+options.Workspace+`": {
    "disabledMcpServers": ["tool"],
    "disabledMcpServers": []
  }}
}`)
	checker, diagnostics, err := Load(options)
	if err != nil || len(diagnostics) == 0 {
		t.Fatalf("Load: diagnostics=%v err=%v", diagnostics, err)
	}
	allowed, policyErr := checker.NativeMCPAllowed(mcpnative.PolicyRequest{
		Name: "tool", Dialect: mcpnative.DialectClaude,
		Scope: mcpnative.ScopePlugin, Source: mcpnative.SourceClaudePlugin,
		Transport: mcpnative.TransportStdio,
	})
	if allowed || !errors.Is(policyErr, ErrClaudePolicyUnavailable) {
		t.Fatalf("malformed project state did not quarantine plugin policy: allowed=%v err=%v", allowed, policyErr)
	}
}

func TestCodexPluginRequirementsUsePluginAndServerIdentity(t *testing.T) {
	options, paths := testPolicyOptions(t)
	writeTestFile(t, paths.CodexRequirements, `
[mcp_servers]

[plugins.example.mcp_servers.tool]
identity = { command = "approved" }
`)
	checker, diagnostics, err := Load(options)
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("Load: diagnostics=%v err=%v", diagnostics, err)
	}
	pluginRoot := filepath.Join(options.Workspace, "codex-plugin")
	writeTestFile(t, filepath.Join(pluginRoot, "mcp.json"), `{"tool":{"command":"approved","args":["ignored-by-string-form"]}}`)
	result := mcpnative.ParsePluginMCP(mcpnative.PluginMCPOptions{
		Dialect: mcpnative.DialectCodex, PluginID: "example", PluginRoot: pluginRoot,
		Path: filepath.Join(pluginRoot, "mcp.json"), Shape: mcpnative.PluginMCPDirect,
	})
	allowed, policyErr := checker.NativeMCPAllowed(requestFor(t, result, "codex:plugin:example:tool"))
	if policyErr != nil || !allowed {
		t.Fatalf("approved plugin identity rejected: allowed=%v err=%v", allowed, policyErr)
	}

	other := mcpnative.ParsePluginMCP(mcpnative.PluginMCPOptions{
		Dialect: mcpnative.DialectCodex, PluginID: "other", PluginRoot: pluginRoot,
		Path: filepath.Join(pluginRoot, "mcp.json"), Shape: mcpnative.PluginMCPDirect,
	})
	allowed, policyErr = checker.NativeMCPAllowed(requestFor(t, other, "codex:plugin:other:tool"))
	if policyErr != nil || allowed {
		t.Fatalf("unlisted plugin admitted: allowed=%v err=%v", allowed, policyErr)
	}
}

func TestCodexUnrelatedPluginRequirementsDoNotRestrictMCP(t *testing.T) {
	options, paths := testPolicyOptions(t)
	writeTestFile(t, paths.CodexRequirements, `
[plugins.example]
enabled = false
`)
	checker, diagnostics, err := Load(options)
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("Load: diagnostics=%v err=%v", diagnostics, err)
	}
	if checker.CodexPluginMCPRestricted() {
		t.Fatal("unrelated plugin requirements enabled MCP filtering")
	}
	pluginRoot := filepath.Join(options.Workspace, "codex-plugin")
	writeTestFile(t, filepath.Join(pluginRoot, "mcp.json"), `{"tool":{"command":"local"}}`)
	result := mcpnative.ParsePluginMCP(mcpnative.PluginMCPOptions{
		Dialect: mcpnative.DialectCodex, PluginID: "example", PluginRoot: pluginRoot,
		Path: filepath.Join(pluginRoot, "mcp.json"), Shape: mcpnative.PluginMCPDirect,
	})
	allowed, policyErr := checker.NativeMCPAllowed(requestFor(t, result, "codex:plugin:example:tool"))
	if policyErr != nil || !allowed {
		t.Fatalf("unrelated plugin requirements filtered MCP: allowed=%v err=%v", allowed, policyErr)
	}
}

func TestCodexLegacyDescriptionDoesNotQuarantineIdentity(t *testing.T) {
	options, paths := testPolicyOptions(t)
	writeTestFile(t, paths.CodexRequirements, `
[mcp_servers.docs]
description = "documentation server"
identity = { command = "docs" }
`)
	checker, diagnostics, err := Load(options)
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("Load: diagnostics=%v err=%v", diagnostics, err)
	}
	result := discoverCodex(t, options, `[mcp_servers.docs]
command = "docs"
`)
	allowed, policyErr := checker.NativeMCPAllowed(requestFor(t, result, "codex:docs"))
	if policyErr != nil || !allowed {
		t.Fatalf("description quarantined valid identity: allowed=%v err=%v", allowed, policyErr)
	}
}

func TestCodexUncheckedCloudQuarantinesWithoutAuthFile(t *testing.T) {
	options, _ := testPolicyOptions(t)
	options.CloudRequirementsChecked = false
	checker, diagnostics, err := Load(options)
	if err != nil || len(diagnostics) == 0 {
		t.Fatalf("Load: diagnostics=%v err=%v", diagnostics, err)
	}
	result := discoverCodex(t, options, `[mcp_servers.x]
command = "x"
`)
	allowed, policyErr := checker.NativeMCPAllowed(requestFor(t, result, "codex:x"))
	if allowed || !errors.Is(policyErr, ErrCodexPolicyUnavailable) {
		t.Fatalf("unchecked cloud admitted local MCP: allowed=%v err=%v", allowed, policyErr)
	}
}

func TestMalformedDuplicateAndEscapingSettingsQuarantineClaude(t *testing.T) {
	t.Run("duplicate-key", func(t *testing.T) {
		options, paths := testPolicyOptions(t)
		writeTestFile(t, paths.ClaudeUserSettings, `{"allowedMcpServers":[],"allowedMcpServers":[{"serverName":"x"}]}`)
		checker, diagnostics, err := Load(options)
		if err != nil || len(diagnostics) == 0 {
			t.Fatalf("Load: diagnostics=%v err=%v", diagnostics, err)
		}
		result := discoverClaude(t, options, `{"mcpServers":{"x":{"command":"x"}}}`)
		allowed, policyErr := checker.NativeMCPAllowed(requestFor(t, result, "claude:x"))
		if allowed || !errors.Is(policyErr, ErrClaudePolicyUnavailable) {
			t.Fatalf("duplicate key did not quarantine: allowed=%v err=%v", allowed, policyErr)
		}
	})

	t.Run("project-symlink-escape", func(t *testing.T) {
		options, paths := testPolicyOptions(t)
		outside := filepath.Join(filepath.Dir(options.Workspace), "outside.json")
		writeTestFile(t, outside, `{"deniedMcpServers":[{"serverName":"x"}]}`)
		if err := os.MkdirAll(filepath.Dir(paths.ClaudeProjectSettings), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, paths.ClaudeProjectSettings); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		checker, diagnostics, err := Load(options)
		if err != nil || len(diagnostics) == 0 {
			t.Fatalf("Load: diagnostics=%v err=%v", diagnostics, err)
		}
		result := discoverClaude(t, options, `{"mcpServers":{"x":{"command":"x"}}}`)
		allowed, policyErr := checker.NativeMCPAllowed(requestFor(t, result, "claude:x"))
		if allowed || !errors.Is(policyErr, ErrClaudePolicyUnavailable) {
			t.Fatalf("escaping symlink did not quarantine: allowed=%v err=%v", allowed, policyErr)
		}
	})
}

func TestUnsupportedManagedSurfacesFailClosed(t *testing.T) {
	t.Run("remote-cache", func(t *testing.T) {
		options, paths := testPolicyOptions(t)
		writeTestFile(t, paths.ClaudeRemoteSettings, `{"allowedMcpServers":[]}`)
		checker, diagnostics, err := Load(options)
		if err != nil || len(diagnostics) == 0 {
			t.Fatalf("Load: diagnostics=%v err=%v", diagnostics, err)
		}
		result := discoverClaude(t, options, `{"mcpServers":{"x":{"command":"x"}}}`)
		allowed, policyErr := checker.NativeMCPAllowed(requestFor(t, result, "claude:x"))
		if allowed || !errors.Is(policyErr, ErrClaudePolicyUnavailable) {
			t.Fatalf("remote cache did not quarantine: allowed=%v err=%v", allowed, policyErr)
		}
	})

	t.Run("policy-helper", func(t *testing.T) {
		options, paths := testPolicyOptions(t)
		writeTestFile(t, paths.ClaudeManagedSettings, `{"policyHelper":{"path":"/managed/helper"}}`)
		checker, diagnostics, err := Load(options)
		if err != nil || len(diagnostics) == 0 {
			t.Fatalf("Load: diagnostics=%v err=%v", diagnostics, err)
		}
		result := discoverClaude(t, options, `{"mcpServers":{"x":{"command":"x"}}}`)
		allowed, policyErr := checker.NativeMCPAllowed(requestFor(t, result, "claude:x"))
		if allowed || !errors.Is(policyErr, ErrClaudePolicyUnavailable) {
			t.Fatalf("policy helper did not quarantine: allowed=%v err=%v", allowed, policyErr)
		}
	})

	t.Run("managed-preferences", func(t *testing.T) {
		options, paths := testPolicyOptions(t)
		paths.CodexMDM = []string{filepath.Join(filepath.Dir(paths.CodexRequirements), "com.openai.codex.plist")}
		paths.ClaudeMDM = []string{filepath.Join(filepath.Dir(paths.ClaudeManagedSettings), "com.anthropic.claudecode.plist")}
		*options.Paths = paths
		writeTestFile(t, paths.CodexMDM[0], "opaque")
		writeTestFile(t, paths.ClaudeMDM[0], "opaque")
		checker, diagnostics, err := Load(options)
		if err != nil || len(diagnostics) < 2 {
			t.Fatalf("Load: diagnostics=%v err=%v", diagnostics, err)
		}
		codexResult := discoverCodex(t, options, `[mcp_servers.x]
command = "x"
`)
		allowed, policyErr := checker.NativeMCPAllowed(requestFor(t, codexResult, "codex:x"))
		if allowed || !errors.Is(policyErr, ErrCodexPolicyUnavailable) {
			t.Fatalf("Codex MDM did not quarantine: allowed=%v err=%v", allowed, policyErr)
		}
		claudeResult := discoverClaude(t, options, `{"mcpServers":{"x":{"command":"x"}}}`)
		allowed, policyErr = checker.NativeMCPAllowed(requestFor(t, claudeResult, "claude:x"))
		if allowed || !errors.Is(policyErr, ErrClaudePolicyUnavailable) {
			t.Fatalf("Claude MDM did not quarantine: allowed=%v err=%v", allowed, policyErr)
		}
	})
}

func TestDuplicateTOMLAndOversizedPolicyFailClosed(t *testing.T) {
	t.Run("duplicate-toml", func(t *testing.T) {
		options, paths := testPolicyOptions(t)
		writeTestFile(t, paths.CodexRequirements, `[mcp_servers.x]
identity = { command = "one" }
identity = { command = "two" }
`)
		checker, diagnostics, err := Load(options)
		if err != nil || len(diagnostics) == 0 {
			t.Fatalf("Load: diagnostics=%v err=%v", diagnostics, err)
		}
		result := discoverCodex(t, options, `[mcp_servers.x]
command = "two"
`)
		allowed, policyErr := checker.NativeMCPAllowed(requestFor(t, result, "codex:x"))
		if allowed || !errors.Is(policyErr, ErrCodexPolicyUnavailable) {
			t.Fatalf("duplicate TOML did not quarantine: allowed=%v err=%v", allowed, policyErr)
		}
	})

	t.Run("oversized-json", func(t *testing.T) {
		options, paths := testPolicyOptions(t)
		writeTestFile(t, paths.ClaudeUserSettings, `{"padding":"`+strings.Repeat("x", int(MaxPolicyFileBytes))+`"}`)
		checker, diagnostics, err := Load(options)
		if err != nil || len(diagnostics) == 0 {
			t.Fatalf("Load: diagnostics=%v err=%v", diagnostics, err)
		}
		result := discoverClaude(t, options, `{"mcpServers":{"x":{"command":"x"}}}`)
		allowed, policyErr := checker.NativeMCPAllowed(requestFor(t, result, "claude:x"))
		if allowed || !errors.Is(policyErr, ErrClaudePolicyUnavailable) {
			t.Fatalf("oversized settings did not quarantine: allowed=%v err=%v", allowed, policyErr)
		}
	})
}

func TestRedactedRendering(t *testing.T) {
	secret := "policy-secret-never-render"
	document := NewDocument(secret, []byte(`{"env":{"TOKEN":"`+secret+`"}}`))
	options, _ := testPolicyOptions(t)
	options.ClaudeRemoteSettings = &document
	checker, diagnostics, err := Load(options)
	if err != nil {
		t.Fatal(err)
	}
	for _, rendered := range []string{document.String(), checker.String(), strings.Join(diagnosticStrings(diagnostics), "\n")} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("secret rendered: %q", rendered)
		}
	}
}

func diagnosticStrings(diagnostics []Diagnostic) []string {
	result := make([]string, len(diagnostics))
	for index, diagnostic := range diagnostics {
		result[index] = diagnostic.String()
	}
	return result
}
