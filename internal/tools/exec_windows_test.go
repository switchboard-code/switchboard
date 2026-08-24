//go:build windows

package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/execution"
)

func TestWindowsExecScriptUsesCmdDialectAndRuns(t *testing.T) {
	registry, err := NewRegistry(t.TempDir(), execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := registry.Get("exec")
	if !ok {
		t.Fatal("no exec tool")
	}
	if schema := string(tool.Schema()); !strings.Contains(schema, "cmd.exe") || strings.Contains(schema, "/bin/sh") {
		t.Fatalf("Windows exec schema describes the wrong shell: %s", schema)
	}
	plan, err := tool.Plan(json.RawMessage(`{"script":"echo switchboard-windows-shell"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := Describe(plan.Request.Argv, plan.Request.Shell); !strings.HasPrefix(got, "cmd.exe /c ") {
		t.Fatalf("Describe() = %q", got)
	}
	result, err := plan.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || !strings.Contains(result.Content, "switchboard-windows-shell") {
		t.Fatalf("Windows script result = %#v", result)
	}
}

func TestWindowsRegistryDisplayUsesSlashPolicyGrammar(t *testing.T) {
	root := t.TempDir()
	registry, err := NewRegistry(root, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	got := registry.display(root + `\src\nested\file.go`)
	if got != "src/nested/file.go" {
		t.Fatalf("display() = %q, want slash-normalized path", got)
	}
}
