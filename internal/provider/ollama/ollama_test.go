package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func serve(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(WithBaseURL(srv.URL))
}

func serveBody(t *testing.T, body string) *Client {
	t.Helper()
	return serve(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, body)
	})
}

func drain(t *testing.T, s provider.EventStream) ([]provider.Event, error) {
	t.Helper()
	var events []provider.Event
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			return events, nil
		}
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
}

func TestBuildRequestEmitsOnlyAnExplicitNumPredict(t *testing.T) {
	decode := func(target provider.RouteTarget) *int {
		t.Helper()
		raw, err := New().buildRequest(target, provider.Request{}, true)
		if err != nil {
			t.Fatal(err)
		}
		var wire struct {
			Options *struct {
				NumPredict int `json:"num_predict"`
			} `json:"options"`
		}
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		if wire.Options == nil {
			return nil
		}
		return &wire.Options.NumPredict
	}

	target := Target("custom")
	if got := decode(target); got != nil {
		t.Fatalf("omitted max output emitted num_predict=%d", *got)
	}
	target.Params.MaxOutputTokens = 4_096
	if got := decode(target); got == nil || *got != 4_096 {
		t.Fatalf("explicit max output emitted num_predict=%v, want 4096", got)
	}
}

// The fixture is a verbatim capture from Ollama 0.32.9 running
// qwen3.5:9b-mlx, so the mapping is tested against what the server really
// sends rather than against the documented shape.
func TestStreamRecordedToolCall(t *testing.T) {
	fixture, err := os.ReadFile("testdata/tool_call_stream.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	c := serveBody(t, string(fixture))

	s, err := c.Stream(context.Background(), Target("qwen3.5:9b-mlx"), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	events, err := drain(t, s)
	if err != nil {
		t.Fatalf("draining stream: %v", err)
	}

	var thinking strings.Builder
	var toolUses []provider.ToolUse
	var done *provider.Event
	for i, ev := range events {
		switch ev.Type {
		case provider.EventThinkingDelta:
			if ev.Index != 0 {
				t.Errorf("event %d: contiguous thinking deltas must share a block index, got %d", i, ev.Index)
			}
			thinking.WriteString(ev.Text)
		case provider.EventToolUse:
			toolUses = append(toolUses, *ev.ToolUse)
		case provider.EventDone:
			done = &events[i]
		}
	}

	if !strings.Contains(thinking.String(), "main.go") {
		t.Errorf("reassembled thinking looks wrong: %q", thinking.String())
	}
	if len(toolUses) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(toolUses))
	}
	if toolUses[0].Name != "read" {
		t.Errorf("tool name = %q, want read", toolUses[0].Name)
	}
	if toolUses[0].ID != "call_hquris0t" {
		t.Errorf("tool ID = %q, want the server-supplied call_hquris0t", toolUses[0].ID)
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(toolUses[0].Input, &args); err != nil {
		t.Fatalf("tool arguments did not survive as JSON: %v", err)
	}
	if args.Path != "main.go" {
		t.Errorf("args.path = %q, want main.go", args.Path)
	}

	if done == nil {
		t.Fatal("stream produced no done event")
	}
	// The server reports done_reason "stop" on this turn even though it ended in
	// a tool call. Reporting end_turn would strand the call unexecuted.
	if done.StopReason != provider.StopToolUse {
		t.Errorf("StopReason = %q, want tool_use", done.StopReason)
	}
	if done.Usage.InputTokens != 283 || done.Usage.OutputTokens != 57 {
		t.Errorf("usage = %+v, want 283 in / 57 out", done.Usage)
	}
	if done.Usage.CacheReadTokens != 0 || done.Usage.CacheWriteTokens != 0 {
		t.Error("Ollama reports no cache accounting; claiming any would mislead the cost model")
	}
}

func TestStreamSeparatesThinkingFromText(t *testing.T) {
	c := serveBody(t, strings.Join([]string{
		`{"message":{"role":"assistant","content":"","thinking":"weigh"},"done":false}`,
		`{"message":{"role":"assistant","content":"","thinking":" it"},"done":false}`,
		`{"message":{"role":"assistant","content":"the "},"done":false}`,
		`{"message":{"role":"assistant","content":"answer"},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":10,"eval_count":4}`,
	}, "\n"))

	s, err := c.Stream(context.Background(), Target("m"), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	events, err := drain(t, s)
	if err != nil {
		t.Fatal(err)
	}

	byIndex := map[int]string{}
	for _, ev := range events {
		if ev.Type == provider.EventThinkingDelta || ev.Type == provider.EventTextDelta {
			byIndex[ev.Index] += ev.Text
		}
	}
	if byIndex[0] != "weigh it" {
		t.Errorf("block 0 = %q, want the reassembled thinking", byIndex[0])
	}
	if byIndex[1] != "the answer" {
		t.Errorf("block 1 = %q, want the reassembled text", byIndex[1])
	}
	if events[len(events)-1].StopReason != provider.StopEndTurn {
		t.Errorf("StopReason = %q, want end_turn", events[len(events)-1].StopReason)
	}
}

func TestStreamTruncatedIsDistinguishable(t *testing.T) {
	c := serveBody(t, `{"message":{"role":"assistant","content":"half a th"},"done":false}`+"\n")

	s, err := c.Stream(context.Background(), Target("m"), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	events, err := drain(t, s)
	if !errors.Is(err, provider.ErrStreamIncomplete) {
		t.Fatalf("err = %v, want ErrStreamIncomplete", err)
	}
	// The partial output must survive, since the loop records it as an
	// incomplete message rather than discarding the turn.
	if len(events) != 1 || events[0].Text != "half a th" {
		t.Errorf("partial content was lost: %+v", events)
	}
}

func TestStreamRefusesOversizeFrameWithoutDelimiter(t *testing.T) {
	body := `{"message":{"role":"assistant","content":"` + strings.Repeat("x", maxStreamFrameBytes) + `"},"done":false}`
	c := serveBody(t, body)

	s, err := c.Stream(context.Background(), Target("m"), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	_, err = drain(t, s)
	var protocolErr *provider.ProtocolError
	if !errors.As(err, &protocolErr) || !strings.Contains(err.Error(), "frame limit") {
		t.Fatalf("err = %v, want bounded frame ProtocolError", err)
	}
}

func TestStreamAcceptsLargeFramesWithoutATotalOutputCap(t *testing.T) {
	part := strings.Repeat("x", maxStreamFrameBytes/2)
	body := strings.Join([]string{
		`{"message":{"role":"assistant","content":"` + part + `"},"done":false}`,
		`{"message":{"role":"assistant","content":"` + part + `"},"done":false}`,
		`{"message":{"role":"assistant","content":"` + part + `"},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done":true,"done_reason":"stop"}`,
	}, "\n")
	c := serveBody(t, body)

	s, err := c.Stream(context.Background(), Target("m"), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	events, err := drain(t, s)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, event := range events {
		total += len(event.Text)
	}
	if total != 3*len(part) || total <= maxStreamFrameBytes {
		t.Fatalf("total output = %d, want %d (> per-frame cap)", total, 3*len(part))
	}
}

func TestStreamAcceptsExactFrameBoundary(t *testing.T) {
	prefix := `{"message":{"role":"assistant","content":"`
	suffix := `"},"done":true,"done_reason":"stop"}`
	body := prefix + strings.Repeat("x", maxStreamFrameBytes-len(prefix)-len(suffix)) + suffix
	c := serveBody(t, body)

	s, err := c.Stream(context.Background(), Target("m"), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := drain(t, s); err != nil {
		t.Fatalf("exact-boundary frame: %v", err)
	}
}

func TestModelsBoundsBodyAndRejectsUnsafeIDs(t *testing.T) {
	t.Run("oversize body", func(t *testing.T) {
		c := serveBody(t, strings.Repeat(" ", provider.MaxProviderJSONBodyBytes+1))
		if _, err := c.Models(context.Background()); err == nil || !strings.Contains(err.Error(), "response limit") {
			t.Fatalf("err = %v, want response-limit refusal", err)
		}
	})

	t.Run("credential shaped id", func(t *testing.T) {
		token := "ghp_" + strings.Repeat("A", 40)
		c := serveBody(t, `{"models":[{"name":"model-`+token+`"}]}`)
		_, err := c.Models(context.Background())
		if err == nil || strings.Contains(err.Error(), token) {
			t.Fatalf("unsafe model ID was accepted or echoed: %v", err)
		}
	})
}

func TestStreamMidStreamErrorIsNotRetried(t *testing.T) {
	c := serveBody(t, strings.Join([]string{
		`{"message":{"role":"assistant","content":"start"},"done":false}`,
		`{"error":"unable to load model"}`,
	}, "\n"))

	s, err := c.Stream(context.Background(), Target("m"), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	_, err = drain(t, s)
	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *provider.APIError", err)
	}
	if !strings.Contains(apiErr.Body, "unable to load model") {
		t.Errorf("error lost the server's message: %q", apiErr.Body)
	}
	if apiErr.Retryable() {
		t.Error("an in-band stream error carries no status to justify a retry")
	}
}

func TestStreamRejectsMalformedToolCall(t *testing.T) {
	c := serveBody(t, `{"message":{"role":"assistant","tool_calls":[{"function":{"arguments":{}}}]},"done":false}`+"\n")

	s, err := c.Stream(context.Background(), Target("m"), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := drain(t, s); err == nil {
		t.Fatal("a tool call with no function name must not be accepted")
	}
}

func TestHTTPErrorCarriesServerMessage(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":"model 'nope:1b' not found"}`)
	})

	_, err := c.Stream(context.Background(), Target("nope:1b"), provider.Request{})
	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *provider.APIError", err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", apiErr.StatusCode)
	}
	if apiErr.Body != "model 'nope:1b' not found" {
		t.Errorf("body = %q, want the unwrapped server message", apiErr.Body)
	}
}

func TestHTTPErrorCredentialBodyIsRedactedBeforeItsCap(t *testing.T) {
	token := "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for name, body := range map[string]string{
		"complete":       `{"error":"rejected ` + token + `"}`,
		"cross-boundary": strings.Repeat("x", provider.MaxAPIErrorBodyBytes-len(token)+1) + token,
	} {
		t.Run(name, func(t *testing.T) {
			c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, body)
			})
			_, err := c.Stream(context.Background(), Target("m"), provider.Request{})
			var apiErr *provider.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v, want APIError", err)
			}
			if strings.Contains(apiErr.Body, token) || strings.Contains(apiErr.Body, "ghp_") {
				t.Fatalf("API error body exposed a credential fragment: %q", apiErr.Body)
			}
		})
	}
}

