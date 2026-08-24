package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/session"
)

func TestStartupResumeReconcilesInterruptedToolCalls(t *testing.T) {
	server := fakeOllama(t, "resume-model")
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	tier := ollamaTier("t1", "resume-model")
	cfg := &config.Config{Tiers: []config.Tier{tier}}
	cat := catalogWithLocalModels(t, localModelSpec{name: "resume-model", contextWindow: 100_000})

	original, err := store.Create(workspace, tier.Target.ID(), cat.Revision)
	if err != nil {
		t.Fatal(err)
	}
	id := original.ID()
	appendCrashToolCall(t, original, "startup-call")
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}

	opts := options{resume: id}
	var chosen router.Decision
	resumed, _, _, wasResumed, _, err := openSession(
		context.Background(), store, newProviders(server.URL, cfg), cfg, cat, workspace, &opts, &chosen,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if !wasResumed {
		t.Fatal("startup did not report the opened session as resumed")
	}
	assertRecoveredToolTail(t, resumed.State(), "startup-call")
}

func TestTUIResumeReconcilesBeforeFeasibilityAndSwap(t *testing.T) {
	m := testModel(t)
	server := fakeOllama(t, "resume-model")
	tier := ollamaTier("t2", "resume-model")
	m.app.catalog = catalogWithLocalModels(t, localModelSpec{name: "resume-model", contextWindow: 100_000})
	m.app.config.Tiers = append(m.app.config.Tiers, tier)
	m.app.providers = newProviders(server.URL, m.app.config)

	original, err := m.app.store.Create(m.app.workspace, tier.Target.ID(), m.app.catalog.Revision)
	if err != nil {
		t.Fatal(err)
	}
	id := original.ID()
	appendCrashToolCall(t, original, "tui-call")
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := m.reopen(id)
	if cmd == nil {
		t.Fatal("resume returned no command")
	}
	msg, ok := cmd().(sessionSwapMsg)
	if !ok || msg.err != nil {
		t.Fatalf("resume result = %#v", msg)
	}
	// The asynchronous reopen performs feasibility checks before the UI adopts
	// the session, so its returned state must already be provider-safe.
	assertRecoveredToolTail(t, msg.sess.State(), "tui-call")
	m.onSessionSwap(msg)
	if m.app.loop.Session.ID() != id {
		t.Fatalf("TUI adopted session %s, want %s", m.app.loop.Session.ID(), id)
	}
}

func appendCrashToolCall(t *testing.T, sess *session.Session, id string) {
	t.Helper()
	if err := sess.AppendMessage(provider.UserText("publish the release")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
		provider.ToolUse{ID: id, Name: "exec", Input: json.RawMessage(`{"argv":["release"]}`)},
	}}); err != nil {
		t.Fatal(err)
	}
}

func assertRecoveredToolTail(t *testing.T, state session.State, id string) {
	t.Helper()
	if len(state.Messages) != 3 {
		t.Fatalf("recovered messages = %d, want user/call/result", len(state.Messages))
	}
	message := state.Messages[2]
	if message.Role != provider.RoleTool || len(message.Content) != 1 {
		t.Fatalf("recovered tail = %#v", message)
	}
	result, ok := message.Content[0].(provider.ToolResult)
	if !ok || result.ToolUseID != id || !result.IsError || !strings.Contains(result.Content, "outcome unknown") {
		t.Fatalf("recovered result = %#v", message.Content[0])
	}
}
