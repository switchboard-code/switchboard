package mcpnative

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestConfigStructureAndEntryBudgetsFailClosed(t *testing.T) {
	base := Provenance{Dialect: DialectClaude, Scope: ScopeUser, Path: "state.json", RealPath: "state.json"}
	args := make([]string, MaxEntryValues+1)
	for index := range args {
		args[index] = "x"
	}
	encoded, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"large": map[string]any{
		"command": "server", "args": args,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	servers, diagnostics := parseClaudeServers(encoded, base, "", false)
	if len(servers) != 1 || servers[0].Supported || len(diagnostics) == 0 {
		t.Fatalf("oversized entry did not fail closed: servers=%+v diagnostics=%+v", servers, diagnostics)
	}

	deep := strings.Repeat("[", MaxConfigDepth+2) + "0" + strings.Repeat("]", MaxConfigDepth+2)
	_, diagnostics = parseClaudeServers([]byte(`{"mcpServers":{"deep":{"command":"server","future":`+deep+`}}}`), base, "", false)
	if len(diagnostics) == 0 || diagnostics[0].Code != "invalid-json" {
		t.Fatalf("deep JSON did not hit the structural bound: %+v", diagnostics)
	}
}

func TestCodexCurrentFieldsPresenceAndEffectiveTimeout(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	mustMkdir(t, filepath.Join(home, ".codex"))
	mustWrite(t, filepath.Join(home, ".codex", "config.toml"), `
[mcp_servers.modern]
name = "Modern"
url = "https://example.test/mcp"
environment_id = "local"
http_headers_helper = "headers-command"
auth = "oauth"
scopes = []
oauth = { client_id = "client-secret-looking-id", callback_port = 0 }
startup_timeout_ms = 0
startup_timeout_sec = 0.5
tool_timeout_sec = 0
enabled_tools = []
disabled_tools = []
omit_tools_from = ["direct", "code_mode"]
supports_parallel_tool_calls = true
`)
	snapshot := mustUserCodexSnapshot(t, filepath.Join(home, ".codex", "config.toml"), home)
	result := Discover(Options{HomeDir: home, CodexSnapshot: snapshot})
	server := serverNamed(t, result, "codex:modern")
	if !server.Supported || !server.Tools.EnabledSet || !server.Tools.DisabledSet ||
		!server.OAuthScopesSet || !server.Timeouts.StartupMillisSet || !server.Timeouts.StartupSet ||
		!server.Timeouts.ToolSet || server.EnvironmentID != "local" || !server.EnvironmentIDSet {
		t.Fatalf("current Codex fields lost presence or validity: %+v", server)
	}
	features := server.RequiredFeatures()
	if !slices.Contains(features, FeatureImmediateTimeout) {
		t.Fatalf("explicit zero tool timeout was not separated from the runtime's unset-zero semantics: %v", features)
	}
	for _, feature := range []Feature{
		FeatureCodexHeadersHelper, FeatureCodexOAuth, FeatureStartupTimeout,
		FeatureToolTimeout, FeatureToolFilters, FeatureToolExposure, FeatureParallelTools,
	} {
		if !slices.Contains(features, feature) {
			t.Fatalf("required features %v omit %s", features, feature)
		}
	}
	if slices.Contains(features, FeatureRemoteExecution) {
		t.Fatalf("local environment spuriously requires remote execution: %v", features)
	}
	activation := approve(t, result, server.ID)
	if _, err := result.Materialize(server.ID, nil, fixedPolicy{allowed: true}, activation); err == nil {
		t.Fatalf("server materialized without non-baseline features: %v", err)
	} else {
		var compatibility *CompatibilityError
		if !errors.As(err, &compatibility) {
			t.Fatalf("compatibility error = %T %v", err, err)
		}
	}
	materialized, err := result.Materialize(server.ID, nil, fixedPolicy{allowed: true}, activation, features...)
	if err != nil {
		t.Fatal(err)
	}
	if !materialized.Timeouts.StartupSet || materialized.Timeouts.StartupSeconds != 0.5 || materialized.Timeouts.StartupMillisSet {
		t.Fatalf("seconds did not win over milliseconds: %+v", materialized.Timeouts)
	}
}

func TestCodexCWDIsResolvedAgainstItsDeclaringLayerBeforeMerge(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "repo")
	mustMkdir(t, filepath.Join(home, ".codex"))
	mustMkdir(t, filepath.Join(workspace, ".codex"))
	mustWrite(t, filepath.Join(home, ".codex", "config.toml"), `
[mcp_servers.same]
command = "node"
cwd = "./mcp"
enabled = false
`)
	mustWrite(t, filepath.Join(workspace, ".codex", "config.toml"), `
[mcp_servers.same]
args = ["server.js"]
`)
	result := Discover(Options{HomeDir: home, Workspace: workspace})
	server := serverNamed(t, result, "codex:same")
	wantDir, err := filepath.EvalSymlinks(filepath.Join(home, ".codex"))
	if err != nil {
		t.Fatal(err)
	}
	if server.CWD == nil || server.CWD.raw() != filepath.Join(wantDir, "mcp") || server.Enabled {
		t.Fatalf("layer-relative cwd or inherited disable was reinterpreted: %+v", server)
	}
}