func TestBuildRequestShape(t *testing.T) {
	req := provider.Request{
		System: []provider.Block{provider.Text{Text: "be terse"}},
		Tools: []provider.ToolDefinition{{
			Name:        "read",
			Description: "Read a file",
			Schema:      json.RawMessage(`{"type":"object"}`),
		}},
		Messages: []provider.Message{
			provider.UserText("read main.go"),
			{Role: provider.RoleAssistant, Content: []provider.Block{
				provider.Thinking{Text: "need the file"},
				provider.ToolUse{ID: "call_1", Name: "read", Input: json.RawMessage(`{"path":"main.go"}`)},
			}},
			// One canonical tool message holding two results, which is how the
			// loop appends them (§10.1).
			{Role: provider.RoleTool, Content: []provider.Block{
				provider.ToolResult{ToolUseID: "call_1", Name: "read", Content: "package main"},
				provider.ToolResult{ToolUseID: "call_2", Name: "exec", Content: "boom", IsError: true},
			}},
			// An interrupted message must never be replayed as a finished turn.
			{Role: provider.RoleAssistant, Incomplete: true, Content: []provider.Block{
				provider.Text{Text: "cut off"},
			}},
		},
	}

	raw, err := New().buildRequest(Target("m"), req, true)
	if err != nil {
		t.Fatal(err)
	}
	var got chatRequest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	roles := make([]string, len(got.Messages))
	for i, m := range got.Messages {
		roles[i] = m.Role
	}
	want := []string{"system", "user", "assistant", "tool", "tool"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
	if got.Messages[0].Content != "be terse" {
		t.Errorf("system content = %q", got.Messages[0].Content)
	}
	if got.Messages[3].ToolName != "read" || got.Messages[4].ToolName != "exec" {
		t.Errorf("each result must name its tool, got %q and %q", got.Messages[3].ToolName, got.Messages[4].ToolName)
	}
	if n := len(got.Messages[2].ToolCalls); n != 1 {
		t.Fatalf("assistant tool calls = %d, want 1", n)
	}
	if string(got.Messages[2].ToolCalls[0].Function.Arguments) != `{"path":"main.go"}` {
		t.Errorf("tool arguments must stay a JSON object, got %s", got.Messages[2].ToolCalls[0].Function.Arguments)
	}
	if got.Messages[2].Thinking != "need the file" {
		t.Errorf("thinking = %q, want it echoed back for replay", got.Messages[2].Thinking)
	}
	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "read" {
		t.Errorf("tools = %+v", got.Tools)
	}
}

func TestReplayExcludesIncompleteAssistantFromOllamaWireAndEstimate(t *testing.T) {
	const partial = "PARTIAL MUST NOT REPLAY"
	base := provider.Request{Messages: []provider.Message{
		provider.UserText("before"),
		provider.UserText("after"),
	}}
	withPartial := base
	withPartial.Messages = []provider.Message{
		base.Messages[0],
		{Role: provider.RoleAssistant, Incomplete: true, Content: []provider.Block{provider.Text{Text: partial}}},
		base.Messages[1],
	}
	client := New()
	target := Target("m")
	body, err := client.buildRequest(target, withPartial, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), partial) {
		t.Fatalf("incomplete assistant reached Ollama wire:\n%s", body)
	}
	if !strings.Contains(string(body), "before") || !strings.Contains(string(body), "after") {
		t.Fatalf("projection removed surrounding durable messages:\n%s", body)
	}
	got, err := client.CountTokens(context.Background(), target, withPartial)
	if err != nil {
		t.Fatal(err)
	}
	want, err := client.CountTokens(context.Background(), target, base)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("estimate with partial = %+v, want %+v", got, want)
	}
}

