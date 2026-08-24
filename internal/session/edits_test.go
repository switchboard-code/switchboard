package session

import (
	"encoding/json"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
)

// The reader's whole value is pairing each call with the records around
// it, so the fixture is a log shaped the way the loop writes one:
// message, usage, results, and the turn's route record after it all.
func TestReadFileEditsPairsCallsWithTheirAttribution(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	light := provider.RouteTargetID("ollama/local/qwen3:4b")
	heavy := provider.RouteTargetID("kimi/api/k2")

	sess, err := store.Create(workspace, light, "test")
	if err != nil {
		t.Fatal(err)
	}

	// Turn 1: a write on the light rung, routed there and completed there.
	appendAll(t, sess,
		provider.UserText("make a parser"),
		provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
			provider.ToolUse{ID: "w1", Name: "write", Input: json.RawMessage(`{"path":"parser.go","content":"a\nb\n"}`)},
		}},
	)
	if err := sess.AppendUsage(Usage{Target: string(light)}); err != nil {
		t.Fatal(err)
	}
	appendAll(t, sess, provider.Message{Role: provider.RoleTool, Content: []provider.Block{
		provider.ToolResult{ToolUseID: "w1", Name: "write", Content: "wrote parser.go"},
	}})
	if err := sess.AppendRoute(Route{TurnDepth: 0, Tier: "t1", Target: light}); err != nil {
		t.Fatal(err)
	}

	// Turn 2: an edit that succeeded and one that failed, produced on a
	// target the turn's route record does not name — so no rung rides it.
	appendAll(t, sess,
		provider.UserText("rename b"),
		provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
			provider.ToolUse{ID: "e1", Name: "edit", Input: json.RawMessage(`{"path":"parser.go","old_string":"b","new_string":"c"}`)},
			provider.ToolUse{ID: "e2", Name: "edit", Input: json.RawMessage(`{"path":"parser.go","old_string":"zz","new_string":"q"}`)},
		}},
	)
	if err := sess.AppendUsage(Usage{Target: string(heavy)}); err != nil {
		t.Fatal(err)
	}
	appendAll(t, sess, provider.Message{Role: provider.RoleTool, Content: []provider.Block{
		provider.ToolResult{ToolUseID: "e1", Name: "edit", Content: "edited parser.go"},
		provider.ToolResult{ToolUseID: "e2", Name: "edit", Content: "old_string was not found", IsError: true},
	}})
	if err := sess.AppendRoute(Route{TurnDepth: 3, Tier: "t1", Target: light}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	infos, err := store.List(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("one session recorded, %d listed", len(infos))
	}
	edits, err := ReadFileEdits(infos[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 2 {
		t.Fatalf("two calls succeeded, %d replayed: %+v", len(edits), edits)
	}

	write := edits[0]
	if !write.Write || write.Path != "parser.go" || write.Content != "a\nb\n" {
		t.Errorf("the write lost its bytes: %+v", write)
	}
	if write.Turn != 1 || write.Prompt != "make a parser" {
		t.Errorf("the write is not filed under its turn: %+v", write)
	}
	if write.Target != string(light) || write.Tier != "t1" {
		t.Errorf("the write's attribution drifted: target %q tier %q", write.Target, write.Tier)
	}
	if write.Workspace != workspace {
		t.Errorf("the workspace did not ride along: %q", write.Workspace)
	}

	edit := edits[1]
	if edit.Write || edit.Old != "b" || edit.New != "c" {
		t.Errorf("the edit lost its replacement: %+v", edit)
	}
	if edit.Turn != 2 || edit.Prompt != "rename b" {
		t.Errorf("the edit is not filed under its turn: %+v", edit)
	}
	if edit.Target != string(heavy) {
		t.Errorf("the edit's target should come from the usage record: %q", edit.Target)
	}
	if edit.Tier != "" {
		t.Errorf("the route record names another target; a rung here is a guess: %q", edit.Tier)
	}
}

// A call whose result never arrived — an interrupted turn — must not be
// replayed: the log does not say the file changed.
func TestReadFileEditsDropsCallsWithoutResults(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	target := provider.RouteTargetID("ollama/local/qwen3:4b")
	sess, err := store.Create(workspace, target, "test")
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, sess,
		provider.UserText("write a file"),
		provider.Message{Role: provider.RoleAssistant, Incomplete: true, Content: []provider.Block{
			provider.ToolUse{ID: "w1", Name: "write", Input: json.RawMessage(`{"path":"a.go","content":"x"}`)},
		}},
	)
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	infos, err := store.List(workspace)
	if err != nil {
		t.Fatal(err)
	}
	edits, err := ReadFileEdits(infos[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 0 {
		t.Fatalf("an interrupted call replayed as a mutation: %+v", edits)
	}
}

// An injected mid-turn message is not the user speaking; it must not open
// a turn or steal the prompt the real opening carries.
func TestReadFileEditsIgnoresInjectedMessages(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	target := provider.RouteTargetID("ollama/local/qwen3:4b")
	sess, err := store.Create(workspace, target, "test")
	if err != nil {
		t.Fatal(err)
	}
	injected := provider.UserText("verifier: tests now failing")
	injected.Injected = true
	appendAll(t, sess,
		provider.UserText("fix the build"),
		injected,
		provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
			provider.ToolUse{ID: "w1", Name: "write", Input: json.RawMessage(`{"path":"a.go","content":"x"}`)},
		}},
	)
	if err := sess.AppendUsage(Usage{Target: string(target)}); err != nil {
		t.Fatal(err)
	}
	appendAll(t, sess, provider.Message{Role: provider.RoleTool, Content: []provider.Block{
		provider.ToolResult{ToolUseID: "w1", Name: "write", Content: "wrote a.go"},
	}})
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	infos, err := store.List(workspace)
	if err != nil {
		t.Fatal(err)
	}
	edits, err := ReadFileEdits(infos[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 1 {
		t.Fatalf("one call succeeded, %d replayed", len(edits))
	}
	if edits[0].Turn != 1 || edits[0].Prompt != "fix the build" {
		t.Errorf("the injected message moved the turn: %+v", edits[0])
	}
}

func TestReadFileEditsWithholdsLegacyExpansionAndIgnoresMachineUserRoles(t *testing.T) {
	store, workspace := newStore(t)
	target := provider.RouteTargetID("ollama/local/qwen3:4b")
	sess, err := store.Create(workspace, target, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	const expanded = "write @private.txt\nEXPANDED_FILE_BYTES"
	legacy := provider.Message{Role: provider.RoleUser, Content: []provider.Block{provider.Text{Text: expanded}}}
	injected := provider.Message{Role: provider.RoleUser, Injected: true, Content: []provider.Block{
		provider.Text{Text: "[advisor] INJECTED_OUTPUT"},
	}}
	appendAll(t, sess, legacy, injected, provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
		provider.ToolUse{ID: "w1", Name: "write", Input: json.RawMessage(`{"path":"a.go","content":"safe\n"}`)},
	}})
	if err := sess.AppendUsage(Usage{Target: string(target)}); err != nil {
		t.Fatal(err)
	}
	// Anthropic-compatible histories may carry tool results in a user-role
	// message. That protocol carrier is not a second authored opening.
	appendAll(t, sess, provider.Message{Role: provider.RoleUser, Content: []provider.Block{
		provider.ToolResult{ToolUseID: "w1", Name: "write", Content: "TOOL_RESULT_OUTPUT"},
	}})

	edits, err := ReadFileEdits(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 1 {
		t.Fatalf("successful legacy write replay = %+v", edits)
	}
	if edits[0].Turn != 1 || edits[0].Prompt != "" || edits[0].PromptAuthoredKnown || edits[0].PromptSynthetic {
		t.Fatalf("legacy expanded prompt escaped edit provenance: %+v", edits[0])
	}
}

func appendAll(t *testing.T, sess *Session, messages ...provider.Message) {
	t.Helper()
	for _, m := range messages {
		if err := sess.AppendMessage(m); err != nil {
			t.Fatal(err)
		}
	}
}
