package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/switchboard-code/switchboard/internal/provider"
)

// ResponsesClient serves the subscription surface.
//
// Everything here was established by asking the endpoint rather than by reading
// about it, because what it does and what the format documents are not the same
// thing: the model list needs a client version, store has to be false, and the
// input is a list of items rather than messages with roles.
type ResponsesClient struct {
	baseURL string
	token   string

	// clientVersion is sent on every request. Below some floor the model list
	// comes back empty instead of erroring, so a stale value looks like an
	// account with no models rather than a version problem.
	clientVersion string

	http *http.Client
}

// DefaultClientVersion is what this build sends. It is a value the endpoint
// accepts, not a claim to be any particular release.
const DefaultClientVersion = "0.147.0"

type ResponsesOption func(*ResponsesClient)

func WithResponsesBaseURL(raw string) ResponsesOption {
	return func(c *ResponsesClient) {
		if raw != "" {
			c.baseURL = strings.TrimSuffix(strings.TrimSpace(raw), "/")
		}
	}
}

func WithResponsesToken(token string) ResponsesOption {
	return func(c *ResponsesClient) { c.token = token }
}

func WithClientVersion(v string) ResponsesOption {
	return func(c *ResponsesClient) {
		if v != "" {
			c.clientVersion = v
		}
	}
}

func WithResponsesHTTPClient(h *http.Client) ResponsesOption {
	return func(c *ResponsesClient) { c.http = h }
}

func NewResponses(opts ...ResponsesOption) *ResponsesClient {
	c := &ResponsesClient{
		baseURL:       SubscriptionBaseURL,
		clientVersion: DefaultClientVersion,
		// No overall timeout: a long generation is not a stuck connection, and
		// the caller's context governs cancellation.
		http: &http.Client{},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *ResponsesClient) Name() string { return Name }

func (c *ResponsesClient) Stream(ctx context.Context, target provider.RouteTarget, req provider.Request) (provider.EventStream, error) {
	body, err := c.buildRequest(target, req)
	if err != nil {
		return nil, provider.MarkUnissued(err)
	}
	resp, err := c.do(ctx, http.MethodPost, "/responses", body)
	if err != nil {
		return nil, err
	}
	return newResponsesStream(ctx, resp.Body), nil
}

// CountTokens has no exact answer here: this endpoint exposes no counting
// route. The estimate is characters over four, with the measured bias in
// docs/estimator.md, and is flagged inexact so a budget check widens rather
// than trusting it.
func (c *ResponsesClient) CountTokens(_ context.Context, _ provider.RouteTarget, req provider.Request) (provider.TokenEstimate, error) {
	chars := 0
	for _, b := range req.System {
		chars += blockChars(b)
	}
	for _, t := range req.Tools {
		chars += len(t.Name) + len(t.Description) + len(t.Schema)
	}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			chars += blockChars(b)
		}
	}
	return provider.TokenEstimate{InputTokens: chars / 4, Exact: false}, nil
}

func blockChars(b provider.Block) int {
	switch v := b.(type) {
	case provider.Text:
		return len(v.Text)
	case provider.Thinking:
		return len(v.Text)
	case provider.ToolUse:
		return len(v.Name) + len(v.Input)
	case provider.ToolResult:
		return len(v.Name) + len(v.Content)
	default:
		return 0
	}
}

func (c *ResponsesClient) Probe(ctx context.Context, target provider.RouteTarget) (provider.ProbeResult, error) {
	var res provider.ProbeResult

	resp, err := c.do(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		var apiErr *provider.APIError
		// An endpoint that answered with an error is reachable. Only a
		// transport failure means nothing responded.
		res.Reachable = errors.As(err, &apiErr)
		res.Detail = err.Error()
		return res, nil
	}
	defer resp.Body.Close()

	var list codexModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return res, &provider.ProtocolError{Provider: Name, Detail: "decoding the model list", Err: err}
	}
	res.Reachable = true

	if len(list.Models) == 0 {
		// An empty list is what a client version below the endpoint's floor
		// returns, and it is indistinguishable from an account with nothing
		// available unless the difference is named here.
		res.Detail = fmt.Sprintf("the model list came back empty, which is also what client_version %q returns when it is below what the endpoint accepts", c.clientVersion)
		return res, nil
	}

	var offered []string
	for _, m := range list.Models {
		offered = append(offered, m.Slug)
		if m.Slug == target.ModelID {
			res.ModelPresent = true
			res.ContextWindow = m.ContextWindow
			// The endpoint's per-model window is its own statement of what
			// the model holds, not a metadata inference.
			res.WindowEnforced = m.ContextWindow > 0
			res.EffortLevels = m.effortLevels()
		}
	}
	if !res.ModelPresent {
		res.Detail = fmt.Sprintf("this account is not offered %q; it has %s",
			target.ModelID, strings.Join(offered, ", "))
		return res, nil
	}
	res.Tools = provider.ToolsParallel
	return res, nil
}

// Models lists what the account is offered, which is the only way to find out:
// the slugs are not the developer API's names and cannot be guessed.
func (c *ResponsesClient) Models(ctx context.Context) ([]string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var list codexModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, &provider.ProtocolError{Provider: Name, Detail: "decoding the model list", Err: err}
	}
	out := make([]string, 0, len(list.Models))
	for _, m := range list.Models {
		out = append(out, m.Slug)
	}
	return out, nil
}