func TestContinuityBlockKeepsBlankLineBeforeFlattenedPrompt(t *testing.T) {
	raw, err := New().buildRequest(Target("m"), provider.Request{Messages: []provider.Message{{
		Role: provider.RoleUser,
		Content: []provider.Block{
			provider.Text{Text: "[continuity capsule]\n\n"},
			provider.Text{Text: "continue with the fix"},
		},
		ContinuityRef: "0123456789abcdef0123456789abcdef",
	}}}, true)
	if err != nil {
		t.Fatal(err)
	}
	var decoded chatRequest
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Messages) != 1 || decoded.Messages[0].Content != "[continuity capsule]\n\ncontinue with the fix" {
		t.Fatalf("flattened continuity opening = %+v", decoded.Messages)
	}
	if strings.Contains(string(raw), "continuity_ref") {
		t.Fatal("session-only continuity metadata reached the Ollama wire")
	}
}

// Two calls to the same tool in one turn are only distinguishable by ID, so the
// result messages must carry it.
func TestBuildRequestCorrelatesRepeatedToolCalls(t *testing.T) {
	req := provider.Request{Messages: []provider.Message{{
		Role: provider.RoleTool,
		Content: []provider.Block{
			provider.ToolResult{ToolUseID: "call_a", Name: "read", Content: "first file"},
			provider.ToolResult{ToolUseID: "call_b", Name: "read", Content: "second file"},
		},
	}}}

	raw, err := New().buildRequest(Target("m"), req, true)
	if err != nil {
		t.Fatal(err)
	}
	var got chatRequest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("got %d messages, want one per result", len(got.Messages))
	}
	if got.Messages[0].ToolCallID != "call_a" || got.Messages[1].ToolCallID != "call_b" {
		t.Errorf("results not correlated by ID: %q and %q",
			got.Messages[0].ToolCallID, got.Messages[1].ToolCallID)
	}
}

