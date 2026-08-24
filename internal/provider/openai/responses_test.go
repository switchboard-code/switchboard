package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func serveResponses(t *testing.T, handler http.HandlerFunc) *ResponsesClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewResponses(WithResponsesBaseURL(srv.URL))
}

func serveResponsesFixture(t *testing.T, name string) *ResponsesClient {
	t.Helper()
	body, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return serveResponses(t, func(w http.ResponseWriter, _ *http.Request) { w.Write(body) })
}

func drainResponses(t *testing.T, c *ResponsesClient, req provider.Request) ([]provider.Event, error) {
	t.Helper()
	s, err := c.Stream(context.Background(), SubscriptionTarget("gpt-5.4-mini"), req)
	if err != nil {
		return nil, err
	}
	defer s.Close()

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

func terminal(t *testing.T, events []provider.Event) provider.Event {
	t.Helper()
	for _, ev := range events {
		if ev.Type == provider.EventDone {
			return ev
		}
	}
	t.Fatal("no terminal event")
	return provider.Event{}
}

func TestResponsesBuildRequestEmitsOnlyAnExplicitMaxOutputTokens(t *testing.T) {
	decode := func(target provider.RouteTarget) (int, bool) {
		t.Helper()
		raw, err := NewResponses().buildRequest(target, provider.Request{})
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		encoded, ok := wire["max_output_tokens"]
		if !ok {
			return 0, false
		}
		var value int
		if err := json.Unmarshal(encoded, &value); err != nil {
			t.Fatal(err)
		}
		return value, true
	}

	target := SubscriptionTarget("custom")
	if got, present := decode(target); present {
		t.Fatalf("omitted max output emitted max_output_tokens=%d", got)
	}
	target.Params.MaxOutputTokens = 4_096
	if got, present := decode(target); !present || got != 4_096 {
		t.Fatalf("explicit max output emitted max_output_tokens=%d present=%v, want 4096", got, present)
	}
}

// A tool call is an item in the output here rather than a field on a message,
// and its arguments arrive as deltas keyed by item id. The fixture is a verbatim
// capture of the endpoint doing exactly that.
func TestResponsesToolCallIsAssembled(t *testing.T) {
	events, err := drainResponses(t, serveResponsesFixture(t, "responses_tool_call.sse"), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}

	var uses []provider.ToolUse
	for _, ev := range events {
		if ev.Type == provider.EventToolUse {
			uses = append(uses, *ev.ToolUse)
		}
	}
	if len(uses) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(uses))
	}
	if uses[0].Name != "read" {
		t.Errorf("tool name = %q", uses[0].Name)
	}

	// The id carried to the caller has to be call_id, not the item id. A result
	// returned against the item id does not correlate and the turn stalls.
	if !strings.HasPrefix(uses[0].ID, "call_") {
		t.Errorf("tool id = %q, want the call_id rather than the fc_ item id", uses[0].ID)
	}

	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(uses[0].Input, &args); err != nil {
		t.Fatalf("arguments did not reassemble into JSON: %v", err)
	}
	if args.Path != "main.go" {
		t.Errorf("args.path = %q", args.Path)
	}

	if got := terminal(t, events).StopReason; got != provider.StopToolUse {
		t.Errorf("StopReason = %q; the loop executes calls only on tool_use", got)
	}
}

// The turn after a tool result, captured from the same conversation.
func TestResponsesAnswersAfterAToolResult(t *testing.T) {
	events, err := drainResponses(t, serveResponsesFixture(t, "responses_after_tool_result.sse"), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}

	var text strings.Builder
	for _, ev := range events {
		if ev.Type == provider.EventTextDelta {
			text.WriteString(ev.Text)
		}
	}
	if !strings.Contains(text.String(), "package main") {
		t.Errorf("the answer did not use the tool result: %q", text.String())
	}

	done := terminal(t, events)
	if done.StopReason != provider.StopEndTurn {
		t.Errorf("StopReason = %q, want end_turn", done.StopReason)
	}
	if done.Usage.InputTokens == 0 || done.Usage.OutputTokens == 0 {
		t.Errorf("usage = %+v", done.Usage)
	}
}

