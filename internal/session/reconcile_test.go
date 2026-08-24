package session

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/provider/anthropic"
	"github.com/switchboard-code/switchboard/internal/provider/kimi"
	"github.com/switchboard-code/switchboard/internal/provider/ollama"
	"github.com/switchboard-code/switchboard/internal/provider/openai"
	"github.com/switchboard-code/switchboard/internal/provider/openaicompat"
)

func TestReconcileInterruptedToolCallsAppendsMissingParallelResults(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	if err := sess.AppendMessage(provider.UserText("inspect and deploy")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
		provider.ToolUse{ID: "call-read", Name: "read", Input: json.RawMessage(`{"path":"main.go"}`)},
		provider.ToolUse{ID: "call-write", Name: "write", Input: json.RawMessage(`{"path":"main.go","content":"new"}`)},
		provider.ToolUse{ID: "call-release", Name: "exec", Input: json.RawMessage(`{"argv":["gh","release","create"]}`)},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleTool, Content: []provider.Block{
		provider.ToolResult{ToolUseID: "call-read", Name: "read", Content: "package main"},
	}}); err != nil {
		t.Fatal(err)
	}
	// Interrupted assistant output is durable for diagnosis but projected out
	// of provider replay; it must not hide the unresolved call batch behind it.
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleAssistant, Incomplete: true, Content: []provider.Block{
		provider.Text{Text: "cut off while tools were running"},
	}}); err != nil {
		t.Fatal(err)
	}

	added, err := sess.ReconcileInterruptedToolCalls()
	if err != nil || added != 2 {
		t.Fatalf("reconcile added=%d err=%v, want two results", added, err)
	}
	state := sess.State()
	if len(state.Messages) != 5 {
		t.Fatalf("messages=%d, want user/call/partial/incomplete/recovery", len(state.Messages))
	}
	recovery := state.Messages[4]
	if recovery.Role != provider.RoleTool || len(recovery.Content) != 2 {
		t.Fatalf("recovery message = %#v", recovery)
	}
	for i, wantID := range []string{"call-write", "call-release"} {
		result, ok := recovery.Content[i].(provider.ToolResult)
		if !ok || result.ToolUseID != wantID || !result.IsError {
			t.Fatalf("recovery result %d = %#v", i, recovery.Content[i])
		}
		for _, want := range []string{"outcome unknown", "Inspect the relevant state before retrying", "never repeat a non-idempotent effect"} {
			if !strings.Contains(result.Content, want) {
				t.Fatalf("recovery result omitted %q: %q", want, result.Content)
			}
		}
	}

	if added, err := sess.ReconcileInterruptedToolCalls(); err != nil || added != 0 || len(sess.State().Messages) != 5 {
		t.Fatalf("second reconcile added=%d err=%v messages=%d", added, err, len(sess.State().Messages))
	}
}

func TestReconcileInterruptedToolCallsIsDurablyIdempotent(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := appendDanglingToolCall(sess, "call-once"); err != nil {
		t.Fatal(err)
	}
	id := sess.ID()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.State().Messages) != 3 {
		reopened.Close()
		t.Fatalf("writable Open did not reconcile the replay tail: %#v", reopened.State().Messages)
	}
	before := len(reopened.State().Messages)
	if added, err := reopened.ReconcileInterruptedToolCalls(); err != nil || added != 0 {
		t.Fatalf("replayed reconcile added=%d err=%v", added, err)
	}
	if after := len(reopened.State().Messages); after != before {
		t.Fatalf("repeated resume appended another recovery: %d -> %d", before, after)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if len(again.State().Messages) != before {
		t.Fatalf("second writable resume appended another recovery: %d -> %d", before, len(again.State().Messages))
	}
}