func TestBuildRequestReasoningMapping(t *testing.T) {
	think := func(enabled bool, effort string) any {
		target := Target("m")
		target.Params.Reasoning = &provider.Reasoning{Enabled: enabled, Effort: effort}
		raw, err := New().buildRequest(target, provider.Request{}, true)
		if err != nil {
			return err
		}
		var got chatRequest
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		return got.Think
	}

	if v := think(true, ""); v != true {
		t.Errorf("enabled with no effort = %v, want true", v)
	}
	if v := think(false, ""); v != false {
		t.Errorf("disabled = %v, want false", v)
	}
	if v := think(true, "high"); v != "high" {
		t.Errorf("effort high = %v", v)
	}

	// The server rejects anything outside its own documented set; catching it
	// here turns a mid-turn 400 into an error raised before the request is sent.
	err, ok := think(true, "ludicrous").(error)
	if !ok {
		t.Fatal("an unsupported effort must be an error, not a silent passthrough")
	}
	var capErr *provider.CapabilityError
	if !errors.As(err, &capErr) {
		t.Errorf("err = %v, want *provider.CapabilityError", err)
	}
}

func TestBuildRequestRejectsUnsupportedSemantics(t *testing.T) {
	t.Run("cache plan", func(t *testing.T) {
		req := provider.Request{CachePlan: &provider.CachePlan{
			Breakpoints: []provider.Breakpoint{{}},
		}}
		_, err := New().buildRequest(Target("m"), req, true)
		var capErr *provider.CapabilityError
		if !errors.As(err, &capErr) {
			t.Fatalf("err = %v, want *provider.CapabilityError", err)
		}
	})

	t.Run("document block", func(t *testing.T) {
		req := provider.Request{Messages: []provider.Message{{
			Role:    provider.RoleUser,
			Content: []provider.Block{provider.Document{MediaType: "application/pdf"}},
		}}}
		_, err := New().buildRequest(Target("m"), req, true)
		var capErr *provider.CapabilityError
		if !errors.As(err, &capErr) {
			t.Fatalf("err = %v, want *provider.CapabilityError", err)
		}
	})
}