func TestTransportPresenceAndRemoteHelperFailClosed(t *testing.T) {
	base := Provenance{Dialect: DialectCodex, Scope: ScopeUser, Path: "config.toml", RealPath: "config.toml"}
	servers, _ := parseCodex([]byte(`
[mcp_servers.http_args]
url = "https://example.test/mcp"
args = []

[mcp_servers.stdio_headers]
command = "server"
http_headers = {}

[mcp_servers.remote_helper]
url = "https://example.test/mcp"
environment_id = "remote-one"
http_headers_helper = "helper"
`), base, "")
	if len(servers) != 3 {
		t.Fatalf("servers = %d", len(servers))
	}
	for _, server := range servers {
		if server.Supported {
			t.Fatalf("presence/remote-only conflict remained supported: %+v", server)
		}
	}
}

func TestTimeoutAndRemoteEnvironmentBoundsFailClosed(t *testing.T) {
	base := Provenance{Dialect: DialectCodex, Scope: ScopeUser, Path: "config.toml", RealPath: "config.toml"}
	servers, _ := parseCodex([]byte(`
[mcp_servers.huge]
command = "server"
startup_timeout_sec = 1e300

[mcp_servers.local_remote_env]
command = "server"
env_vars = [{ name = "TOKEN", source = "remote" }]

[mcp_servers.remote]
command = "server"
environment_id = "remote-one"
env_vars = [{ name = "TOKEN", source = "remote" }]
`), base, "")
	if len(servers) != 3 || servers[0].Supported || servers[1].Supported || !servers[2].Supported {
		t.Fatalf("duration/remote-source validation = %+v", servers)
	}

	claudeBase := Provenance{Dialect: DialectClaude, Scope: ScopeUser, Path: "state.json", RealPath: "state.json"}
	claude, _ := parseClaudeServers([]byte(`{"mcpServers":{
  "zero":{"command":"server","timeout":0},
  "fraction":{"command":"server","timeout":1.5},
  "empty-scope":{"type":"http","url":"https://example.test/mcp","oauth":{"scopes":""}}
}}`), claudeBase, "", false)
	if len(claude) != 3 {
		t.Fatalf("Claude servers = %d", len(claude))
	}
	for _, server := range claude {
		if server.Supported {
			t.Fatalf("invalid Claude timeout/OAuth scope remained supported: %+v", server)
		}
	}
}

func TestCodexChatGPTAuthRequiresOAuthFallbackSupport(t *testing.T) {
	base := Provenance{Dialect: DialectCodex, Scope: ScopeUser, Path: "config.toml", RealPath: "config.toml"}
	servers, diagnostics := parseCodex([]byte(`
[mcp_servers.chatgpt]
url = "https://example.test/mcp"
auth = "chatgpt"
`), base, "")
	if len(diagnostics) != 0 || len(servers) != 1 || !servers[0].Supported {
		t.Fatalf("ChatGPT auth did not parse: %+v %+v", servers, diagnostics)
	}
	features := servers[0].RequiredFeatures()
	if !slices.Contains(features, FeatureCodexChatGPT) || !slices.Contains(features, FeatureCodexOAuth) {
		t.Fatalf("ChatGPT fallback chain is not fully gated: %v", features)
	}
}

