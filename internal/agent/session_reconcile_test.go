package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

func TestBindSessionReconcilesInterruptedCallsWithoutExecutingThem(t *testing.T) {
	h := newHarness(t, permission.ModeDefault)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	next, err := store.Create(h.root, "scripted/local/recovered", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()

	path := filepath.Join(h.root, "must-not-change.txt")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := next.AppendMessage(provider.UserText("change the file")); err != nil {
		t.Fatal(err)
	}
	if err := next.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
		provider.ToolUse{ID: "write-before-crash", Name: "write", Input: json.RawMessage(`{"path":"must-not-change.txt","content":"after"}`)},
	}}); err != nil {
		t.Fatal(err)
	}

	if err := h.loop.BindSession(next); err != nil {
		t.Fatal(err)
	}
	if h.loop.Session != next {
		t.Fatal("reconciled session was not published")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "before" {
		t.Fatalf("binding reexecuted the interrupted write: %q", data)
	}
	messages := next.State().Messages
	if len(messages) != 3 || messages[2].Role != provider.RoleTool {
		t.Fatalf("reconciled messages = %#v", messages)
	}
	result, ok := messages[2].Content[0].(provider.ToolResult)
	if !ok || !result.IsError || !strings.Contains(result.Content, "outcome unknown") {
		t.Fatalf("synthetic result = %#v", messages[2].Content[0])
	}
}

func TestBindSessionRefusesWhenRecoveryCannotBeAppended(t *testing.T) {
	h := newHarness(t, permission.ModeDefault)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	next, err := store.Create(h.root, "scripted/local/recovered", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := next.AppendMessage(provider.UserText("run it")); err != nil {
		t.Fatal(err)
	}
	if err := next.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
		provider.ToolUse{ID: "call-closed", Name: "exec", Input: json.RawMessage(`{"argv":["release"]}`)},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}

	old := h.loop.Session
	err = h.loop.BindSession(next)
	if !errors.Is(err, session.ErrSessionPoisoned) {
		t.Fatalf("bind error = %v, want poisoned recovery append", err)
	}
	if h.loop.Session != old {
		t.Fatal("failed recovery published the new session")
	}
	if len(next.State().Messages) != 2 {
		t.Fatal("failed recovery published a synthetic result in memory")
	}
}
