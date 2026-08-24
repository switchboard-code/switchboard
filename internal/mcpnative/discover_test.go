package mcpnative

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverGoldenAndNativePrecedence(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "repo")
	mustMkdir(t, filepath.Join(home, ".codex"))
	mustMkdir(t, filepath.Join(workspace, ".codex"))

	mustWrite(t, filepath.Join(home, ".codex", "config.toml"), `
model = "unrelated-setting-is-ignored"

[mcp_servers.shared]
command = "user-codex"
args = ["--user"]
env = { USER_TOKEN = "codex-user-secret" }
enabled_tools = ["read"]
startup_timeout_sec = 5

[mcp_servers.remote]
url = "https://example.test/mcp?token=codex-url-secret"
http_headers = { X_API_Key = "codex-header-secret" }
env_http_headers = { Authorization = "REMOTE_AUTH" }
bearer_token_env_var = "BEARER_TOKEN"
required = true
tool_timeout_sec = 60
`)
	mustWrite(t, filepath.Join(workspace, ".codex", "config.toml"), `
[mcp_servers.shared]
command = "project-codex"
args = ["--project"]
cwd = "."
env = { PROJECT_TOKEN = "${NATIVE_SECRET}" }
disabled_tools = ["write"]
enabled = false
`)

	projectClaude := `{
  "mcpServers": {
    "claude-shared": {"type":"stdio","command":"project-claude","args":["--project"]},
    "shared": {"type":"http","url":"https://claude.example/mcp","headers":{"Authorization":"Bearer ${CLAUDE_TOKEN}"}},
    "unsupported": {"command":"never-run","oauth":{"clientId":"secret-client-id"}}
  }
}`
	mustWrite(t, filepath.Join(workspace, ".mcp.json"), projectClaude)

	claudeHome := map[string]any{
		"mcpServers": map[string]any{
			"claude-shared": map[string]any{"command": "user-claude", "args": []string{"--user"}},
			"user-only": map[string]any{
				"command": "user-only", "env": map[string]string{"TOKEN": "claude-user-secret"},
			},
		},
		"projects": map[string]any{
			workspace: map[string]any{
				"hasTrustDialogAccepted": true,
				"enabledMcpjsonServers":  []string{"claude-shared"},
				"mcpServers": map[string]any{
					"claude-shared": map[string]any{
						"type": "stdio", "command": "local-claude", "args": []string{"--local"},
						"env": map[string]string{"TOKEN": "claude-local-secret"},
					},
				},
			},
		},
	}
	encoded, err := json.MarshalIndent(claudeHome, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(home, ".claude.json"), string(encoded))

	t.Setenv("NATIVE_SECRET", "expanded-secret-must-not-appear")
	result := Discover(Options{HomeDir: home, Workspace: workspace})

	shared := serverNamed(t, result, "codex:shared")
	if expose(shared.Command) != "project-codex" || len(shared.Args) != 1 || shared.Args[0].raw() != "--project" {
		t.Fatalf("Codex project fields did not override user fields: %+v", shared)
	}
	if shared.Env["USER_TOKEN"].raw() != "codex-user-secret" || shared.Env["PROJECT_TOKEN"].raw() != "${NATIVE_SECRET}" {
		t.Fatal("Codex recursively merged environment table was not preserved")
	}
	if shared.Enabled || !shared.EnabledSet || !shared.Timeouts.StartupSet ||
		!shared.Tools.EnabledSet || !shared.Tools.DisabledSet {
		t.Fatalf("Codex restrictive and inherited fields were lost during recursive merge: %+v", shared)
	}
	if len(shared.Provenance.ContributingLayers) != 2 ||
		shared.Provenance.ContributingLayers[0].Scope != ScopeUser ||
		shared.Provenance.ContributingLayers[1].Scope != ScopeProject {
		t.Fatalf("Codex contributing-layer provenance = %+v", shared.Provenance.ContributingLayers)
	}
	if !shared.ExecutionTrustRequired || shared.TrustRoot == "" {
		t.Fatal("project Codex entry did not retain its Switchboard trust gate")
	}
	if got := shared.Env["PROJECT_TOKEN"].raw(); got != "${NATIVE_SECRET}" {
		t.Fatalf("environment reference was expanded during discovery: %q", got)
	}

	claudeShared := serverNamed(t, result, "claude:claude-shared")
	if expose(claudeShared.Command) != "local-claude" || claudeShared.Provenance.Scope != ScopeLocal {
		t.Fatalf("Claude local entry did not replace project and user entries: %+v", claudeShared)
	}
	if !claudeShared.ExecutionTrustRequired {
		t.Fatal("project-specific ~/.claude.json entry bypassed Switchboard trust")
	}

	unsupported := serverNamed(t, result, "claude:unsupported")
	if unsupported.Supported || unsupported.ClaudeOAuth == nil {
		t.Fatalf("OAuth on an incompatible transport did not fail closed: %+v", unsupported)
	}

	got, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"codex-user-secret", "codex-url-secret", "codex-header-secret",
		"claude-user-secret", "claude-local-secret", "secret-client-id",
		"expanded-secret-must-not-appear",
	} {
		if strings.Contains(string(got), secret) {
			t.Fatalf("serialized discovery result exposed %q", secret)
		}
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	realWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	normalizeDiscoveryGolden(&result,
		goldenPathReplacement{from: realWorkspace, to: "$WORKSPACE"},
		goldenPathReplacement{from: workspace, to: "$WORKSPACE"},
		goldenPathReplacement{from: realRoot, to: "$ROOT"},
		goldenPathReplacement{from: root, to: "$ROOT"},
	)
	got, err = json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	normalized := string(got)
	want, err := os.ReadFile(filepath.Join("testdata", "discovery.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	wantText := strings.ReplaceAll(string(want), "\r\n", "\n")
	if normalized != strings.TrimSpace(wantText) {
		t.Fatalf("discovery golden mismatch\n--- got ---\n%s\n--- want ---\n%s", normalized, want)
	}
}

type goldenPathReplacement struct {
	from string
	to   string
}

func normalizeDiscoveryGolden(result *Result, replacements ...goldenPathReplacement) {
	normalize := func(value string) string {
		original := value
		for _, replacement := range replacements {
			if replacement.from == "" {
				continue
			}
			clean := filepath.Clean(replacement.from)
			value = strings.ReplaceAll(value, clean, replacement.to)
			value = strings.ReplaceAll(value, filepath.ToSlash(clean), replacement.to)
		}
		if value != original || strings.Contains(value, "$ROOT") || strings.Contains(value, "$WORKSPACE") {
			return strings.ReplaceAll(value, `\`, "/")
		}
		return value
	}
	for i := range result.Servers {
		server := &result.Servers[i]
		server.Provenance.Path = normalize(server.Provenance.Path)
		server.Provenance.RealPath = normalize(server.Provenance.RealPath)
		server.Provenance.ConfigKey = normalize(server.Provenance.ConfigKey)
		server.Provenance.PluginRoot = normalize(server.Provenance.PluginRoot)
		for j := range server.Provenance.ContributingLayers {
			layer := &server.Provenance.ContributingLayers[j]
			layer.Path = normalize(layer.Path)
			layer.RealPath = normalize(layer.RealPath)
		}
		server.TrustRoot = normalize(server.TrustRoot)
	}
	for i := range result.Diagnostics {
		result.Diagnostics[i].Path = normalize(result.Diagnostics[i].Path)
		result.Diagnostics[i].Message = normalize(result.Diagnostics[i].Message)
	}
	for i := range result.Quarantines {
		result.Quarantines[i].Path = normalize(result.Quarantines[i].Path)
	}
}

func quotedJSONString(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func TestSensitiveValueHasNoRenderingThatShowsItsValue(t *testing.T) {
	const secret = "secret-that-must-never-render"
	value := sensitive("prefix-${TOKEN:-" + secret + "}")
	renderings := []string{
		fmt.Sprint(value),
		fmt.Sprintf("%v", value),
		fmt.Sprintf("%+v", value),
		fmt.Sprintf("%#v", value),
		fmt.Sprint(&value),
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	renderings = append(renderings, string(encoded))
	textMarshaler, ok := any(value).(encoding.TextMarshaler)
	if !ok {
		t.Fatal("SensitiveValue lost its text redaction boundary")
	}
	text, err := textMarshaler.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	renderings = append(renderings, string(text))
	for _, rendering := range renderings {
		if strings.Contains(rendering, secret) {
			t.Fatalf("secret rendered through %q", rendering)
		}
	}
	commandSecret := sensitive(secret)
	server := Server{Command: &commandSecret, Args: []SensitiveValue{sensitive("--token=" + secret)}}
	serverJSON, err := json.Marshal(server)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprintf("%+v", server), secret) || strings.Contains(string(serverJSON), secret) {
		t.Fatal("command or argument value escaped its redaction boundary")
	}
	if got := value.EnvReferences(); len(got) != 1 || got[0] != "TOKEN" {
		t.Fatalf("environment references = %v, want [TOKEN]", got)
	}
}

func TestCodexForwardsEnvAndPreservesApprovalModes(t *testing.T) {
	base := Provenance{Dialect: DialectCodex, Scope: ScopeUser, Path: "config.toml", RealPath: "config.toml"}
	servers, diagnostics := parseCodex([]byte(`
[mcp_servers.native]
command = "server"
env_vars = ["LOCAL_TOKEN", { name = "SECOND_TOKEN", source = "local" }]
default_tools_approval_mode = "writes"

[mcp_servers.native.tools.read]
approval_mode = "approve"

[mcp_servers.native.tools.delete]
approval_mode = "prompt"
`), base, "")
	if len(diagnostics) != 0 || len(servers) != 1 {
		t.Fatalf("modern Codex MCP fields did not parse: servers=%+v diagnostics=%+v", servers, diagnostics)
	}
	server := servers[0]
	if !server.Supported || len(server.ForwardedEnv) != 2 {
		t.Fatalf("forwarded environment was not preserved: %+v", server)
	}
	if server.ForwardedEnv[0] != (EnvVar{Name: "LOCAL_TOKEN", Source: EnvSourceLocal}) ||
		server.ForwardedEnv[1] != (EnvVar{Name: "SECOND_TOKEN", Source: EnvSourceLocal}) {
		t.Fatalf("forwarded environment = %+v", server.ForwardedEnv)
	}
	if server.Approvals.Default != ApprovalWrites || !server.Approvals.DefaultSet ||
		server.Approvals.Tools["read"] != ApprovalApprove || server.Approvals.Tools["delete"] != ApprovalPrompt {
		t.Fatalf("approval modes were not preserved: %+v", server.Approvals)
	}
}

func TestCommandArgumentsPreserveOrderAndDuplicates(t *testing.T) {
	base := Provenance{Dialect: DialectCodex, Scope: ScopeUser, Path: "config.toml", RealPath: "config.toml"}
	servers, diagnostics := parseCodex([]byte(`
[mcp_servers.argv]
command = "server"
args = ["z", "a", "z"]
`), base, "")
	if len(diagnostics) != 0 || len(servers) != 1 {
		t.Fatalf("parse failed: servers=%+v diagnostics=%+v", servers, diagnostics)
	}
	args := servers[0].Args
	if len(args) != 3 || args[0].raw() != "z" || args[1].raw() != "a" || args[2].raw() != "z" {
		t.Fatalf("argv order or multiplicity changed: %+v", args)
	}
}

func TestCodexRemoteExecutionAndExplicitAuthRequireRuntimeFeatures(t *testing.T) {
	base := Provenance{Dialect: DialectCodex, Scope: ScopeUser, Path: "config.toml", RealPath: "config.toml"}
	servers, diagnostics := parseCodex([]byte(`
[mcp_servers.remote_stdio]
command = "server"
environment_id = "remote-one"
env_vars = [{ name = "TOKEN", source = "remote" }]

[mcp_servers.oauth]
url = "https://example.test/mcp"
auth = "oauth"
`), base, "")
	if len(servers) != 2 {
		t.Fatalf("servers = %d, want 2 (diagnostics=%+v)", len(servers), diagnostics)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("valid native semantics produced parse diagnostics: %+v", diagnostics)
	}
	for _, server := range servers {
		if !server.Supported {
			t.Fatalf("recognized native semantic was not preserved: %+v", server)
		}
		if len(server.RequiredFeatures()) == 0 {
			t.Fatalf("server lost required runtime features: %+v", server)
		}
	}
}

func TestUnknownFieldFailsClosedWithoutRenderingItsValue(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	mustMkdir(t, home)
	const secret = "unknown-field-secret"
	mustWrite(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {
    "unsafe": {"command":"never-run","futureAuth":"`+secret+`"}
  }
}`)
	result := Discover(Options{HomeDir: home})
	server := serverNamed(t, result, "claude:unsafe")
	if server.Supported {
		t.Fatal("server with unknown field remained supported")
	}
	rendered := fmt.Sprintf("%+v", result)
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered, secret) || strings.Contains(string(encoded), secret) {
		t.Fatal("unknown field value escaped through a result or diagnostic")
	}
}

func TestProjectConfigNeverImportsForeignTrust(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "repo")
	mustMkdir(t, home)
	mustMkdir(t, workspace)
	state := fmt.Sprintf(`{
  "projects": {
    %q: {
      "hasTrustDialogAccepted": true,
      "mcpServers": {"local":{"command":"never-run"}}
    }
  }
}`, workspace)
	mustWrite(t, filepath.Join(home, ".claude.json"), state)
	result := Discover(Options{HomeDir: home, Workspace: workspace})
	server := serverNamed(t, result, "claude:local")
	if !server.ExecutionTrustRequired || server.TrustRoot == "" {
		t.Fatalf("foreign trust state was imported: %+v", server)
	}
	if !hasDiagnostic(result, "foreign-trust-state-ignored") {
		t.Fatal("ignored foreign trust state was not surfaced")
	}
}

func TestDuplicateJSONKeysFailClosed(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	mustMkdir(t, home)
	mustWrite(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {"duplicate":{"command":"one","command":"two"}}
}`)
	result := Discover(Options{HomeDir: home})
	if len(result.Servers) != 0 || !hasDiagnostic(result, "invalid-json") {
		t.Fatalf("duplicate JSON key did not reject the file: %+v", result)
	}
}

func TestReadsAreBounded(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	mustMkdir(t, home)
	path := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(path, make([]byte, MaxConfigBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	result := Discover(Options{HomeDir: home})
	if !hasDiagnostic(result, "config-too-large") {
		t.Fatalf("oversized config was not rejected: %+v", result)
	}
}

func TestAggregateNestedReadsAreBoundedAndQuarantineLowerWinner(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "repo")
	mustMkdir(t, home)
	mustMkdir(t, workspace)
	current := workspace
	padding := strings.Repeat("# pad\n", 150_000)
	for index := 0; index < 5; index++ {
		if index > 0 {
			current = filepath.Join(current, fmt.Sprintf("level-%d", index))
		}
		mustMkdir(t, filepath.Join(current, ".codex"))
		mustWrite(t, filepath.Join(current, ".codex", "config.toml"), fmt.Sprintf(`
[mcp_servers.bounded]
command = "server-%d"
`, index)+padding)
	}
	result := Discover(Options{HomeDir: home, Workspace: workspace, CurrentDir: current})
	if !hasDiagnostic(result, "discovery-budget-exceeded") {
		t.Fatalf("aggregate byte budget was not enforced: %+v", result.Diagnostics)
	}
	var quarantine *DiscoveryQuarantinedError
	if _, err := result.ActivationRequest("codex:bounded"); !errors.As(err, &quarantine) {
		t.Fatalf("lower nested winner survived unread higher layer: %v", err)
	}
}

func TestCallerResolvedNativeConfigLocations(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	codexDir := filepath.Join(root, "custom-codex")
	claudeState := filepath.Join(root, "custom-claude", "state.json")
	mustMkdir(t, home)
	mustMkdir(t, codexDir)
	mustMkdir(t, filepath.Dir(claudeState))
	mustWrite(t, filepath.Join(codexDir, "config.toml"), "[mcp_servers.custom]\ncommand='codex-custom'\n")
	mustWrite(t, claudeState, `{"mcpServers":{"custom":{"command":"claude-custom"}}}`)
	result := Discover(Options{
		HomeDir: home, CodexConfigDir: codexDir, ClaudeStatePath: claudeState,
	})
	if expose(serverNamed(t, result, "codex:custom").Command) != "codex-custom" ||
		expose(serverNamed(t, result, "claude:custom").Command) != "claude-custom" {
		t.Fatalf("caller-resolved native config locations were ignored: %+v", result.Servers)
	}
}

func TestProjectConfigSymlinkCannotEscapeWorkspace(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "repo")
	mustMkdir(t, home)
	mustMkdir(t, workspace)
	external := filepath.Join(root, "external.json")
	mustWrite(t, external, `{"mcpServers":{"escape":{"command":"never-run"}}}`)
	if err := os.Symlink(external, filepath.Join(workspace, ".mcp.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	result := Discover(Options{HomeDir: home, Workspace: workspace})
	if !hasDiagnostic(result, "config-escapes-workspace") || len(result.Servers) != 0 {
		t.Fatalf("escaping project config was not refused: %+v", result)
	}
}

func serverNamed(t *testing.T, result Result, id string) Server {
	t.Helper()
	for _, server := range result.Servers {
		if server.ID == id {
			return server
		}
	}
	t.Fatalf("server %q not found in %+v", id, result.Servers)
	return Server{}
}

func expose(value *SensitiveValue) string {
	if value == nil {
		return ""
	}
	return value.raw()
}

func hasDiagnostic(result Result, code string) bool {
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func FuzzParseCodex(f *testing.F) {
	f.Add([]byte(`[mcp_servers.example]
command = "example"
`))
	f.Fuzz(func(t *testing.T, data []byte) {
		parseCodex(data, Provenance{Dialect: DialectCodex, Scope: ScopeUser, Path: "fuzz.toml"}, "")
	})
}

func FuzzParseClaude(f *testing.F) {
	f.Add([]byte(`{"mcpServers":{"example":{"command":"example"}}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		parseClaudeServers(data, Provenance{Dialect: DialectClaude, Scope: ScopeUser, Path: "fuzz.json"}, "", false)
	})
}