func TestProbeReportsCapabilities(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			io.WriteString(w, `{"models":[{"name":"qwen3.5:9b-mlx"}]}`)
		case "/api/show":
			io.WriteString(w, `{"capabilities":["completion","vision","thinking","tools"]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	res, err := c.Probe(context.Background(), Target("qwen3.5:9b-mlx"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reachable || !res.ModelPresent || !res.VisionKnown || !res.Vision {
		t.Errorf("probe = %+v", res)
	}
	if res.Tools == provider.ToolsNone {
		t.Error("model advertises tools but the probe reported none")
	}

	missing, err := c.Probe(context.Background(), Target("absent:1b"))
	if err != nil {
		t.Fatal(err)
	}
	if missing.ModelPresent {
		t.Error("a model that is not pulled must not report present")
	}
	if !missing.Reachable {
		t.Error("the server answered, so it is reachable")
	}
}

func TestProbeUnreachableServer(t *testing.T) {
	// Port 1 on loopback refuses connections without a DNS round trip.
	c := New(WithBaseURL("http://127.0.0.1:1"))
	res, err := c.Probe(context.Background(), Target("m"))
	if err != nil {
		t.Fatalf("an unreachable server is a probe result, not a hard error: %v", err)
	}
	if res.Reachable {
		t.Error("Reachable must be false when nothing answered")
	}
}

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"":                       defaultHost,
		"localhost:11434":        "http://localhost:11434",
		"http://box:11434/":      "http://box:11434",
		"https://remote.example": "https://remote.example",
	} {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContextCancellationStopsStream(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"message":{"content":"one"},"done":false}`+"\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	s, err := c.Stream(ctx, Target("m"), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.Next(); err != nil {
		t.Fatalf("first event: %v", err)
	}
	cancel()
	if _, err := s.Next(); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// An unset flag passed straight through must not overwrite what the
// environment already said. The wiring reads the flag first and hands the
// result on regardless, so an empty address has to mean "leave it alone" —
// otherwise OLLAMA_HOST is silently ignored on every default launch.
func TestAnEmptyBaseURLLeavesTheEnvironmentAlone(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "http://ollama.example:11434")

	if got := New(WithBaseURL("")).BaseURL(); got != "http://ollama.example:11434" {
		t.Fatalf("BaseURL() = %q, want the environment's address", got)
	}
	if got := New(WithBaseURL("  ")).BaseURL(); got != "http://ollama.example:11434" {
		t.Fatalf("blank BaseURL() = %q, want the environment's address", got)
	}
	if got := New(WithBaseURL("box:11434")).BaseURL(); got != "http://box:11434" {
		t.Fatalf("BaseURL() = %q, want the given address normalized", got)
	}
}