func TestLatestReconcilesInterruptedToolCalls(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := appendDanglingToolCall(sess, "call-latest"); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	latest, err := store.Latest(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer latest.Close()
	assertSyntheticToolResult(t, latest.State(), "call-latest")
}

func TestForkReconcilesChildWithoutMutatingInterruptedSource(t *testing.T) {
	store, workspace := newStore(t)
	source, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := appendDanglingToolCall(source, "call-fork"); err != nil {
		t.Fatal(err)
	}

	child, err := store.ForkSession(source, len(source.State().Messages))
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	assertSyntheticToolResult(t, child.State(), "call-fork")
	if got := len(source.State().Messages); got != 2 {
		t.Fatalf("fork recovery mutated the live source: messages=%d, want 2", got)
	}
}

func TestOpenTruncatesTornToolResultThenReconciles(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := appendDanglingToolCall(sess, "call-torn-result"); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleTool, Content: []provider.Block{
		provider.ToolResult{ToolUseID: "call-torn-result", Name: "exec", Content: "completed"},
	}}); err != nil {
		t.Fatal(err)
	}
	id, path := sess.ID(), sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lastRecord := strings.LastIndexByte(string(data[:len(data)-1]), '\n') + 1
	if lastRecord <= 0 || lastRecord >= len(data) {
		t.Fatalf("could not locate final tool-result frame in %d bytes", len(data))
	}
	cut := lastRecord + (len(data)-lastRecord)/2
	if err := os.WriteFile(path, data[:cut], 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.TruncatedBytes() == 0 {
		t.Fatal("torn tool-result frame was not reported as truncated")
	}
	assertSyntheticToolResult(t, reopened.State(), "call-torn-result")
}

func TestPendingRaceRefusalDoesNotReconcileInterruptedToolCalls(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := appendDanglingToolCall(sess, "call-pending-race"); err != nil {
		t.Fatal(err)
	}
	if err := sess.MarkRaceBranchPending("origin"); err != nil {
		t.Fatal(err)
	}
	id, path := sess.ID(), sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.Open(id); !errors.Is(err, ErrRaceBranchPending) {
		t.Fatalf("open pending branch err=%v, want ErrRaceBranchPending", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("pending race refusal appended interrupted-call recovery")
	}
}

func TestReconcileInterruptedToolCallsOnlyRepairsTheReplayTail(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := appendDanglingToolCall(sess, "historical-call"); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("new task")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "done"}}}); err != nil {
		t.Fatal(err)
	}
	before := len(sess.State().Messages)
	if added, err := sess.ReconcileInterruptedToolCalls(); err != nil || added != 0 {
		t.Fatalf("historical batch reconcile added=%d err=%v", added, err)
	}
	if len(sess.State().Messages) != before {
		t.Fatal("reconciliation rewrote around later conversation history")
	}
}

func TestReconcileInterruptedToolCallsAppendFailurePublishesNothing(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := appendDanglingToolCall(sess, "call-fail"); err != nil {
		t.Fatal(err)
	}
	before := len(sess.State().Messages)
	// Closing the descriptor directly deterministically exercises the WAL
	// failure path without a second Close obscuring the error under test.
	if err := sess.f.Close(); err != nil {
		t.Fatal(err)
	}
	if added, err := sess.ReconcileInterruptedToolCalls(); added != 0 || !errors.Is(err, ErrSessionPoisoned) {
		t.Fatalf("failed reconcile added=%d err=%v", added, err)
	}
	if after := len(sess.State().Messages); after != before {
		t.Fatalf("failed recovery published state: %d -> %d messages", before, after)
	}
}

