package mcpnative

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestClaudeManagedMCPIsExclusiveServerSource(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "repo")
	managedPath := filepath.Join(root, "managed-mcp.json")
	mustMkdir(t, home)
	mustMkdir(t, workspace)
	mustWrite(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers":{"user":{"command":"user-server"}},
  "projects":{`+quotedJSONString(workspace)+`:{"disabledMcpServers":["managed"]}}
}`)
	mustWrite(t, filepath.Join(workspace, ".mcp.json"), `{
  "mcpServers":{"project":{"command":"project-server"}}
}`)
	mustWrite(t, managedPath, `{
  "mcpServers":{"managed":{"command":"managed-server"}}
}`)

	result := Discover(Options{
		HomeDir: home, Workspace: workspace, ClaudeManagedMCPPath: managedPath,
	})
	if len(result.Servers) != 1 || result.Servers[0].ID != "claude:managed" {
		t.Fatalf("managed file was not exclusive: %+v", result.Servers)
	}
	managed := result.Servers[0]
	if managed.Provenance.Scope != ScopeManaged || managed.Provenance.Source != SourceClaudeManaged || managed.ExecutionTrustRequired || !managed.Enabled {
		t.Fatalf("managed provenance/trust = %+v", managed)
	}
	activation := approve(t, result, managed.ID)
	materialized, err := result.Materialize(managed.ID, nil, fixedPolicy{allowed: true}, activation)
	if err != nil || materialized.Command == nil || materialized.Command.Expose() != "managed-server" {
		t.Fatalf("managed materialization = %+v, %v", materialized, err)
	}

	mustWrite(t, managedPath, `{"mcpServers":{}}`)
	result = Discover(Options{
		HomeDir: home, Workspace: workspace, ClaudeManagedMCPPath: managedPath,
	})
	if len(result.Servers) != 0 {
		t.Fatalf("empty managed server set did not disable Claude: %+v", result.Servers)
	}
}

func TestMalformedClaudeManagedMCPNeverFallsBack(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	managedPath := filepath.Join(root, "managed-mcp.json")
	mustMkdir(t, home)
	mustWrite(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers":{"user":{"command":"user-server"}}
}`)
	mustWrite(t, managedPath, `{not-json`)
	result := Discover(Options{HomeDir: home, ClaudeManagedMCPPath: managedPath})
	if len(result.Servers) != 0 {
		t.Fatalf("malformed exclusive file retained lower servers: %+v", result.Servers)
	}
	if _, err := result.ActivationRequest("claude:user"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("malformed managed file fell back to user server: %v", err)
	}
	if len(result.Quarantines) == 0 || result.Quarantines[0].Dialect != DialectClaude {
		t.Fatalf("malformed managed file was not quarantined: %+v", result.Quarantines)
	}
}