// ModelEfforts pairs each offered slug with the reasoning efforts the
// endpoint states for it, in the server's own order. Every slug appears, with
// an empty list where the entry says nothing, so the caller's model set is
// the same one Models would return.
func (c *ResponsesClient) ModelEfforts(ctx context.Context) (map[string][]string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var list codexModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, &provider.ProtocolError{Provider: Name, Detail: "decoding the model list", Err: err}
	}
	out := make(map[string][]string, len(list.Models))
	for _, m := range list.Models {
		out[m.Slug] = m.effortLevels()
	}
	return out, nil
}

func (c *ResponsesClient) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	target := c.baseURL + path + "?" + url.Values{"client_version": {c.clientVersion}}.Encode()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		resp.Body.Close()
		return nil, &provider.APIError{
			Provider:   Name,
			StatusCode: resp.StatusCode,
			Body:       responsesErrorMessage(raw),
		}
	}
	return resp, nil
}

// responsesErrorMessage unwraps what this endpoint returns, which is a "detail"
// string rather than the nested error object the documented API uses. A body
// that is neither is returned as-is but capped, because an HTML error page is
// itself the useful signal that a path does not exist.
func responsesErrorMessage(raw []byte) string {
	var envelope struct {
		Detail any `json:"detail"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil {
		if envelope.Error != nil && envelope.Error.Message != "" {
			return envelope.Error.Message
		}
		switch d := envelope.Detail.(type) {
		case string:
			if d != "" {
				return d
			}
		case nil:
		default:
			if encoded, err := json.Marshal(d); err == nil {
				return string(encoded)
			}
		}
	}

	text := strings.TrimSpace(string(raw))
	if strings.HasPrefix(text, "<") {
		return "the endpoint returned an HTML page rather than an API response, which means this path does not exist"
	}
	const limit = 400
	if len(text) > limit {
		text = text[:limit] + "..."
	}
	return text
}

func (c *ResponsesClient) buildRequest(target provider.RouteTarget, req provider.Request) ([]byte, error) {
	if req.CachePlan != nil && len(req.CachePlan.Breakpoints) > 0 {
		// This endpoint caches by routing key rather than by marker, so a plan
		// built from block positions has nowhere to land. Sending the request
		// without the markers would silently drop what the manager asked for.
		return nil, &provider.CapabilityError{
			Target:     target.ID(),
			Capability: "cache breakpoints",
			Detail:     "this target caches by routing key, not by markers on blocks; set a prompt cache key instead",
		}
	}

	input := make([]responsesItem, 0, len(req.Messages))

	// The system prompt goes in its own field. Sending it as a message item
	// with role "system" is refused: "System messages are not allowed".
	var instructions strings.Builder
	for _, block := range req.System {
		if text, ok := block.(provider.Text); ok {
			instructions.WriteString(text.Text)
		}
	}

	for _, m := range req.Messages {
		items, err := messageToItems(target, m)
		if err != nil {
			return nil, err
		}
		input = append(input, items...)
	}

	out := responsesRequest{
		Model:           target.ModelID,
		Stream:          true,
		Store:           false,
		Instructions:    instructions.String(),
		Input:           input,
		MaxOutputTokens: target.Params.MaxOutputTokens,
		Temperature:     target.Params.Temperature,
	}
	if req.CachePlan != nil {
		out.PromptCacheKey = req.CachePlan.RoutingKey
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, responsesTool{
			Type: "function", Name: t.Name, Description: t.Description, Parameters: t.Schema,
		})
	}
	if r := target.Params.Reasoning; r != nil && r.Enabled {
		out.Reasoning = &responsesReasoning{Effort: r.Effort}
	}
	return json.Marshal(out)
}

// messageToItems flattens one canonical message into items.
//
// One message can become several: an assistant turn that made two tool calls is
// one message here and three items there, and a tool result is its own item
// with no role at all.
func messageToItems(target provider.RouteTarget, m provider.Message) ([]responsesItem, error) {
	var items []responsesItem
	var parts []responsesContent

	textType := "input_text"
	if m.Role == provider.RoleAssistant {
		textType = "output_text"
	}

	for _, block := range m.Content {
		switch v := block.(type) {
		case provider.Text:
			parts = append(parts, responsesContent{Type: textType, Text: v.Text})

		case provider.Thinking:
			// Reasoning is not replayed. This endpoint issues its own reasoning
			// items with ids, and sending back text it did not issue is not the
			// same thing as returning what it gave.

		case provider.ToolUse:
			items = append(items, responsesItem{
				Type: "function_call", CallID: v.ID, Name: v.Name,
				Arguments: string(v.Input), Status: "completed",
			})

		case provider.ToolResult:
			items = append(items, responsesItem{
				Type: "function_call_output", CallID: v.ToolUseID, Output: v.Content,
			})

		case provider.Image:
			parts = append(parts, responsesContent{
				Type:     "input_image",
				ImageURL: "data:" + v.MediaType + ";base64," + base64.StdEncoding.EncodeToString(v.Data),
			})

		default:
			return nil, &provider.CapabilityError{
				Target:     target.ID(),
				Capability: fmt.Sprintf("content block %q", block.Kind()),
				Detail:     "this adapter has no item form for that block",
			}
		}
	}

	if len(parts) > 0 {
		// Content parts lead, so a turn's text precedes the calls it made.
		items = append([]responsesItem{{
			Type: "message", Role: string(m.Role), Content: parts,
		}}, items...)
	}
	return items, nil
}
