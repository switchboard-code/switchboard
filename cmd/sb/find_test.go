package main

import (
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

// /find greps what was said. It matches the conversation — user and
// assistant text — case-insensitively, hands back the id /resume takes,
// and stays quiet about tool payloads, because the question is about the
// conversation.
func TestFindSearchesTheConversation(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	target := provider.RouteTargetID("ollama/local/test:7b")

	hit, err := store.Create(workspace, target, "test")
	if err != nil {
		t.Fatal(err)
	}
	hit.AppendMessage(provider.UserText("make the runner test deterministic"))
	hit.AppendMessage(provider.Message{Role: provider.RoleAssistant,
		Content: []provider.Block{provider.Text{Text: "The Runner race is a wait on the process group."}}})
	hitID := hit.State().ID
	hit.Close()

	miss, err := store.Create(workspace, target, "test")
	if err != nil {
		t.Fatal(err)
	}
	miss.AppendMessage(provider.UserText("rename the config field"))
	miss.Close()

	out := strings.Join(findLines(store, workspace, "runner"), "\n")
	if !strings.Contains(out, hitID) {
		t.Fatalf("the matching session's id is absent:\n%s", out)
	}
	if !strings.Contains(out, "2 matches") {
		t.Fatalf("case-insensitive matching missed a hit:\n%s", out)
	}
	if !strings.Contains(out, "/resume") {
		t.Fatalf("the way to pick a session up is not stated:\n%s", out)
	}
	if strings.Contains(out, miss.State().ID) {
		t.Fatalf("a session that never said it was listed:\n%s", out)
	}

	none := strings.Join(findLines(store, workspace, "zeppelin"), "\n")
	if !strings.Contains(none, "nothing in 2 sessions") {
		t.Fatalf("an empty result did not say so:\n%s", none)
	}
}

func TestFindSearchesOnlyProvenUserWordingAndModelAnswers(t *testing.T) {
	legacy := provider.Message{Role: provider.RoleUser, Content: []provider.Block{
		provider.Text{Text: "inspect @config\nLEGACY_FILE_SENTINEL"},
	}}
	modern := provider.Message{Role: provider.RoleUser, AuthoredKnown: true, Authored: "inspect @config",
		Content: []provider.Block{provider.Text{Text: "inspect @config\nMODERN_FILE_SENTINEL\nshell MODERN_SHELL_SENTINEL"}}}
	injected := provider.UserText("MACHINE_INJECTION_SENTINEL")
	injected.Injected = true
	steer := provider.UserText("USER_STEER_SENTINEL")
	steer.Injected = true
	steer.UserSteer = true

	messages := []provider.Message{legacy, modern, injected, steer, {
		Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "MODEL_ANSWER_SENTINEL"}},
	}}
	for _, query := range []string{"legacy_file_sentinel", "modern_file_sentinel", "modern_shell_sentinel", "machine_injection_sentinel"} {
		if hits, snippet := searchMessages(messages, query); hits != 0 || snippet != "" {
			t.Errorf("unproven query %q matched %d: %q", query, hits, snippet)
		}
	}
	for _, query := range []string{"inspect @config", "user_steer_sentinel", "model_answer_sentinel"} {
		if hits, _ := searchMessages(messages, query); hits != 1 {
			t.Errorf("proven query %q matched %d, want 1", query, hits)
		}
	}
}

func TestFindLabelsLegacyOpeningWithoutRenderingExpandedBytes(t *testing.T) {
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
		provider.Text{Text: "@secret.env\nLEGACY_EXPANDED_OPENING"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
		provider.Text{Text: "the model discussed NEEDLE"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	out := strings.Join(findLines(store, workspace, "needle"), "\n")
	if strings.Contains(out, "LEGACY_EXPANDED_OPENING") || strings.Contains(out, "@secret.env") ||
		!strings.Contains(out, "authored wording unavailable") {
		t.Fatalf("legacy find label crossed the authorship boundary:\n%s", out)
	}
}

// The all-form answers "which project was that": matches grouped under the
// workspace each log's own header names.
func TestFindAllSpansWorkspaces(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := provider.RouteTargetID("ollama/local/test:7b")
	wsA, wsB := t.TempDir(), t.TempDir()
	for i, ws := range []string{wsA, wsB} {
		sess, err := store.Create(ws, target, "test")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			sess.AppendMessage(provider.UserText("the zeppelin design goes here"))
		} else {
			sess.AppendMessage(provider.UserText("nothing relevant"))
		}
		sess.Close()
	}

	out := strings.Join(findAllLines(store, "zeppelin"), "\n")
	if !strings.Contains(out, wsA) {
		t.Fatalf("the matching workspace is absent:\n%s", out)
	}
	if strings.Contains(out, wsB) {
		t.Fatalf("a workspace with no match was listed:\n%s", out)
	}
	if !strings.Contains(out, "-workspace") {
		t.Fatalf("the way to pick a session up across workspaces is not stated:\n%s", out)
	}
}
