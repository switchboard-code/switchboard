package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// A session with no skills must render tool schemas byte-identical to a
// build without the feature: the definitions sit in the frozen zone, and a
// tool that appears for everyone would move every user's cached prefix.
func TestNoSkillsLeavesTheSchemasByteIdentical(t *testing.T) {
	isolateTestHome(t, t.TempDir())
	ws := t.TempDir()

	bare, err := tools.NewRegistry(ws, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	assembled, err := tools.NewRegistry(ws, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	list, notes := addSkills(assembled, ws)
	if len(list) != 0 || len(notes) != 0 {
		t.Fatalf("an empty machine grew skills: %v %v", list, notes)
	}

	before, err := json.Marshal(bare.Definitions())
	if err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(assembled.Definitions())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("schemas changed with no skills loaded:\n%s\nwant\n%s", after, before)
	}
}

func TestSkillsJoinTheSuiteWhenDefined(t *testing.T) {
	isolateTestHome(t, t.TempDir())
	ws := t.TempDir()
	dir := filepath.Join(ws, ".switchboard", "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "migrations.md"),
		[]byte("---\ndescription: how migrations are written here\n---\nNumbered, never edited after merge.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	registry, err := tools.NewRegistry(ws, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	list, notes := addSkills(registry, ws)
	if len(list) != 1 || len(notes) != 1 {
		t.Fatalf("loaded %v with notes %v, want one skill and its count note", list, notes)
	}
	if _, ok := registry.Get("skill"); !ok {
		t.Fatal("the skill tool did not register")
	}
}

func TestManualOnlySkillsStayInInventoryWithoutChangingSchemas(t *testing.T) {
	isolateTestHome(t, t.TempDir())
	ws := t.TempDir()
	dir := filepath.Join(ws, ".agents", "skills", "deploy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(
		"---\nname: deploy\ndescription: deploy deliberately\ndisable-model-invocation: true\n---\nDeploy only when asked.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	bare, err := tools.NewRegistry(ws, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	assembled, err := tools.NewRegistry(ws, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	inventory, notes := addSkills(assembled, ws)
	if len(inventory) != 1 || !inventory[0].ImplicitDisabled {
		t.Fatalf("manual-only skill disappeared from inventory: %+v", inventory)
	}
	if len(notes) != 1 || notes[0].text != "skills: 1 discovered, none model-visible" {
		t.Fatalf("manual-only assembly notes = %+v", notes)
	}
	if _, ok := assembled.Get("skill"); ok {
		t.Fatal("manual-only inventory registered a model tool")
	}
	before, _ := json.Marshal(bare.Definitions())
	after, _ := json.Marshal(assembled.Definitions())
	if string(before) != string(after) {
		t.Fatalf("manual-only inventory changed model schemas:\n%s\nwant\n%s", after, before)
	}
}
