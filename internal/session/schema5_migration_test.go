package session

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func TestSchemaFourUpgradesBeforeDraftAppendAndReopens(t *testing.T) {
	if SchemaVersion != 5 {
		t.Fatalf("test requires the v4-to-v5 migration boundary, current schema is %d", SchemaVersion)
	}
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "scripted/local/migrate", "rev-v4")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendRuntimeBinding("t2", "scripted/local/migrate", true); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("preserve this v4 conversation")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
		provider.Text{Text: "preserved answer"},
	}}); err != nil {
		t.Fatal(err)
	}
	id, path := sess.ID(), sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	v5Header := []byte(fmt.Sprintf("%s %d\n", magic, SchemaVersion))
	v4Header := []byte(magic + " 4\n")
	legacy := bytes.Replace(raw, v5Header, v4Header, 1)
	if bytes.Equal(legacy, raw) {
		t.Fatalf("session did not contain the expected schema-%d header", SchemaVersion)
	}
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(id)
	if err != nil {
		t.Fatalf("opening valid schema-4 session: %v", err)
	}
	state := reopened.State()
	if len(state.Messages) != 2 || state.Messages[0].Text() != "preserve this v4 conversation" ||
		state.Messages[1].Text() != "preserved answer" {
		t.Fatalf("schema-4 replay lost messages: %#v", state.Messages)
	}
	if got := state.RuntimeBinding; got.Tier != "t2" || got.Target != "scripted/local/migrate" || !got.Pinned {
		t.Fatalf("schema-4 replay lost runtime binding: %+v", got)
	}
	upgraded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(upgraded, v5Header) {
		t.Fatalf("schema-4 open did not durably upgrade before append: %q", strings.SplitN(string(upgraded), "\n", 2)[0])
	}

	draftID, err := reopened.CheckpointAssistantDraft("", []provider.Event{{
		Type: provider.EventTextDelta, Index: 0, Text: "v5 draft after migration",
	}})
	if err != nil {
		t.Fatalf("appending v5 assistant draft after migration: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := store.Open(id)
	if err != nil {
		t.Fatalf("reopening migrated session after v5 append: %v", err)
	}
	defer again.Close()
	state = again.State()
	if len(state.Messages) != 3 || !state.Messages[2].Incomplete || state.Messages[2].DraftID != draftID ||
		state.Messages[2].Text() != "v5 draft after migration" {
		t.Fatalf("migrated v4 state or v5 append did not survive reopen: %#v", state.Messages)
	}
	if got := state.RuntimeBinding; got.Tier != "t2" || got.Target != "scripted/local/migrate" || !got.Pinned {
		t.Fatalf("migrated runtime binding changed after v5 append: %+v", got)
	}
}
