package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestCodexRequirementsAbsentIsStrict(t *testing.T) {
	if checked, err := codexRequirementsAbsent([]byte(`{"requirements":null}`)); err != nil || !checked {
		t.Fatalf("null requirements = %v, %v", checked, err)
	}
	if checked, err := codexRequirementsAbsent([]byte(`{"requirements":{}}`)); err != nil || checked {
		t.Fatalf("present requirements = %v, %v", checked, err)
	}
	for _, raw := range []string{
		`{"requirements":null,"extra":true}`,
		`{"requirements":null,"requirements":{}}`,
		`{}`,
	} {
		if _, err := codexRequirementsAbsent([]byte(raw)); err == nil {
			t.Fatalf("invalid requirements snapshot %s was accepted", raw)
		}
	}
}

func TestCodexAppServerSnapshotUsesBoundedAuthoritativeResponses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX test launcher")
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.toml")
	if err := os.WriteFile(configPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	name := map[string]any{"type": "user", "file": configPath, "profile": nil}
	configResult, err := json.Marshal(map[string]any{
		"config":  map[string]any{"mcp_servers": map[string]any{}},
		"origins": map[string]any{},
		"layers": []any{map[string]any{
			"name": name, "version": "test-v1", "config": map[string]any{},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	responses := []string{
		`{"id":1,"result":{"ok":true}}`,
		fmt.Sprintf(`{"id":2,"result":%s}`, configResult),
		`{"id":3,"result":{"requirements":null}}`,
	}
	script := "#!/bin/sh\n"
	for _, response := range responses {
		script += "IFS= read -r request || exit 1\nprintf '%s\\n' '" + strings.ReplaceAll(response, "'", "'\"'\"'") + "'\n"
	}
	codexPath := filepath.Join(bin, "codex")
	environmentPath := filepath.Join(root, "app-server.env")
	script = "#!/bin/sh\n/usr/bin/env > '" + environmentPath + "'\n" + strings.TrimPrefix(script, "#!/bin/sh\n")
	if err := os.WriteFile(codexPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("SB_CODEX_TEST_TOKEN", "must-not-reach-app-server")

	snapshot, err := readCodexAppServerSnapshot(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.config == nil || !snapshot.requirementsChecked {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	environment, err := os.ReadFile(environmentPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(environment), "must-not-reach-app-server") || strings.Contains(string(environment), "SB_CODEX_TEST_TOKEN") {
		t.Fatalf("credential-bearing environment reached Codex app-server: %q", environment)
	}
}

func TestCodexExecutableCannotBeWorkspaceSymlinkToExternalBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses POSIX executable permissions")
	}
	workspace := t.TempDir()
	marker := filepath.Join(t.TempDir(), "codex-ran")
	external := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(external, []byte("#!/bin/sh\n/usr/bin/touch '"+marker+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(workspace, "codex")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("PATH", workspace)
	if _, err := readCodexAppServerSnapshot(context.Background(), workspace); err == nil || !strings.Contains(err.Error(), "workspace-local") {
		t.Fatalf("workspace-symlinked Codex executable error = %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("workspace-symlinked Codex binary executed: %v", err)
	}
}

func TestCodexExecutableCannotComeFromWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses POSIX executable permissions")
	}
	workspace := t.TempDir()
	codexPath := filepath.Join(workspace, "codex")
	if err := os.WriteFile(codexPath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", workspace)
	if _, err := trustedCodexExecutable(workspace); err == nil || !strings.Contains(err.Error(), "workspace-local") {
		t.Fatalf("workspace-local Codex executable = %v", err)
	}
}

func TestCodexExecutableCannotComeFromLaunchCheckoutForDifferentWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses POSIX executable permissions")
	}
	launch := t.TempDir()
	if err := os.Mkdir(filepath.Join(launch, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(launch, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "executed")
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nprintf executed > '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(launch)
	t.Setenv("PATH", bin)
	if _, err := readCodexAppServerSnapshot(context.Background(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "workspace-local") {
		t.Fatalf("launch-checkout Codex error = %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("launch-checkout Codex binary executed: %v", err)
	}
}

func TestCodexExecutableRejectsSiblingUnderEveryEnclosingVCSAuthority(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses POSIX executable permissions")
	}
	tests := []struct {
		name         string
		outerMarker  string
		nestedMarker string
	}{
		{name: "nested git cannot hide outer git", outerMarker: ".git", nestedMarker: ".git"},
		{name: "mercurial", outerMarker: ".hg"},
		{name: "subversion", outerMarker: ".svn"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := t.TempDir()
			if err := os.Mkdir(filepath.Join(repository, test.outerMarker), 0o755); err != nil {
				t.Fatal(err)
			}
			nested := filepath.Join(repository, "nested")
			workspace := filepath.Join(nested, "workspace")
			if err := os.MkdirAll(workspace, 0o755); err != nil {
				t.Fatal(err)
			}
			if test.nestedMarker != "" {
				if err := os.Mkdir(filepath.Join(nested, test.nestedMarker), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			bin := filepath.Join(repository, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)

			if _, err := trustedCodexExecutable(workspace); err == nil || !strings.Contains(err.Error(), "workspace-local") {
				t.Fatalf("sibling Codex under %s authority error = %v", test.outerMarker, err)
			}
		})
	}
}

func TestCodexAppServerEnvShebangCannotDispatchWorkspaceNode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("env-shebang fixture is Unix-only")
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	workspaceBin := filepath.Join(workspace, "bin")
	externalBin := filepath.Join(root, "external-bin")
	if err := os.MkdirAll(workspaceBin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(externalBin, 0o700); err != nil {
		t.Fatal(err)
	}
	workspaceMarker := filepath.Join(root, "workspace-node-ran")
	trustedMarker := filepath.Join(root, "trusted-node-ran")
	environmentPath := filepath.Join(root, "node.env")
	if err := os.WriteFile(filepath.Join(workspaceBin, "node"), []byte("#!/bin/sh\n/usr/bin/touch '"+workspaceMarker+"'\nexit 90\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	trustedNode := "#!/bin/sh\n/usr/bin/touch '" + trustedMarker + "'\n/usr/bin/env > '" + environmentPath + "'\n"
	if err := os.WriteFile(filepath.Join(externalBin, "node"), []byte(trustedNode), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(externalBin, "codex"), []byte("#!/usr/bin/env node\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	maliciousRequire := filepath.Join(workspace, "evil.js")
	if err := os.WriteFile(maliciousRequire, []byte("throw new Error('executed')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", workspaceBin+string(os.PathListSeparator)+externalBin)
	t.Setenv("NODE_OPTIONS", "--require="+maliciousRequire)

	executable, err := trustedCodexExecutable(workspace)
	if err != nil {
		t.Fatal(err)
	}
	command, err := executable.Command("app-server", "--stdio")
	if err != nil {
		t.Fatal(err)
	}
	command.Env, err = codexAppServerEnvironment(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(trustedMarker); err != nil {
		t.Fatalf("trusted external node did not run: %v", err)
	}
	if _, err := os.Stat(workspaceMarker); !os.IsNotExist(err) {
		t.Fatalf("workspace node executed through Codex env shebang: %v", err)
	}
	environment, err := os.ReadFile(environmentPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(environment), "NODE_OPTIONS=") || strings.Contains(string(environment), workspaceBin) {
		t.Fatalf("Codex node received workspace runtime authority: %q", environment)
	}
}

func TestCodexAppServerDropsNodePreloadBeforeEnvShebang(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("env-shebang fixture is Unix-only")
	}
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is not installed")
	}
	nodePath, err = filepath.Abs(nodePath)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	workspaceBin := filepath.Join(workspace, "bin")
	externalBin := filepath.Join(root, "external-bin")
	if err := os.MkdirAll(workspaceBin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(externalBin, 0o700); err != nil {
		t.Fatal(err)
	}
	workspaceNodeMarker := filepath.Join(root, "workspace-node-ran")
	preloadMarker := filepath.Join(root, "node-preload-ran")
	trustedMarker := filepath.Join(root, "trusted-codex-ran")
	if err := os.WriteFile(filepath.Join(workspaceBin, "node"), []byte("#!/bin/sh\n/usr/bin/touch '"+workspaceNodeMarker+"'\nexit 90\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	preload := "require('fs').writeFileSync(" + strconv.Quote(preloadMarker) + ", 'bad')\n"
	if err := os.WriteFile(filepath.Join(workspace, "evil.js"), []byte(preload), 0o600); err != nil {
		t.Fatal(err)
	}
	codex := "#!/usr/bin/env node\nrequire('fs').writeFileSync(" + strconv.Quote(trustedMarker) + ", 'ok')\n"
	if err := os.WriteFile(filepath.Join(externalBin, "codex"), []byte(codex), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", strings.Join([]string{workspaceBin, externalBin, filepath.Dir(nodePath)}, string(os.PathListSeparator)))
	t.Setenv("NODE_OPTIONS", "--require="+filepath.Join(workspace, "evil.js"))

	executable, err := trustedCodexExecutable(workspace)
	if err != nil {
		t.Fatal(err)
	}
	command, err := executable.Command("app-server", "--stdio")
	if err != nil {
		t.Fatal(err)
	}
	command.Env, err = codexAppServerEnvironment(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(trustedMarker); err != nil {
		t.Fatalf("trusted Codex script did not run: %v", err)
	}
	for name, path := range map[string]string{"workspace Node": workspaceNodeMarker, "NODE_OPTIONS preload": preloadMarker} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s executed through Codex app-server launch: %v", name, err)
		}
	}
}
