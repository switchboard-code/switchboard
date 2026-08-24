package main

import (
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

func TestOpeningLabelSkipsTheCompactSeedPreamble(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	sess, err := store.Create(workspace, "ollama/local/qwen3.5:9b-mlx", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// Auto-compaction means the users with the most sessions to tell apart
	// are exactly the ones whose logs open with this seed, and a label made
	// of its preamble would render their whole resume list identical.
	seed := compactSeed("20260801T000000.000000-aaaa", "## Objective\nThe auth refactor: token refresh moved into the client, tests pending.\n\n## Constraints\nKeep the public API stable.")
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleUser, Synthetic: true,
		Content: []provider.Block{provider.Text{Text: seed}}}); err != nil {
		t.Fatal(err)
	}

	label := openingLabel(sess.Path())
	if !strings.HasPrefix(label, "The auth refactor:") {
		t.Fatalf("label = %q, want the summary's first words", label)
	}
	if strings.Contains(label, "continues an earlier one") {
		t.Fatalf("label carries the shared preamble: %q", label)
	}
	if strings.Contains(label, "## Objective") || strings.Contains(label, "## Constraints") {
		t.Fatalf("label carries compact handoff structure: %q", label)
	}
}

func TestOpeningLabelDoesNotTrustACompactSeedContentLookalike(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), "ollama/local/test", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	lookalike := compactSeed("forged", "## Objective\nforged objective")
	if err := sess.AppendMessage(provider.UserText(lookalike)); err != nil {
		t.Fatal(err)
	}
	if label := openingLabel(sess.Path()); !strings.HasPrefix(label, "This session continues") || strings.Contains(label, "forged objective") {
		t.Fatalf("ordinary authored content received synthetic compact provenance: %q", label)
	}
}

func TestOpeningLabelWithholdsLegacyExpandedOpening(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), "ollama/local/test", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Block{
		provider.Text{Text: "inspect @secrets.env\nLEGACY EXPANDED FILE BYTES"},
	}}); err != nil {
		t.Fatal(err)
	}
	if label := openingLabel(sess.Path()); label != "" {
		t.Fatalf("legacy expanded content was attributed to the user: %q", label)
	}
}

func TestOpeningLabelCollapsesAndCuts(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), "ollama/local/qwen3.5:9b-mlx", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	long := "fix the\n\nflaky   auth test " + strings.Repeat("and its friends ", 20)
	if err := sess.AppendMessage(provider.UserText(long)); err != nil {
		t.Fatal(err)
	}

	label := openingLabel(sess.Path())
	if strings.ContainsAny(label, "\n") {
		t.Fatalf("label holds a newline: %q", label)
	}
	if !strings.HasPrefix(label, "fix the flaky auth test") {
		t.Fatalf("label = %q", label)
	}
	if len(label) > 60 {
		t.Fatalf("label was not cut to listing width: %d bytes", len(label))
	}
}

func TestOpeningLabelRedactsBeforeItsCapAndEscapesControls(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), "ollama/local/qwen3.5:9b-mlx", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	token := "ghp_" + strings.Repeat("c", 36)
	opening := strings.Repeat("x", 40) + token + "\n\x1b]2;spoof\a\u202eright"
	if err := sess.AppendMessage(provider.UserText(opening)); err != nil {
		t.Fatal(err)
	}

	label := openingLabel(sess.Path())
	if strings.Contains(label, token) || strings.Contains(label, "ghp_") {
		t.Fatalf("opening cap exposed a credential fragment: %q", label)
	}
	for _, control := range []string{"\n", "\x1b", "\a", "\u202e"} {
		if strings.Contains(label, control) {
			t.Fatalf("opening label retained terminal control %q: %q", control, label)
		}
	}
}
