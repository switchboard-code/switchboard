package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/config"
)

func TestThinkChangesEffortForTheSession(t *testing.T) {
	m := testModel(t)

	if cmd := m.applyThink("high"); cmd == nil {
		t.Fatal("applyThink produced no result")
	}
	if got := effortOf(m.app.loop.Target); got != "high" {
		t.Fatalf("the loop's target carries effort %q, want high", got)
	}
	if !strings.Contains(string(m.app.loop.Target.ID()), "think") {
		t.Fatalf("the target ID should say it thinks: %s", m.app.loop.Target.ID())
	}
	if !strings.Contains(m.View(), "think high") {
		t.Fatal("the status bar should show the effort")
	}

	m.applyThink("default")
	if got := effortOf(m.app.loop.Target); got != "" {
		t.Fatalf("default should clear the effort, got %q", got)
	}

	if cmd := m.applyThink("ultra"); cmd == nil {
		t.Fatal("an unknown level should produce an error notice")
	} else if n := cmd().(noticeMsg); n.level != "error" {
		t.Fatalf("an unknown level should refuse, got %#v", n)
	}
}

func TestSetupChecklistCoversTheMachine(t *testing.T) {
	home := t.TempDir()
	isolateTestHome(t, home)

	m := modelsTestModel(t) // real catalog, unreachable local server

	msg, ok := setupChecklist(m)().(pickerMsg)
	if !ok {
		t.Fatalf("expected the checklist, got %T", msg)
	}

	byID := map[string]pickerItem{}
	for _, it := range msg.items {
		byID[it.id] = it
	}
	if it, found := byID[setupLocalID]; !found || !strings.Contains(it.desc, "not answering") {
		t.Fatalf("the local row should report the server is down: %+v", it)
	}
	if _, found := byID["kimi/coding"]; !found {
		t.Fatal("catalog providers should each get a row")
	}
	if _, found := byID[setupCodexID]; found {
		t.Fatal("no ~/.codex/auth.json here, so no codex row should be offered")
	}
	if _, found := byID[setupDoneID]; !found {
		t.Fatal("the checklist needs a way out")
	}

	// With a codex login on the machine and no helper wired, the offer appears.
	codexDir := filepath.Join(home, ".codex")
	os.MkdirAll(codexDir, 0o755)
	os.WriteFile(filepath.Join(codexDir, "auth.json"),
		[]byte(`{"tokens":{"access_token":"not-a-real-token"}}`), 0o600)
	msg = setupChecklist(m)().(pickerMsg)
	found := false
	for _, it := range msg.items {
		found = found || it.id == setupCodexID
	}
	if !found {
		t.Fatal("a codex login on the machine should be offered for wiring")
	}
}

func TestWireCodexHelperPersists(t *testing.T) {
	m := testModel(t)
	m.app.workspace = t.TempDir()
	m.app.config.Path = filepath.Join(t.TempDir(), config.FileName)

	msg := wireCodexHelper(m)()
	if n, ok := msg.(noticeMsg); !ok || n.level == "error" {
		t.Fatalf("wiring failed: %#v", msg)
	}

	saved, err := config.LoadFile(m.app.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	helper := saved.AuthFor("openai").Helper
	if len(helper) != 3 || !filepath.IsAbs(helper[0]) || helper[1] != codexCredentialHelperCommand || helper[2] != codexCredentialHelperKind {
		t.Fatalf("the helper did not persist: %v", helper)
	}
	joined := strings.ToLower(strings.Join(helper, " "))
	if strings.Contains(joined, "python") || strings.Contains(joined, "sh -c") || strings.Contains(joined, "auth.json") {
		t.Fatalf("the persisted helper contains an interpreter or credential path: %v", helper)
	}

	// Once wired, setup stops offering it.
	if codexLoginAvailable(m.app.config) {
		t.Fatal("a wired login should not be offered again")
	}
}

func TestFailedCodexHelperSaveLeavesLiveAuthAndProvidersUnchanged(t *testing.T) {
	m := testModel(t)
	m.app.workspace = t.TempDir()
	m.app.config.Path = t.TempDir() // a directory cannot be replaced by the config file
	m.app.providers = newProviders("http://127.0.0.1:1", m.app.config)
	beforeGeneration := m.app.providers.generation

	msg := wireCodexHelper(m)()
	notice, ok := msg.(noticeMsg)
	if !ok || notice.level != "error" || !strings.Contains(notice.text, "wiring the codex login failed") {
		t.Fatalf("failed helper save = %#v, want error notice", msg)
	}
	if helper := m.app.config.AuthFor("openai").Helper; len(helper) != 0 {
		t.Fatalf("failed save left a live helper: %v", helper)
	}
	if m.app.providers.generation != beforeGeneration {
		t.Fatal("failed helper save reset cached providers")
	}
}
