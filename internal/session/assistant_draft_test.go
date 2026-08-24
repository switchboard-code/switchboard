package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func TestAssistantDraftTinyDeltasStayChunkedAndWALLinear(t *testing.T) {
	const (
		chunks    = 4096
		batchSize = 64
		chunk     = "0123456789abcdef"
	)
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("stream many tiny deltas")); err != nil {
		t.Fatal(err)
	}

	id := ""
	for offset := 0; offset < chunks; offset += batchSize {
		events := make([]provider.Event, batchSize)
		for i := range events {
			events[i] = provider.Event{Type: provider.EventTextDelta, Index: 0, Text: chunk}
		}
		id, err = sess.CheckpointAssistantDraft(id, events)
		if err != nil {
			t.Fatalf("checkpoint at delta %d: %v", offset, err)
		}
	}
	draft := sess.assistantDrafts[id]
	if draft == nil || len(draft.blocks) != 1 || len(draft.blocks[0].chunks) != chunks {
		t.Fatalf("incremental draft shape = %#v", draft)
	}
	if got := sess.state.Messages[draft.messageIndex].Content; len(got) != 0 {
		t.Fatalf("checkpoint path materialized %d canonical blocks instead of retaining chunks", len(got))
	}

	want := strings.Repeat(chunk, chunks)
	state := sess.State()
	if len(state.Messages) != 2 || state.Messages[1].Text() != want {
		t.Fatalf("materialized tiny-delta draft bytes = %d, want %d", len(state.Messages[1].Text()), len(want))
	}
	raw, err := os.ReadFile(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if occurrences := bytes.Count(raw, []byte(chunk)); occurrences != chunks {
		t.Fatalf("WAL stored chunk %d times, want exactly %d delta copies", occurrences, chunks)
	}
	if len(raw) > len(want)*8 {
		t.Fatalf("WAL grew %d bytes for %d draft bytes; delta records must remain linear", len(raw), len(want))
	}
	readState, err := ReadState(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(readState.Messages) != 2 || readState.Messages[1].Text() != want {
		t.Fatalf("read-only replay lost chunked draft: %#v", readState.Messages)
	}
	timeline, err := ReadTimeline(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(timeline) != 2 || timeline[1].Message == nil || timeline[1].Message.Text() != want {
		t.Fatalf("timeline lost chunked draft: %#v", timeline)
	}
}

func TestOversizedAssistantDraftDoesNotConsumeSequence(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	id := sess.ID()
	// JSON renders each NUL as six bytes, producing a frame just beyond the
	// bound without retaining a 64 MiB source string in the test process.
	oversized := strings.Repeat("\x00", maxSessionRecordBytes/6+1024)
	_, err = sess.CheckpointAssistantDraft("", []provider.Event{{
		Type: provider.EventTextDelta, Index: 0, Text: oversized,
	}})
	if !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("oversized CheckpointAssistantDraft error = %v, want ErrRecordTooLarge", err)
	}
	if sess.seq != 1 {
		t.Fatalf("sequence after rejected draft = %d, want session_start sequence 1", sess.seq)
	}
	if err := sess.AppendNote("info", "small record after rejected draft"); err != nil {
		t.Fatalf("small append after rejected draft: %v", err)
	}
	if sess.seq != 2 {
		t.Fatalf("sequence after small append = %d, want 2", sess.seq)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(id)
	if err != nil {
		t.Fatalf("reopening after rejected draft and small append: %v", err)
	}
	defer reopened.Close()
	if reopened.seq != 2 {
		t.Fatalf("replayed sequence = %d, want 2", reopened.seq)
	}
}

func TestAssistantDraftCheckpointsFoldAndFinalizeAsOneMessage(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("explain then inspect")); err != nil {
		t.Fatal(err)
	}

	id, err := sess.CheckpointAssistantDraft("", []provider.Event{
		{Type: provider.EventThinkingDelta, Index: 0, Text: "consider ", Signature: "sig-1"},
		{Type: provider.EventTextDelta, Index: 1, Text: "first"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("first checkpoint returned no draft id")
	}
	idAgain, err := sess.CheckpointAssistantDraft(id, []provider.Event{
		{Type: provider.EventThinkingDelta, Index: 0, Text: "the file", Signature: "sig-2"},
		{Type: provider.EventTextDelta, Index: 1, Text: " second"},
		{Type: provider.EventToolUse, Index: 2, ToolUse: &provider.ToolUse{ID: "call-read", Name: "read", Input: json.RawMessage(`{"path":"main.go"}`)}},
	})
	if err != nil || idAgain != id {
		t.Fatalf("second checkpoint id=%q err=%v", idAgain, err)
	}
	draftState := sess.State()
	if len(draftState.Messages) != 2 {
		t.Fatalf("checkpoint state has %d messages, want user plus one logical draft", len(draftState.Messages))
	}
	draft := draftState.Messages[1]
	if !draft.Incomplete || draft.DraftID != id || draft.Text() != "first second" || len(draft.ToolUses()) != 1 {
		t.Fatalf("folded draft=%#v", draft)
	}
	thinking, ok := draft.Content[0].(provider.Thinking)
	if !ok || thinking.Text != "consider the file" || thinking.Signature != "sig-2" {
		t.Fatalf("folded thinking=%#v", draft.Content[0])
	}

	final := provider.CloneMessage(draft)
	final.Incomplete = false
	if err := sess.AppendMessage(final); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleTool, Content: []provider.Block{
		provider.ToolResult{ToolUseID: "call-read", Name: "read", Content: "package main"},
	}}); err != nil {
		t.Fatal(err)
	}
	state := sess.State()
	if len(state.Messages) != 3 || state.Messages[1].Incomplete || state.Messages[1].DraftID != id {
		t.Fatalf("finalization did not replace the draft: %#v", state.Messages)
	}
	idSession, path := sess.ID(), sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(idSession)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.State().Messages; len(got) != 3 || got[1].Incomplete || got[1].Text() != "first second" {
		t.Fatalf("replayed finalized draft=%#v", got)
	}
	timeline, err := ReadTimeline(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(timeline) != 3 || timeline[1].Message == nil || timeline[1].Message.Incomplete || timeline[1].Message.Text() != "first second" {
		t.Fatalf("timeline exposed physical checkpoints: %#v", timeline)
	}
}

func TestAssistantDraftCrashTailIsRecoverableExcludedAndForkable(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("continue the refactor")); err != nil {
		t.Fatal(err)
	}
	id, err := sess.CheckpointAssistantDraft("", []provider.Event{
		{Type: provider.EventTextDelta, Index: 0, Text: "visible before the kill"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := sess.ID()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	state := reopened.State()
	if len(state.Messages) != 2 || !state.Messages[1].Incomplete || state.Messages[1].DraftID != id || state.Messages[1].Text() != "visible before the kill" {
		t.Fatalf("crash tail was not reconstructed: %#v", state.Messages)
	}
	projected := provider.ReplayRequest(provider.Request{Messages: state.Messages})
	if len(projected.Messages) != 1 || projected.Messages[0].Role != provider.RoleUser {
		t.Fatalf("incomplete draft reached provider replay: %#v", projected.Messages)
	}

	child, err := store.ForkSession(reopened, len(state.Messages))
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	childMessages := child.State().Messages
	if len(childMessages) != 2 || !childMessages[1].Incomplete || childMessages[1].Text() != "visible before the kill" {
		t.Fatalf("full-tip fork lost or duplicated the draft: %#v", childMessages)
	}
	if got := reopened.State().Messages; len(got) != 2 || got[1].Text() != "visible before the kill" {
		t.Fatalf("fork mutated the source draft: %#v", got)
	}
}

func TestAssistantDraftRejectsUnsupportedEventsWithoutAppending(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	before := len(sess.State().Messages)
	if _, err := sess.CheckpointAssistantDraft("", []provider.Event{{Type: provider.EventDone}}); err == nil {
		t.Fatal("done event was accepted as draft content")
	}
	if after := len(sess.State().Messages); after != before {
		t.Fatalf("rejected draft changed state: %d -> %d", before, after)
	}
}

func TestTornAssistantFinalizationFallsBackToTheSyncedDraft(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("finish this answer")); err != nil {
		t.Fatal(err)
	}
	id, err := sess.CheckpointAssistantDraft("", []provider.Event{
		{Type: provider.EventTextDelta, Index: 0, Text: "durable visible prefix"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{
		Role: provider.RoleAssistant, DraftID: id,
		Content: []provider.Block{provider.Text{Text: "durable visible prefix"}},
	}); err != nil {
		t.Fatal(err)
	}
	sessionID, path := sess.ID(), sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	finalStart := strings.LastIndexByte(string(raw[:len(raw)-1]), '\n') + 1
	if finalStart <= 0 || finalStart >= len(raw) {
		t.Fatalf("could not locate final message frame in %d bytes", len(raw))
	}
	if err := os.WriteFile(path, raw[:finalStart+(len(raw)-finalStart)/2], 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	state := reopened.State()
	if reopened.TruncatedBytes() == 0 || len(state.Messages) != 2 || !state.Messages[1].Incomplete || state.Messages[1].Text() != "durable visible prefix" {
		t.Fatalf("torn finalization did not fall back to its checkpoint: truncated=%d messages=%#v", reopened.TruncatedBytes(), state.Messages)
	}
}
