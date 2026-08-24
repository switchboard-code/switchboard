package provider

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestMessageRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		msg  Message
	}{
		{"text", UserText("hello")},
		{
			"assistant with thinking and tool use",
			Message{Role: RoleAssistant, Content: []Block{
				Thinking{Text: "consider the options", Signature: "sig-1"},
				Text{Text: "reading the file"},
				ToolUse{ID: "call_1", Name: "read", Input: json.RawMessage(`{"path":"main.go"}`)},
			}},
		},
		{
			"tool results share one message",
			Message{Role: RoleTool, Content: []Block{
				ToolResult{ToolUseID: "call_1", Name: "read", Content: "package main"},
				ToolResult{ToolUseID: "call_2", Name: "exec", Content: "exit status 1", IsError: true},
			}},
		},
		{
			"incomplete assistant message",
			Message{Role: RoleAssistant, Incomplete: true, DraftID: "session:draft:7", Content: []Block{Text{Text: "partial"}}},
		},
		{
			"continuity delivery metadata",
			Message{
				Role: RoleUser, Content: []Block{Text{Text: "continue"}},
				ContinuityRef: "0123456789abcdef0123456789abcdef",
			},
		},
		{
			"user steer provenance",
			Message{
				Role: RoleUser, Content: []Block{Text{Text: "[steer] stop the release"}},
				Authored: "stop the release", AuthoredKnown: true, Injected: true, UserSteer: true,
			},
		},
		{
			"synthetic opening provenance",
			Message{Role: RoleUser, Content: []Block{Text{Text: "continue"}}, Synthetic: true},
		},
		{
			"binary blocks",
			Message{Role: RoleUser, Content: []Block{
				Image{MediaType: "image/png", Data: []byte{0x89, 0x50, 0x4e, 0x47}},
				Document{MediaType: "application/pdf", Name: "spec.pdf", Data: []byte{0x25, 0x50}},
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.msg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got Message
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !reflect.DeepEqual(got, tc.msg) {
				t.Errorf("round trip changed the message\n got: %#v\nwant: %#v", got, tc.msg)
			}
		})
	}
}

func TestReplayRequestExcludesOnlyIncompleteAssistantMessages(t *testing.T) {
	partial := Message{
		Role:       RoleAssistant,
		Incomplete: true,
		Content:    []Block{Text{Text: "partial must stay durable"}},
	}
	flaggedUser := UserText("a non-assistant marker is not replay policy")
	flaggedUser.Incomplete = true
	plan := &CachePlan{RoutingKey: "stable-prefix"}
	req := Request{
		Messages: []Message{
			UserText("before"),
			partial,
			flaggedUser,
			UserText("after"),
		},
		CachePlan: plan,
	}

	got := ReplayRequest(req)
	if len(got.Messages) != 3 {
		t.Fatalf("projected messages = %+v, want only the incomplete assistant removed", got.Messages)
	}
	if got.Messages[0].Text() != "before" || got.Messages[1].Text() != flaggedUser.Text() || got.Messages[2].Text() != "after" {
		t.Fatalf("projected messages changed order or removed the wrong role: %+v", got.Messages)
	}
	if !got.Messages[1].Incomplete {
		t.Fatal("an incomplete marker on a non-assistant role was silently given assistant replay semantics")
	}
	if got.CachePlan != plan {
		t.Fatal("projection changed unrelated request metadata")
	}

	// The source request is the durable shape. Projecting it must never erase
	// the diagnostic record that a later resume or transcript reader needs.
	if len(req.Messages) != 4 || req.Messages[1].Text() != partial.Text() || !req.Messages[1].Incomplete {
		t.Fatalf("ReplayRequest mutated its input: %+v", req.Messages)
	}
	if twice := ReplayRequest(got); !reflect.DeepEqual(twice, got) {
		t.Fatalf("projection is not idempotent:\n once: %+v\ntwice: %+v", got, twice)
	}
}

// A block kind from a newer binary must fail loudly. Dropping it would hand the
// next request a conversation that silently lost a tool call.
func TestUnknownBlockKindIsRejected(t *testing.T) {
	var m Message
	err := json.Unmarshal([]byte(`{"role":"assistant","content":[{"kind":"hologram","data":{}}]}`), &m)
	if err == nil {
		t.Fatal("expected an error for an unknown block kind")
	}
	if !strings.Contains(err.Error(), "hologram") {
		t.Errorf("error should name the unknown kind, got: %v", err)
	}
}

