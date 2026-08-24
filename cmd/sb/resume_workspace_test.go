package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/session"
)

func crossWorkspaceSession(t *testing.T, store *session.Store, workspace string) (id, path string, before []byte) {
	t.Helper()
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("release the other workspace")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
		provider.ToolUse{ID: "cross-workspace-call", Name: "exec", Input: json.RawMessage(`{"argv":["release"]}`)},
	}}); err != nil {
		t.Fatal(err)
	}
	id, path = sess.ID(), sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	before, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return id, path, before
}

func TestStartupExplicitResumeRefusesAnotherWorkspaceBeforeRecovery(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	selectedWorkspace := t.TempDir()
	recordedWorkspace := t.TempDir()
	recordedID, recordedPath, before := crossWorkspaceSession(t, store, recordedWorkspace)

	opts := options{resume: recordedID}
	var chosen router.Decision
	if resumed, _, _, _, _, err := openSession(
		context.Background(), store, nil, nil, nil, selectedWorkspace, &opts, &chosen,
	); err == nil {
		resumed.Close()
		t.Fatal("startup adopted a transcript into another workspace's runtime")
	} else if !strings.Contains(err.Error(), recordedWorkspace) || !strings.Contains(err.Error(), "-workspace") {
		t.Fatalf("workspace refusal does not explain the safe launch: %v", err)
	}
	after, err := os.ReadFile(recordedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("startup reconciled or otherwise mutated the rejected session")
	}
}

func TestTUIExplicitResumeRefusesAnotherWorkspaceBeforeRecovery(t *testing.T) {
	m := testModel(t)
	recordedWorkspace := t.TempDir()
	recordedID, recordedPath, before := crossWorkspaceSession(t, m.app.store, recordedWorkspace)
	sourceID := m.app.loop.Session.ID()

	cmd := m.reopen(recordedID)
	if cmd == nil {
		t.Fatal("resume returned no command")
	}
	msg, ok := cmd().(sessionSwapMsg)
	if !ok || msg.err == nil {
		t.Fatalf("cross-workspace resume result = %#v", msg)
	}
	if !strings.Contains(msg.err.Error(), recordedWorkspace) || !strings.Contains(msg.err.Error(), "-workspace") {
		t.Fatalf("workspace refusal does not explain the safe launch: %v", msg.err)
	}
	m.onSessionSwap(msg)
	if m.app.loop.Session.ID() != sourceID {
		t.Fatal("TUI changed sessions after the workspace refusal")
	}
	if m.operationActive {
		t.Fatal("TUI remained busy after the workspace refusal")
	}
	after, err := os.ReadFile(recordedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("TUI reconciled or otherwise mutated the rejected session")
	}
}
