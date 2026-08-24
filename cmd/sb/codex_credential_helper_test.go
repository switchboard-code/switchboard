package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/fileprivacy"
)

func writePrivateCodexAuth(t *testing.T, home string, data []byte) string {
	t.Helper()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "auth.json")
	file, err := fileprivacy.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCodexCredentialHelperDispatchPrintsOnlyTheToken(t *testing.T) {
	home := t.TempDir()
	isolateTestHome(t, home)
	const token = "header.payload.signature"
	writePrivateCodexAuth(t, home, []byte(`{"tokens":{"access_token":"`+token+`"},"account_id":"not-output"}`))

	var out strings.Builder
	handled, err := runCLISubcommand(context.Background(), &out, options{},
		[]string{codexCredentialHelperCommand, codexCredentialHelperKind})
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if got := out.String(); got != token+"\n" {
		t.Fatalf("helper stdout = %q", got)
	}
}

func TestCodexCredentialHelperRejectsInvalidInputWithoutLeakingIt(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "invalid JSON", data: []byte(`{"tokens":{"access_token":"sensitive-token"}`)},
		{name: "missing token", data: []byte(`{"tokens":{}}`)},
		{name: "empty token", data: []byte(`{"tokens":{"access_token":""}}`)},
		{name: "leading whitespace in token", data: []byte(`{"tokens":{"access_token":" sensitive-token"}}`)},
		{name: "control in token", data: []byte(`{"tokens":{"access_token":"sensitive\ntoken"}}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			isolateTestHome(t, home)
			writePrivateCodexAuth(t, home, test.data)
			var out strings.Builder
			err := runCredentialHelperDispatch(&out, []string{codexCredentialHelperKind})
			if err == nil {
				t.Fatal("invalid Codex login was accepted")
			}
			if out.Len() != 0 {
				t.Fatalf("failed helper wrote stdout: %q", out.String())
			}
			if strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("failed helper rendered input bytes: %v", err)
			}
		})
	}
}

func TestCodexCredentialHelperBoundsAuthFile(t *testing.T) {
	home := t.TempDir()
	isolateTestHome(t, home)
	writePrivateCodexAuth(t, home, bytes.Repeat([]byte{' '}, int(maxCodexAuthBytes)+1))

	var out strings.Builder
	err := runCredentialHelperDispatch(&out, []string{codexCredentialHelperKind})
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversize helper error = %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("oversize helper wrote stdout: %q", out.String())
	}
}

func TestCodexCredentialHelperRejectsWrongInternalInvocation(t *testing.T) {
	for _, args := range [][]string{nil, {"other"}, {codexCredentialHelperKind, "extra"}} {
		var out strings.Builder
		if err := runCredentialHelperDispatch(&out, args); err == nil {
			t.Fatalf("internal args %q were accepted", args)
		}
		if out.Len() != 0 {
			t.Fatalf("internal args %q wrote stdout %q", args, out.String())
		}
	}
}

func TestCodexCredentialHelperRefusesNonPrivateFile(t *testing.T) {
	home := t.TempDir()
	isolateTestHome(t, home)
	path := writePrivateCodexAuth(t, home, []byte(`{"tokens":{"access_token":"secret"}}`))
	if err := makeCodexAuthNonPrivateForTest(path); err != nil {
		t.Skipf("cannot widen test file privacy on this platform: %v", err)
	}

	var out strings.Builder
	err := runCredentialHelperDispatch(&out, []string{codexCredentialHelperKind})
	if err == nil || !strings.Contains(err.Error(), "owned or protected") {
		t.Fatalf("non-private helper error = %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("non-private helper wrote stdout: %q", out.String())
	}
}

func TestCodexCredentialHelperRefusesWorkspaceExecutableWithoutConfigMutation(t *testing.T) {
	workspace := t.TempDir()
	executable := filepath.Join(workspace, "sb-test")
	if err := os.WriteFile(executable, []byte("workspace controlled"), 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), config.FileName)
	cfg := &config.Config{
		Path: configPath,
		Auth: map[string]credential.Settings{"anthropic": {Env: "KEEP_ME"}},
	}
	before := cfg.Snapshot()

	err := wireCodexForExecutable(cfg, workspace, executable)
	if err == nil || !strings.Contains(err.Error(), "workspace-controlled") {
		t.Fatalf("workspace helper error = %v", err)
	}
	if !reflect.DeepEqual(cfg.Snapshot(), before) {
		t.Fatalf("rejected helper mutated live config:\nbefore=%#v\nafter=%#v", before, cfg.Snapshot())
	}
	if _, statErr := os.Stat(configPath); !os.IsNotExist(statErr) {
		t.Fatalf("rejected helper created or changed durable config: %v", statErr)
	}
}

func TestCodexCredentialHelperRefusesLaunchCheckoutExecutableForDifferentWorkspace(t *testing.T) {
	launch := t.TempDir()
	if err := os.Mkdir(filepath.Join(launch, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(launch, "bin", "sb-test")
	if err := os.Mkdir(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("launch checkout controlled"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(launch)
	workspace := t.TempDir()
	configPath := filepath.Join(t.TempDir(), config.FileName)
	cfg := &config.Config{
		Path: configPath,
		Auth: map[string]credential.Settings{"anthropic": {Env: "KEEP_ME"}},
	}
	before := cfg.Snapshot()

	err := wireCodexForExecutable(cfg, workspace, executable)
	if err == nil || !strings.Contains(err.Error(), "workspace-controlled") {
		t.Fatalf("launch-checkout helper error = %v", err)
	}
	if !reflect.DeepEqual(cfg.Snapshot(), before) {
		t.Fatalf("rejected launch-checkout helper mutated live config:\nbefore=%#v\nafter=%#v", before, cfg.Snapshot())
	}
	if _, statErr := os.Stat(configPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rejected launch-checkout helper created durable config: %v", statErr)
	}
}

func TestCodexCredentialHelperAcceptsExecutableOutsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	argv, err := codexCredentialHelperArgvForPath(workspace, executable)
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) != 3 || argv[0] != filepath.Clean(want) || argv[1] != codexCredentialHelperCommand || argv[2] != codexCredentialHelperKind {
		t.Fatalf("external helper argv = %q, want canonical %q helper dispatch", argv, want)
	}
}

func TestCodexCredentialHelperAcceptsUnrelatedMarkedInstallPrefix(t *testing.T) {
	prefix := t.TempDir()
	if err := os.Mkdir(filepath.Join(prefix, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(prefix, "bin", "sb-test")
	if err := os.Mkdir(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("package-manager install"), 0o755); err != nil {
		t.Fatal(err)
	}
	argv, err := codexCredentialHelperArgvForPath(t.TempDir(), executable)
	if err != nil {
		t.Fatalf("marked package-prefix helper was rejected: %v", err)
	}
	want, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) != 3 || argv[0] != filepath.Clean(want) {
		t.Fatalf("helper argv = %q, want canonical executable %q", argv, want)
	}
}

func TestCodexCredentialHelperRejectsWorkspaceSymlinkToExternalExecutable(t *testing.T) {
	workspace := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(workspace, "sb-link")
	if err := os.Symlink(executable, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := codexCredentialHelperArgvForPath(workspace, alias); err == nil || !strings.Contains(err.Error(), "workspace-controlled") {
		t.Fatalf("workspace symlink helper error = %v", err)
	}
}

func TestCodexCredentialHelperRejectsExecutableThroughWorkspaceSymlinkAncestor(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	executable := filepath.Join(external, "sb-test")
	if err := os.WriteFile(executable, []byte("external executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(workspace, "linked-bin")
	if err := os.Symlink(external, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	candidate := filepath.Join(alias, filepath.Base(executable))
	if _, err := codexCredentialHelperArgvForPath(workspace, candidate); err == nil || !strings.Contains(err.Error(), "workspace-controlled") {
		t.Fatalf("workspace symlink-ancestor helper error = %v", err)
	}
}

func TestCodexCredentialHelperRejectsExternalSymlinkIntoWorkspace(t *testing.T) {
	workspace := t.TempDir()
	target := filepath.Join(workspace, "sb-test")
	if err := os.WriteFile(target, []byte("workspace controlled"), 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "sb-link")
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := codexCredentialHelperArgvForPath(workspace, alias); err == nil || !strings.Contains(err.Error(), "workspace-controlled") {
		t.Fatalf("external alias into workspace error = %v", err)
	}
}

func TestCodexCredentialHelperRejectsExecutableUnderRepositoryAncestor(t *testing.T) {
	repository := t.TempDir()
	if err := os.Mkdir(filepath.Join(repository, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(repository, "nested", "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(repository, "bin", "sb-test")
	if err := os.Mkdir(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("repository controlled"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := codexCredentialHelperArgvForPath(workspace, executable); err == nil || !strings.Contains(err.Error(), "workspace-controlled") {
		t.Fatalf("repository-ancestor helper error = %v", err)
	}
}

func TestCodexCredentialHelperRejectsSiblingExecutableUnderEveryEnclosingVCSAuthority(t *testing.T) {
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
			executable := filepath.Join(repository, "bin", "sb-test")
			if err := os.Mkdir(filepath.Dir(executable), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(executable, []byte("repository controlled"), 0o755); err != nil {
				t.Fatal(err)
			}

			configPath := filepath.Join(t.TempDir(), config.FileName)
			cfg := &config.Config{
				Path: configPath,
				Auth: map[string]credential.Settings{"anthropic": {Env: "KEEP_ME"}},
			}
			before := cfg.Snapshot()
			err := wireCodexForExecutable(cfg, workspace, executable)
			if err == nil || !strings.Contains(err.Error(), "workspace-controlled") {
				t.Fatalf("sibling helper under %s authority error = %v", test.outerMarker, err)
			}
			if !reflect.DeepEqual(cfg.Snapshot(), before) {
				t.Fatalf("rejected sibling helper mutated live config:\nbefore=%#v\nafter=%#v", before, cfg.Snapshot())
			}
			if _, statErr := os.Stat(configPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("rejected sibling helper created durable config: %v", statErr)
			}
		})
	}
}

func TestCodexCredentialHelperCanonicalizesWorkspaceAliasBeforeFindingRepositoryAuthority(t *testing.T) {
	repository := t.TempDir()
	if err := os.Mkdir(filepath.Join(repository, ".hg"), 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(repository, "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	workspaceAlias := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(workspace, workspaceAlias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	executable := filepath.Join(repository, "bin", "sb-test")
	if err := os.Mkdir(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("repository controlled"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := codexCredentialHelperArgvForPath(workspaceAlias, executable); err == nil || !strings.Contains(err.Error(), "workspace-controlled") {
		t.Fatalf("aliased workspace repository authority error = %v", err)
	}
}