func TestAuthoredTextExcludesOnlyStampedContinuityBlock(t *testing.T) {
	message := Message{
		Role:          RoleUser,
		ContinuityRef: strings.Repeat("a", 32),
		Content: []Block{
			Text{Text: "[continuity]\n\n"},
			Text{Text: "user prompt"},
			Image{MediaType: "image/png", Data: []byte("image")},
			Text{Text: " and detail"},
		},
	}
	if got := message.Text(); got != "[continuity]\n\nuser prompt and detail" {
		t.Fatalf("wire text = %q", got)
	}
	if got := message.AuthoredText(); got != "user prompt and detail" {
		t.Fatalf("authored text = %q", got)
	}
	message.ContinuityRef = ""
	if got := message.AuthoredText(); got != message.Text() {
		t.Fatalf("unstamped authored text = %q, wire = %q", got, message.Text())
	}
}

func TestAuthoredProjectionDoesNotGuessFromProviderExpandedContent(t *testing.T) {
	expanded := Message{
		Role:    RoleUser,
		Content: []Block{Text{Text: "inspect @notes.txt\n\nContents of notes.txt:\nsecret evidence"}},
	}.WithAuthoredText("inspect @notes.txt")
	if got, known := expanded.AuthoredProjection(); !known || got != "inspect @notes.txt" {
		t.Fatalf("authored projection = %q known=%v", got, known)
	}
	if got := expanded.Text(); !strings.Contains(got, "secret evidence") {
		t.Fatalf("wire text lost expansion: %q", got)
	}

	legacy := Message{Role: RoleUser, Content: []Block{Text{Text: "possibly expanded legacy content"}}}
	if got, known := legacy.AuthoredProjection(); known || got != "" {
		t.Fatalf("legacy projection was guessed: %q known=%v", got, known)
	}
	if got := legacy.AuthoredText(); got != legacy.Text() {
		t.Fatalf("legacy compatibility projection = %q, wire = %q", got, legacy.Text())
	}
}

func TestCloneMessageCanonicalizesPointersAndOwnsMutableBlocks(t *testing.T) {
	input := json.RawMessage(`{"path":"main.go"}`)
	image := []byte{1, 2, 3}
	document := []byte{4, 5, 6}
	message := Message{Role: RoleUser, Content: []Block{
		&Text{Text: "prompt"},
		&ToolUse{ID: "call", Name: "read", Input: input},
		&Image{MediaType: "image/png", Data: image},
		&Document{MediaType: "application/pdf", Data: document},
	}}
	cloned := CloneMessage(message)
	for i, block := range cloned.Content {
		if reflect.ValueOf(block).Kind() == reflect.Pointer {
			t.Fatalf("block %d remained a pointer: %T", i, block)
		}
	}
	input[0], image[0], document[0] = 'X', 9, 9
	if got := string(cloned.Content[1].(ToolUse).Input); got != `{"path":"main.go"}` {
		t.Fatalf("cloned tool input changed: %q", got)
	}
	if got := cloned.Content[2].(Image).Data[0]; got != 1 {
		t.Fatalf("cloned image changed: %d", got)
	}
	if got := cloned.Content[3].(Document).Data[0]; got != 4 {
		t.Fatalf("cloned document changed: %d", got)
	}
}

func TestRouteTargetIDIncludesReasoning(t *testing.T) {
	base := RouteTarget{Provider: "ollama", Surface: "local", ModelID: "qwen3.5:9b-mlx"}
	high := base
	high.Params.Reasoning = &Reasoning{Enabled: true, Effort: "high"}
	low := base
	low.Params.Reasoning = &Reasoning{Enabled: true, Effort: "low"}

	if base.ID() == high.ID() {
		t.Error("enabling reasoning must produce a different target: it changes cache identity")
	}
	if high.ID() == low.ID() {
		t.Error("effort levels must produce different targets")
	}
	if want := RouteTargetID("ollama/local/qwen3.5:9b-mlx"); base.ID() != want {
		t.Errorf("base ID = %q, want %q", base.ID(), want)
	}
}

func TestAPIErrorRetryClassification(t *testing.T) {
	for status, want := range map[int]bool{
		400: false, 401: false, 403: false, 404: false,
		408: true, 429: true, 500: true, 503: true,
	} {
		got := (&APIError{StatusCode: status}).Retryable()
		if got != want {
			t.Errorf("status %d: Retryable() = %v, want %v", status, got, want)
		}
	}
}