func TestReadOnlySessionSurfacesDoNotReconcile(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := appendDanglingToolCall(sess, "call-read-only"); err != nil {
		t.Fatal(err)
	}
	path := sess.Path()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if state, err := ReadState(path); err != nil || len(state.Messages) != 2 {
		t.Fatalf("read state messages=%d err=%v", len(state.Messages), err)
	}
	if infos, err := store.List(workspace); err != nil || len(infos) != 1 {
		t.Fatalf("list infos=%d err=%v", len(infos), err)
	}
	if all, err := store.ListAll(); err != nil || len(all[workspace]) != 1 {
		t.Fatalf("list all infos=%d err=%v", len(all[workspace]), err)
	}
	if timeline, err := ReadTimeline(path); err != nil || len(timeline) == 0 {
		t.Fatalf("timeline entries=%d err=%v", len(timeline), err)
	}
	if opening, err := ReadOpening(path); err != nil || opening == "" {
		t.Fatalf("opening=%q err=%v", opening, err)
	}
	if usages, err := ReadUsages(path); err != nil || len(usages) != 0 {
		t.Fatalf("usages=%d err=%v", len(usages), err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("a read-only session surface appended crash recovery")
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEveryAdapterRendersAReconciledTail(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := appendDanglingToolCall(sess, "call-render"); err != nil {
		t.Fatal(err)
	}
	if added, err := sess.ReconcileInterruptedToolCalls(); err != nil || added != 1 {
		t.Fatalf("reconcile added=%d err=%v", added, err)
	}
	req := provider.Request{
		Tools:    []provider.ToolDefinition{{Name: "exec", Description: "Run a command", Schema: json.RawMessage(`{"type":"object"}`)}},
		Messages: sess.State().Messages,
	}

	compatTarget, err := openaicompat.Target("generic", "model")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		client func(*http.Client) provider.Provider
		target provider.RouteTarget
	}{
		{"anthropic", func(h *http.Client) provider.Provider {
			return anthropic.New(anthropic.WithBaseURL("http://adapter.test"), anthropic.WithHTTPClient(h))
		}, anthropic.Target("claude-haiku-4-5")},
		{"kimi", func(h *http.Client) provider.Provider {
			return kimi.New("", anthropic.WithBaseURL("http://adapter.test"), anthropic.WithHTTPClient(h))
		}, kimi.Target("kimi-for-coding")},
		{"openai responses", func(h *http.Client) provider.Provider {
			return openai.NewResponses(openai.WithResponsesBaseURL("http://adapter.test"), openai.WithResponsesHTTPClient(h))
		}, openai.SubscriptionTarget("gpt-5.4-mini")},
		{"openai compatible", func(h *http.Client) provider.Provider {
			return openaicompat.NewFor("generic", openaicompat.Profile{BaseURL: "http://adapter.test", Tools: true}, openaicompat.WithHTTPClient(h))
		}, compatTarget},
		{"ollama", func(h *http.Client) provider.Provider {
			return ollama.New(ollama.WithBaseURL("http://adapter.test"), ollama.WithHTTPClient(h))
		}, ollama.Target("model")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			capture := &captureRequestTransport{}
			stream, err := tc.client(&http.Client{Transport: capture}).Stream(context.Background(), tc.target, req)
			if err != nil {
				t.Fatalf("adapter rejected reconciled request: %v", err)
			}
			if err := stream.Close(); err != nil {
				t.Fatal(err)
			}
			body := string(capture.body)
			for _, want := range []string{"call-render", "outcome unknown"} {
				if !strings.Contains(body, want) {
					t.Fatalf("wire request omitted %q: %s", want, body)
				}
			}
		})
	}
}

func appendDanglingToolCall(sess *Session, id string) error {
	if err := sess.AppendMessage(provider.UserText("run the release command")); err != nil {
		return err
	}
	return sess.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
		provider.ToolUse{ID: id, Name: "exec", Input: json.RawMessage(`{"argv":["release"]}`)},
	}})
}

func assertSyntheticToolResult(t *testing.T, state State, id string) {
	t.Helper()
	if len(state.Messages) != 3 {
		t.Fatalf("messages=%d, want user/call/recovery", len(state.Messages))
	}
	recovery := state.Messages[2]
	if recovery.Role != provider.RoleTool || len(recovery.Content) != 1 {
		t.Fatalf("recovery message=%#v", recovery)
	}
	result, ok := recovery.Content[0].(provider.ToolResult)
	if !ok || result.ToolUseID != id || !result.IsError || !strings.Contains(result.Content, "outcome unknown") {
		t.Fatalf("synthetic result=%#v", recovery.Content[0])
	}
}

type captureRequestTransport struct{ body []byte }

func (c *captureRequestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		c.body = body
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}, nil
}