// Cached tokens are a subset of input_tokens in this shape, as in
// chat-completions and unlike Anthropic where the three counts are disjoint.
// Adding them would double-count the prefix.
func TestResponsesCachedTokensSplitOutOfInput(t *testing.T) {
	c := serveResponses(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `data: {"type":"response.completed","response":{"status":"completed","usage":`+
			`{"input_tokens":1000,"output_tokens":20,"input_tokens_details":{"cached_tokens":800,"cache_write_tokens":150}}}}`+"\n")
	})

	events, err := drainResponses(t, c, provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	usage := terminal(t, events).Usage

	if usage.CacheReadTokens != 800 {
		t.Errorf("cache read = %d", usage.CacheReadTokens)
	}
	if usage.CacheWriteTokens != 150 {
		t.Errorf("cache write = %d; this shape reports a write count, unlike chat-completions", usage.CacheWriteTokens)
	}
	if usage.InputTokens != 200 {
		t.Errorf("input = %d, want the uncached remainder of 200", usage.InputTokens)
	}
}

// The request shape the endpoint enforces: a list of items, and store false.
func TestResponsesRequestShape(t *testing.T) {
	body, err := NewResponses().buildRequest(SubscriptionTarget("gpt-5.4-mini"), provider.Request{
		System:   []provider.Block{provider.Text{Text: "you are terse"}},
		Messages: []provider.Message{provider.UserText("hello")},
		Tools: []provider.ToolDefinition{{
			Name: "read", Description: "Read a file", Schema: json.RawMessage(`{"type":"object"}`),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var decoded struct {
		Store        *bool  `json:"store"`
		Instructions string `json:"instructions"`
		Input        []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"input"`
		Tools []struct {
			Type string          `json:"type"`
			Name string          `json:"name"`
			Fn   json.RawMessage `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}

	if decoded.Store == nil || *decoded.Store {
		t.Error("store must be present and false, or the endpoint refuses the request outright")
	}
	// A message item with role "system" is refused outright, so the prompt has
	// to travel in its own field.
	if decoded.Instructions != "you are terse" {
		t.Errorf("instructions = %q, want the system prompt", decoded.Instructions)
	}
	for _, item := range decoded.Input {
		if item.Role == "system" {
			t.Error(`an item with role "system" was sent, which this endpoint rejects`)
		}
	}
	if len(decoded.Input) != 1 || decoded.Input[0].Role != "user" {
		t.Fatalf("input = %+v, want just the user message", decoded.Input)
	}
	// The tool shape is flat here. Nesting under "function" is the other format.
	if len(decoded.Tools) != 1 || decoded.Tools[0].Name != "read" || decoded.Tools[0].Fn != nil {
		t.Errorf("tools = %+v, want a flat function tool", decoded.Tools)
	}
}

// A tool result is its own item with no role, and it refers to the call by
// call_id. An assistant turn that made a call becomes two items.
func TestResponsesReplayExcludesIncompleteAssistant(t *testing.T) {
	const partial = "PARTIAL MUST NOT REPLAY"
	base := provider.Request{Messages: []provider.Message{
		provider.UserText("before"),
		provider.UserText("after"),
	}}
	withPartial := base
	withPartial.Messages = []provider.Message{
		base.Messages[0],
		{
			Role:       provider.RoleAssistant,
			Incomplete: true,
			Content:    []provider.Block{provider.Text{Text: partial}},
		},
		base.Messages[1],
	}

	client := NewResponses()
	body, err := client.buildRequest(SubscriptionTarget("gpt-5.4-mini"), withPartial)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), partial) {
		t.Fatalf("incomplete assistant reached Responses wire:\n%s", body)
	}
	if !strings.Contains(string(body), "before") || !strings.Contains(string(body), "after") {
		t.Fatalf("projection removed surrounding durable messages:\n%s", body)
	}
	got, err := client.CountTokens(context.Background(), SubscriptionTarget("gpt-5.4-mini"), withPartial)
	if err != nil {
		t.Fatal(err)
	}
	want, err := client.CountTokens(context.Background(), SubscriptionTarget("gpt-5.4-mini"), base)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("estimate with partial = %+v, want %+v", got, want)
	}
}

func TestResponsesToolResultBecomesItsOwnItem(t *testing.T) {
	body, err := NewResponses().buildRequest(SubscriptionTarget("gpt-5.4-mini"), provider.Request{
		Messages: []provider.Message{
			provider.UserText("read main.go"),
			{Role: provider.RoleAssistant, Content: []provider.Block{
				provider.Text{Text: "I will read it."},
				provider.ToolUse{ID: "call_abc", Name: "read", Input: json.RawMessage(`{"path":"main.go"}`)},
			}},
			{Role: provider.RoleTool, Content: []provider.Block{
				provider.ToolResult{ToolUseID: "call_abc", Name: "read", Content: "package main"},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var decoded struct {
		Input []struct {
			Type   string `json:"type"`
			Role   string `json:"role"`
			CallID string `json:"call_id"`
			Output string `json:"output"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}

	kinds := []string{}
	for _, item := range decoded.Input {
		kinds = append(kinds, item.Type)
	}
	if strings.Join(kinds, ",") != "message,message,function_call,function_call_output" {
		t.Fatalf("items = %v; the assistant turn splits into text and a call, and the result is its own item", kinds)
	}

	result := decoded.Input[3]
	if result.Role != "" {
		t.Errorf("the tool result carried role %q; items of this type have none", result.Role)
	}
	if result.CallID != "call_abc" || result.Output != "package main" {
		t.Errorf("the result did not reference its call: %+v", result)
	}
}

func TestResponsesContinuityAndPromptRemainIsolatedParts(t *testing.T) {
	body, err := NewResponses().buildRequest(SubscriptionTarget("gpt-5.4-mini"), provider.Request{Messages: []provider.Message{{
		Role: provider.RoleUser,
		Content: []provider.Block{
			provider.Text{Text: "[continuity capsule]\n\n"},
			provider.Text{Text: "continue with the fix"},
		},
		ContinuityRef: "0123456789abcdef0123456789abcdef",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Input []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Input) != 1 || len(decoded.Input[0].Content) != 2 ||
		decoded.Input[0].Content[0].Text != "[continuity capsule]\n\n" ||
		decoded.Input[0].Content[1].Text != "continue with the fix" {
		t.Fatalf("Responses continuity parts = %+v", decoded.Input)
	}
	if strings.Contains(string(body), "continuity_ref") {
		t.Fatal("session-only continuity metadata reached the Responses wire")
	}
}

// This endpoint caches by routing key, so a plan of block positions has nowhere
// to land. Sending the request without them would drop what the manager asked
// for and report a miss it caused.
func TestResponsesRefusesABreakpointPlan(t *testing.T) {
	_, err := NewResponses().buildRequest(SubscriptionTarget("gpt-5.4-mini"), provider.Request{
		Messages:  []provider.Message{provider.UserText("hi")},
		CachePlan: &provider.CachePlan{Breakpoints: []provider.Breakpoint{{}}},
	})

	var capErr *provider.CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("err = %v, want a CapabilityError", err)
	}
}

func TestResponsesRendersThePromptCacheRoutingKey(t *testing.T) {
	body, err := NewResponses().buildRequest(SubscriptionTarget("gpt-5.4-mini"), provider.Request{
		Messages:  []provider.Message{provider.UserText("hi")},
		CachePlan: &provider.CachePlan{RoutingKey: "stable-prefix-abc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var request responsesRequest
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	if request.PromptCacheKey != "stable-prefix-abc" {
		t.Fatalf("prompt_cache_key = %q, want the manager's routing key", request.PromptCacheKey)
	}
}

// An empty model list is what a client version below the endpoint's floor
// returns. Reporting it as "no models" would send someone looking at their
// account instead of at the version.
func TestResponsesEmptyModelListNamesTheVersion(t *testing.T) {
	c := serveResponses(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"models":[]}`)
	})

	res, err := c.Probe(context.Background(), SubscriptionTarget("gpt-5.4-mini"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ModelPresent {
		t.Error("an empty list reported the model as present")
	}
	if !strings.Contains(res.Detail, "client_version") {
		t.Errorf("detail = %q; it has to name the version as the likely cause", res.Detail)
	}
}

func TestResponsesProbeListsWhatTheAccountHas(t *testing.T) {
	c := serveResponses(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"models":[{"slug":"gpt-5.4-mini"},{"slug":"gpt-5.5"}]}`)
	})

	if res, _ := c.Probe(context.Background(), SubscriptionTarget("gpt-5.4-mini")); !res.ModelPresent {
		t.Error("a model the account has was reported missing")
	}
	res, _ := c.Probe(context.Background(), SubscriptionTarget("gpt-5"))
	if res.ModelPresent {
		t.Error("a model the account lacks was reported present")
	}
	if !strings.Contains(res.Detail, "gpt-5.5") {
		t.Errorf("detail = %q; the slugs cannot be guessed, so they have to be listed", res.Detail)
	}
}

// The discovery answer states each model's own effort levels, and they are
// not the surface's floor: the capture lists six for Daybreak Blue and four
// for gpt-5.5. Reporting the floor here is what hid xhigh and max from the
// effort picker on a model that takes them.
func TestResponsesProbeReportsTheModelsOwnEffortLevels(t *testing.T) {
	c := serveResponsesFixture(t, "codex_models.json")

	res, err := c.Probe(context.Background(), SubscriptionTarget("gpt-daybreak-blue-latest"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"low", "medium", "high", "xhigh", "max", "ultra"}; !slices.Equal(res.EffortLevels, want) {
		t.Errorf("EffortLevels = %v, want the endpoint's ordered list %v", res.EffortLevels, want)
	}

	res, err = c.Probe(context.Background(), SubscriptionTarget("gpt-5.5"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"low", "medium", "high", "xhigh"}; !slices.Equal(res.EffortLevels, want) {
		t.Errorf("EffortLevels = %v, want %v: the levels are the model's, not the surface's", res.EffortLevels, want)
	}
}

// An entry with no supported_reasoning_levels leaves the list unknown. The
// surface's floor is the caller's fallback, not a fact about this model.
func TestResponsesProbeLeavesUnstatedEffortsUnknown(t *testing.T) {
	c := serveResponses(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"models":[{"slug":"gpt-5.4-mini"}]}`)
	})

	res, err := c.Probe(context.Background(), SubscriptionTarget("gpt-5.4-mini"))
	if err != nil {
		t.Fatal(err)
	}
	if res.EffortLevels != nil {
		t.Errorf("EffortLevels = %v, want nil from an entry that stated none", res.EffortLevels)
	}
	if res.VisionKnown {
		t.Error("an entry with no input_modalities invented text-only evidence")
	}
}

// The discovery answer also states each model's context window, and the
// number to believe is the operative one rather than the architecture's
// maximum: gpt-5.4 reports context_window 272,000 against a
// max_context_window of 1,000,000. Reading the maximum would gate
// auto-compaction against room the server does not give the model.
func TestResponsesProbeReportsTheModelsContextWindow(t *testing.T) {
	c := serveResponsesFixture(t, "codex_models.json")

	res, err := c.Probe(context.Background(), SubscriptionTarget("gpt-daybreak-blue-latest"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ContextWindow != 272000 {
		t.Errorf("ContextWindow = %d, want the 272000 the endpoint states for daybreak", res.ContextWindow)
	}

	res, err = c.Probe(context.Background(), SubscriptionTarget("gpt-5.4"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ContextWindow != 272000 {
		t.Errorf("gpt-5.4 ContextWindow = %d, want the operative 272000, not the 1000000 maximum", res.ContextWindow)
	}

	res, err = c.Probe(context.Background(), SubscriptionTarget("gpt-5.3-codex-spark"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ContextWindow != 128000 {
		t.Errorf("spark ContextWindow = %d, want 128000: the window is the model's, not the surface's", res.ContextWindow)
	}
}

func TestResponsesProbeReportsKnownNoVisionForTextOnlyModel(t *testing.T) {
	c := serveResponsesFixture(t, "codex_models.json")

	textOnly, err := c.Probe(context.Background(), SubscriptionTarget("gpt-5.3-codex-spark"))
	if err != nil {
		t.Fatal(err)
	}
	if !textOnly.VisionKnown || textOnly.Vision {
		t.Fatalf("text-only model vision evidence = known:%v vision:%v", textOnly.VisionKnown, textOnly.Vision)
	}

	vision, err := c.Probe(context.Background(), SubscriptionTarget("gpt-5.6-sol"))
	if err != nil {
		t.Fatal(err)
	}
	if !vision.VisionKnown || !vision.Vision {
		t.Fatalf("image-capable model vision evidence = known:%v vision:%v", vision.VisionKnown, vision.Vision)
	}
}

// The picker's model set and its effort lists come from the one answer, so
// every offered slug has an entry — empty where the model states none.
func TestResponsesModelEffortsCoversEveryOfferedSlug(t *testing.T) {
	c := serveResponsesFixture(t, "codex_models.json")

	names, err := c.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	efforts, err := c.ModelEfforts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(efforts) != len(names) {
		t.Fatalf("ModelEfforts covered %d slugs, Models listed %d", len(efforts), len(names))
	}
	for _, name := range names {
		if _, ok := efforts[name]; !ok {
			t.Errorf("%s is offered but has no entry in the effort map", name)
		}
	}
	if want := []string{"low", "medium", "high", "xhigh", "max", "ultra"}; !slices.Equal(efforts["gpt-daybreak-blue-latest"], want) {
		t.Errorf("daybreak efforts = %v, want %v", efforts["gpt-daybreak-blue-latest"], want)
	}
}

func TestResponsesModelsBoundsBodyAndRejectsUnsafeIDs(t *testing.T) {
	t.Run("oversize body", func(t *testing.T) {
		c := serveResponses(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, strings.Repeat(" ", provider.MaxProviderJSONBodyBytes+1))
		})
		if _, err := c.Models(context.Background()); err == nil || !strings.Contains(err.Error(), "response limit") {
			t.Fatalf("err = %v, want response-limit refusal", err)
		}
	})

	t.Run("credential shaped id", func(t *testing.T) {
		token := "ghp_" + strings.Repeat("A", 40)
		c := serveResponses(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"models":[{"slug":"model-`+token+`"}]}`)
		})
		_, err := c.Models(context.Background())
		if err == nil || strings.Contains(err.Error(), token) {
			t.Fatalf("unsafe model ID was accepted or echoed: %v", err)
		}
	})

	t.Run("unsafe effort", func(t *testing.T) {
		token := "ghp_" + strings.Repeat("A", 40)
		c := serveResponses(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"models":[{"slug":"safe","supported_reasoning_levels":[{"effort":"`+token+`"}]}]}`)
		})
		_, err := c.ModelEfforts(context.Background())
		if err == nil || strings.Contains(err.Error(), token) {
			t.Fatalf("unsafe effort was accepted or echoed: %v", err)
		}
	})
}

// The request carries client_version on every call, including discovery.
func TestResponsesSendsClientVersion(t *testing.T) {
	var seen string
	c := serveResponses(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query().Get("client_version")
		io.WriteString(w, `{"models":[]}`)
	})
	c.Probe(context.Background(), SubscriptionTarget("gpt-5.4-mini"))

	if seen == "" {
		t.Error("no client_version was sent, and without one the endpoint returns nothing useful")
	}
}

// An HTML body is what a wrong path returns. Handing that to a caller as a JSON
// decode failure hides the one fact that explains it.
func TestResponsesHTMLErrorIsExplained(t *testing.T) {
	c := serveResponses(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<html><head><title>404</title></head><body>not found</body></html>")
	})

	_, err := c.Stream(context.Background(), SubscriptionTarget("gpt-5.4-mini"), provider.Request{
		Messages: []provider.Message{provider.UserText("hi")},
	})
	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an APIError", err)
	}
	if !strings.Contains(apiErr.Body, "HTML") {
		t.Errorf("body = %q; an HTML page is the signal that the path is wrong", apiErr.Body)
	}
}

// The endpoint reports a rejected request as a "detail" string rather than the
// nested error object the documented API uses.
func TestResponsesDetailErrorIsUnwrapped(t *testing.T) {
	c := serveResponses(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"detail":"Store must be set to false"}`)
	})

	_, err := c.Stream(context.Background(), SubscriptionTarget("gpt-5.4-mini"), provider.Request{
		Messages: []provider.Message{provider.UserText("hi")},
	})
	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an APIError", err)
	}
	if apiErr.Body != "Store must be set to false" {
		t.Errorf("body = %q", apiErr.Body)
	}
}

func TestResponsesErrorCredentialBodyIsRedactedBeforeItsCap(t *testing.T) {
	token := "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for name, body := range map[string]string{
		"complete":           `{"detail":"rejected ` + token + `"}`,
		"display-cap":        strings.Repeat("x", 400-len(token)+1) + token,
		"structured-display": `{"detail":"` + strings.Repeat("x", 400-len(token)+1) + token + `"}`,
		"body-cap":           strings.Repeat("x", provider.MaxAPIErrorBodyBytes-len(token)+1) + token,
	} {
		t.Run(name, func(t *testing.T) {
			c := serveResponses(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, body)
			})
			_, err := c.Stream(context.Background(), SubscriptionTarget("gpt-5.4-mini"), provider.Request{})
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

func TestResponsesTruncatedStreamIsDistinguishable(t *testing.T) {
	c := serveResponses(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `data: {"type":"response.output_text.delta","item_id":"msg_1","delta":"half a th"}`+"\n")
	})

	events, err := drainResponses(t, c, provider.Request{})
	if !errors.Is(err, provider.ErrStreamIncomplete) {
		t.Fatalf("err = %v, want ErrStreamIncomplete", err)
	}
	if len(events) != 1 || events[0].Text != "half a th" {
		t.Errorf("partial content was lost: %+v", events)
	}
}

// A call with no call_id cannot have a result returned against it, so the turn
// cannot continue. Emitting it anyway would stall the loop with no explanation.
func TestResponsesCallWithoutACallIDIsReported(t *testing.T) {
	c := serveResponses(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `data: {"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","name":"read","arguments":"{}"}}`+"\n")
	})

	_, err := drainResponses(t, c, provider.Request{})
	var protoErr *provider.ProtocolError
	if !errors.As(err, &protoErr) {
		t.Fatalf("err = %v, want a ProtocolError", err)
	}
}
