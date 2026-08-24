package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/session"
)

func requireSafeResumeLaunch(t *testing.T, resumeErr error, recordedWorkspace string) {
	t.Helper()
	const marker = "restart switchboard with -workspace "
	message := resumeErr.Error()
	start := strings.Index(message, marker)
	if start < 0 {
		t.Fatalf("workspace refusal does not explain the safe launch: %v", resumeErr)
	}
	argument := strings.TrimSuffix(message[start+len(marker):], " to resume it")
	if argument == message[start+len(marker):] {
		t.Fatalf("workspace refusal has an incomplete safe launch: %v", resumeErr)
	}
	path, err := strconv.Unquote(argument)
	if err != nil {
		t.Fatalf("workspace refusal has an invalid -workspace argument %q: %v", argument, err)
	}
	want, err := os.Stat(recordedWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.Stat(path)
	if err != nil || !os.SameFile(got, want) {
		t.Fatalf("safe launch workspace = %q, want the recorded workspace %q: %v", path, recordedWorkspace, err)
	}
}

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
	} else {
		requireSafeResumeLaunch(t, err, recordedWorkspace)
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
	requireSafeResumeLaunch(t, msg.err, recordedWorkspace)
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
