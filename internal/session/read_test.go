package session

import (
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func TestReadOpeningStopsAtTheFirstUserWords(t *testing.T) {
	store, workspace := newStore(t)

	sess, err := store.Create(workspace, "ollama/local/qwen3.5:9b-mlx", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// A user message with no text — a bare screenshot — is not the user
	// speaking, so the opening is the first message that carries words.
	if err := sess.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Block{provider.Image{MediaType: "image/png", Data: []byte{1}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("fix the flaky auth test")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Block{provider.Text{Text: "looking at it"}},
	}); err != nil {
		t.Fatal(err)
	}

	// The session is still open for appending: labelling a listing must not
	// need the lock, same posture as ReadState.
	opening, err := ReadOpening(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if opening != "fix the flaky auth test" {
		t.Fatalf("opening = %q, want the first user words", opening)
	}
}

func TestReadOpeningWithholdsLegacyProviderExpandedContent(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	const expanded = "inspect @private.env\nAPI_TOKEN_FROM_EXPANDED_FILE"
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Block{
		provider.Text{Text: expanded},
	}}); err != nil {
		t.Fatal(err)
	}

	opening, err := ReadOpening(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if opening != "" || strings.Contains(opening, "API_TOKEN") {
		t.Fatalf("legacy provider expansion escaped as authored opening: %q", opening)
	}
	summary, err := ReadOpeningSummary(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Found || summary.AuthoredKnown || summary.Text != "" {
		t.Fatalf("legacy opening provenance = %+v", summary)
	}
}

func TestReadUsagesKeepsTheCallsApart(t *testing.T) {
	store, workspace := newStore(t)

	sess, err := store.Create(workspace, "ollama/local/qwen3.5:9b-mlx", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	for i, in := range []int{1_000, 250_000} {
		if err := sess.AppendUsage(Usage{
			Target: "a/b/c",
			Usage:  provider.Usage{InputTokens: in, OutputTokens: 10 * (i + 1)},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Per call, in order, while the session is open: counterfactual pricing
	// bands by the size of one call, so the sum replay keeps is not enough.
	usages, err := ReadUsages(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(usages) != 2 {
		t.Fatalf("got %d usage records, want 2", len(usages))
	}
	if usages[0].Usage.InputTokens != 1_000 || usages[1].Usage.InputTokens != 250_000 {
		t.Fatalf("calls out of order or merged: %+v", usages)
	}
}

func TestReadOpeningOnASessionWithNoUserTurn(t *testing.T) {
	store, workspace := newStore(t)

	sess, err := store.Create(workspace, "ollama/local/qwen3.5:9b-mlx", "rev")
	if err != nil {
		t.Fatal(err)
	}
	path := sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	opening, err := ReadOpening(path)
	if err != nil {
		t.Fatalf("an empty log is a session, not a failure: %v", err)
	}
	if opening != "" {
		t.Fatalf("opening = %q, want empty", opening)
	}
}

// The timeline is the log as a document: conversation and the decisions
// that rode beside it, in the order written, with accounting left to State.
func TestReadTimelineInterleavesInOrder(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), "ollama/local/test:7b", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	sess.AppendMessage(provider.UserText("first ask"))
	sess.AppendRoute(Route{Tier: "t1", Source: "heuristic", Rationale: "short prompt"})
	sess.AppendMessage(provider.Message{Role: provider.RoleAssistant,
		Content: []provider.Block{provider.Text{Text: "the answer"}}})
	sess.AppendNote("warn", "a fallback served t2")
	sess.AppendUsage(Usage{Target: "ollama/local/test:7b"})

	timeline, err := ReadTimeline(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(timeline) != 4 {
		t.Fatalf("got %d events, want 4 (usage is accounting, not timeline): %+v", len(timeline), timeline)
	}
	if timeline[0].Message == nil || timeline[1].Route == nil || timeline[2].Message == nil || timeline[3].Note == nil {
		t.Fatalf("events out of order or mistyped: %+v", timeline)
	}
	if timeline[1].Route.Rationale != "short prompt" {
		t.Fatalf("route payload lost: %+v", timeline[1].Route)
	}
}
