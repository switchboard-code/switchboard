package main

import (
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

func TestExportPrintsTheTimelineNotJustTheWords(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()

	sess, err := store.Create(workspace, "ollama/local/qwen3:4b", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendRoute(session.Route{Tier: "t1", Source: "start", Rationale: "lowest rung"}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("fix the flaky runner test")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
		provider.Text{Text: "Looking at the runner."},
		provider.ToolUse{ID: "c1", Name: "read", Input: []byte(`{}`)},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendRace(session.Race{Prompt: "review", Outcome: "tie", Kept: "t1",
		A: session.RaceArm{Tier: "t1"}, B: session.RaceArm{Tier: "t2"}}); err != nil {
		t.Fatal(err)
	}
	id := sess.State().ID
	sess.Close()

	var b strings.Builder
	if err := runExportCLI(&b, store, workspace, ""); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"# Switchboard session " + id,
		"## User",
		"fix the flaky runner test",
		"## Assistant",
		"Looking at the runner.",
		"*[tool: read]*",
		"> route: t1 via start (lowest rung)",
		"> race: t1 vs t2 — tie, continued on t1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("export missing %q:\n%s", want, out)
		}
	}
	// The route opened the session, so it must precede the words it routed.
	if strings.Index(out, "> route:") > strings.Index(out, "## User") {
		t.Errorf("the route annotation is not where it happened:\n%s", out)
	}
}

func TestExportNamesTheMissingSession(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()

	var b strings.Builder
	if err := runExportCLI(&b, store, workspace, ""); err == nil ||
		!strings.Contains(err.Error(), "no session recorded") {
		t.Errorf("an empty workspace must say so, got %v", err)
	}

	sess, err := store.Create(workspace, "ollama/local/qwen3:4b", "test")
	if err != nil {
		t.Fatal(err)
	}
	sess.Close()
	if err := runExportCLI(&b, store, workspace, "nope"); err == nil ||
		!strings.Contains(err.Error(), "sb find") {
		t.Errorf("a wrong id should point at sb find, got %v", err)
	}
}

func TestExportDisplaysParameterizedRouteTargets(t *testing.T) {
	target := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"}
	target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}
	out := exportMarkdown(session.State{Target: string(target.ID())}, []session.Timeline{{Route: &session.Route{
		Tier: "t2", Source: "signal", Rationale: "harder work", Escalations: 1, EndedTier: "t3", EndedOn: target.ID(),
	}}})
	if !strings.Contains(out, "ended on t3") || !strings.Contains(out, target.ModelID) || !strings.Contains(out, "think:high") || strings.Contains(out, "rt2:") {
		t.Fatalf("parameterized export target is opaque: %q", out)
	}
}

func TestExportRedactsCredentialsAndEscapesTerminalControls(t *testing.T) {
	token := "ghp_" + strings.Repeat("a", 36)
	unsafe := "line\x1b]2;spoof\a\u202eright " + token
	out := exportMarkdown(session.State{Workspace: unsafe}, []session.Timeline{{
		Message: func() *provider.Message {
			message := provider.UserText(unsafe)
			return &message
		}(),
	}})
	if strings.Contains(out, token) || !strings.Contains(out, "[redacted: a GitHub token]") {
		t.Fatalf("export exposed a credential: %q", out)
	}
	for _, control := range []string{"\x1b", "\a", "\u202e"} {
		if strings.Contains(out, control) {
			t.Fatalf("export retained terminal control %q: %q", control, out)
		}
	}
	for _, visible := range []string{`\x1b`, `\x07`, `\u202e`} {
		if !strings.Contains(out, visible) {
			t.Fatalf("export did not visibly escape %q: %q", visible, out)
		}
	}
}
