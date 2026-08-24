package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

func appendWrite(t *testing.T, sess *session.Session, callID, path string) {
	t.Helper()
	input, err := json.Marshal(map[string]any{"path": path, "content": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Block{provider.ToolUse{ID: callID, Name: "write", Input: input}},
	}); err != nil {
		t.Fatal(err)
	}
}

// The recap is one session's story: opening, route, files, races, bill,
// and the next actions, with the recorder's boundary stated.
func TestRecapTellsTheLastSessionsStory(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()

	sess, err := store.Create(workspace, "ollama/local/qwen3:4b", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("fix the flaky runner test")); err != nil {
		t.Fatal(err)
	}
	appendWrite(t, sess, "c1", "internal/run/runner.go")
	appendWrite(t, sess, "c2", "internal/run/runner_test.go")
	if err := sess.AppendRoute(session.Route{Tier: "t1", Target: "ollama/local/qwen3:4b", Outcome: "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendRace(session.Race{Prompt: "review", Outcome: "tie", Kept: "t1",
		A: session.RaceArm{Tier: "t1"}, B: session.RaceArm{Tier: "t2"}}); err != nil {
		t.Fatal(err)
	}
	id := sess.State().ID
	sess.Close()

	out := strings.Join(recapLines(store, workspace, "", ""), "\n")

	if !strings.Contains(out, id) || !strings.Contains(out, "fix the flaky runner test") {
		t.Errorf("the recap must name the session and its opening:\n%s", out)
	}
	if !strings.Contains(out, "every turn ran on t1") {
		t.Errorf("a one-rung session must say so:\n%s", out)
	}
	if !strings.Contains(out, "internal/run/runner.go") || !strings.Contains(out, "runner_test.go") {
		t.Errorf("the written files are missing:\n%s", out)
	}
	if !strings.Contains(out, "1 race: t1 kept ×1") {
		t.Errorf("the race verdict is missing:\n%s", out)
	}
	if !strings.Contains(out, "nothing billed") {
		t.Errorf("an unbilled session must say so, never $0.00:\n%s", out)
	}
	if !strings.Contains(out, "outside the record") {
		t.Errorf("the recorder's boundary is not stated:\n%s", out)
	}
	if !strings.Contains(out, "/resume "+id) || !strings.Contains(out, "/blame") {
		t.Errorf("the next actions are missing:\n%s", out)
	}
}

func TestRecapWithholdsLegacyProviderExpandedOpening(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Block{
		provider.Text{Text: "inspect @private.env\nLEGACY_FILE_AND_SHELL_BYTES"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	out := strings.Join(recapLines(store, workspace, "", ""), "\n")
	if strings.Contains(out, "@private.env") || strings.Contains(out, "LEGACY_FILE_AND_SHELL_BYTES") ||
		!strings.Contains(out, "authored wording unavailable") {
		t.Fatalf("legacy recap attributed expanded content to the user:\n%s", out)
	}
}

// Bare recap from inside a session looks past the session it is typed
// into: "where you left off" is the previous log, not this one.
func TestRecapSkipsTheSessionItRunsIn(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()

	old, err := store.Create(workspace, "ollama/local/qwen3:4b", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := old.AppendMessage(provider.UserText("the earlier work")); err != nil {
		t.Fatal(err)
	}
	oldID := old.State().ID
	old.Close()

	current, err := store.Create(workspace, "ollama/local/qwen3:4b", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := current.AppendMessage(provider.UserText("the running conversation")); err != nil {
		t.Fatal(err)
	}
	currentID := current.State().ID
	current.Close()

	out := strings.Join(recapLines(store, workspace, "", currentID), "\n")
	if !strings.Contains(out, oldID) || strings.Contains(out, currentID) {
		t.Errorf("bare recap must land on the previous session, not the running one:\n%s", out)
	}
}

func TestRecapCountsSharedTargetTierMoves(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	shared := provider.RouteTargetID("ollama/local/shared")
	sess, err := store.Create(workspace, shared, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("move across shared rungs")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendRoute(session.Route{
		Tier: "t1", Target: shared, Outcome: "escalated", Escalations: 2,
		EndedTier: "t2", EndedOn: shared,
	}); err != nil {
		t.Fatal(err)
	}
	id := sess.ID()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	out := strings.Join(recapLines(store, workspace, id, ""), "\n")
	if !strings.Contains(out, "first turn on t1, last on t2, 2 mid-turn moves") {
		t.Fatalf("recap flattened shared-target tier moves:\n%s", out)
	}
}

func TestRecapByIDAndTheEmptyCases(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()

	out := strings.Join(recapLines(store, workspace, "", ""), "\n")
	if !strings.Contains(out, "no earlier session") {
		t.Errorf("an empty history did not say so: %s", out)
	}

	sess, err := store.Create(workspace, "ollama/local/qwen3:4b", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("hello")); err != nil {
		t.Fatal(err)
	}
	id := sess.State().ID
	sess.Close()

	out = strings.Join(recapLines(store, workspace, id, ""), "\n")
	if !strings.Contains(out, id) {
		t.Errorf("recap by id must find the session:\n%s", out)
	}
	out = strings.Join(recapLines(store, workspace, "nope", ""), "\n")
	if !strings.Contains(out, "no session nope") {
		t.Errorf("an unknown id must be refused by name:\n%s", out)
	}
}