func TestClaudeCurrentFieldsURLsAndReservedNames(t *testing.T) {
	base := Provenance{Dialect: DialectClaude, Scope: ScopeUser, Path: "state.json", RealPath: "state.json"}
	servers, diagnostics := parseClaudeServers([]byte(`{
  "mcpServers": {
    "valid": {
      "type":"http",
      "url":"https://${HOST}/mcp",
      "headers":{"Authorization":"Bearer ${TOKEN}"},
      "timeout":1,
      "alwaysLoad":true,
      "headersHelper":"${HELPER}",
      "oauth":{"clientId":"${CLIENT}","callbackPort":3210,"authServerMetadataUrl":"https://auth.example/.well-known/oauth","scopes":"openid"}
    },
    "placeholder": {"type":"http","url":""},
    "upper": {"type":"HTTP","url":"https://example.test/mcp"},
    "missing-type": {"url":"https://example.test/mcp"},
    "bad-scheme": {"type":"http","url":"ftp://example.test/${TOKEN}"},
    "bad-metadata": {"type":"http","url":"https://example.test/mcp","oauth":{"authServerMetadataUrl":"http://auth.example/${TOKEN}"}},
    "workspace": {"command":"reserved"},
    "__proto__": {"command":"reserved"},
    "Claude?Preview": {"command":"reserved"},
    "Slack sign-in (Claude Code tag)": {"command":"reserved"}
  }
}`), base, "", false)
	if len(diagnostics) == 0 || len(servers) != 10 {
		t.Fatalf("servers=%d diagnostics=%+v", len(servers), diagnostics)
	}
	byName := make(map[string]Server)
	for _, server := range servers {
		byName[server.Name] = server
	}
	valid := byName["valid"]
	if !valid.Supported || !valid.Timeouts.ClaudeToolSet || valid.ClaudeOAuth == nil {
		t.Fatalf("valid current Claude entry rejected: %+v", valid)
	}
	for _, feature := range []Feature{
		FeatureClaudeTimeout, FeatureAlwaysLoad, FeatureClaudeHeadersHelper,
		FeatureClaudeOAuth, FeatureClaudeExpansion,
	} {
		if !slices.Contains(valid.RequiredFeatures(), feature) {
			t.Fatalf("valid features %v omit %s", valid.RequiredFeatures(), feature)
		}
	}
	if placeholder := byName["placeholder"]; !placeholder.Supported || !placeholder.NotConfigured {
		t.Fatalf("empty Claude URL is not a safe placeholder: %+v", placeholder)
	}
	for _, name := range []string{"upper", "missing-type", "bad-scheme", "bad-metadata", "workspace", "__proto__", "Claude?Preview", "Slack sign-in (Claude Code tag)"} {
		if byName[name].Supported {
			t.Fatalf("invalid Claude entry %q remained supported: %+v", name, byName[name])
		}
	}
}

func TestClaudeProjectDenialsApplyAfterPrecedence(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "repo")
	mustMkdir(t, home)
	mustMkdir(t, workspace)
	mustWrite(t, filepath.Join(workspace, ".mcp.json"), `{
  "mcpServers": {"project-denied":{"command":"project"}}
}`)
	mustWrite(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {"all-denied":{"command":"user"}},
  "projects": {
    `+quotedJSONString(workspace)+`: {
      "disabledMcpServers":["all-denied"],
      "disabledMcpjsonServers":["project-denied"],
      "enabledMcpjsonServers":["do-not-import"]
    }
  }
}`)
	result := Discover(Options{HomeDir: home, Workspace: workspace})
	for _, id := range []string{"claude:all-denied", "claude:project-denied"} {
		server := serverNamed(t, result, id)
		if server.Enabled || !server.EnabledSet || server.EnablementSource == "" {
			t.Fatalf("Claude denial was not applied to %s: %+v", id, server)
		}
		if _, err := result.ActivationRequest(id); !errors.Is(err, ErrDisabled) {
			t.Fatalf("disabled server %s accepted activation: %v", id, err)
		}
	}
	project := serverNamed(t, result, "claude:project-denied")
	if project.EnablementSource != "projects.<workspace>.disabledMcpjsonServers" {
		t.Fatalf("project server used the wrong native denial: %+v", project)
	}
	if !hasDiagnostic(result, "foreign-trust-state-ignored") {
		t.Fatal("positive native project approval was not explicitly ignored")
	}
}

func TestClaudeRegularDisableDoesNotDisableProjectMCPJSON(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "repo")
	mustMkdir(t, home)
	mustMkdir(t, workspace)
	mustWrite(t, filepath.Join(workspace, ".mcp.json"), `{
  "mcpServers": {"same-name":{"command":"project"}}
}`)
	mustWrite(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {"same-name":{"command":"user"}},
  "projects": {`+quotedJSONString(workspace)+`:{"disabledMcpServers":["same-name"]}}
}`)
	result := Discover(Options{HomeDir: home, Workspace: workspace})
	server := serverNamed(t, result, "claude:same-name")
	if server.Provenance.Scope != ScopeProject || !server.Enabled {
		t.Fatalf("regular-server toggle disabled project .mcp.json winner: %+v", server)
	}
}

func TestClaudeNestedProjectMCPChainUsesClosestWholeEntry(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "repo")
	nested := filepath.Join(workspace, "packages", "app")
	mustMkdir(t, home)
	mustMkdir(t, nested)
	mustWrite(t, filepath.Join(workspace, ".mcp.json"), `{"mcpServers":{"same":{"command":"root"},"root-only":{"command":"root-only"}}}`)
	mustWrite(t, filepath.Join(nested, ".mcp.json"), `{"mcpServers":{"same":{"command":"nested"}}}`)
	result := Discover(Options{HomeDir: home, Workspace: workspace, CurrentDir: nested})
	same := serverNamed(t, result, "claude:same")
	wantPath, err := filepath.EvalSymlinks(filepath.Join(nested, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if expose(same.Command) != "nested" || same.Provenance.RealPath != wantPath {
		t.Fatalf("closest Claude project entry did not win: %+v", same)
	}
	if expose(serverNamed(t, result, "claude:root-only").Command) != "root-only" {
		t.Fatal("ancestor Claude project entry was lost")
	}
}
