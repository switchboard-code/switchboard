// Package ollama adapts a local Ollama server to the canonical provider
// interface. It targets the native /api/chat endpoint, not the
// OpenAI-compatible shim, because the native schema keeps reasoning output in a
// dedicated field and tool arguments as structured JSON.
//
// One caveat matters for later phases: Ollama reuses its KV cache across
// requests but reports nothing about it in usage. Cache read and write counts
// are therefore always zero here, and this target cannot exercise the
// cache-economics work in §6. That needs a provider that bills and reports
// caching.
package ollama

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
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/switchboard-code/switchboard/internal/provider"
)

const (
	// Name is the provider component of a RouteTarget.
	Name = "ollama"

	// SurfaceLocal is the only serving surface Ollama exposes.
	SurfaceLocal = "local"

	defaultHost = "http://localhost:11434"
)

// validThinkEfforts is the set the server itself reports when it rejects a bad
// value. Rejecting anything outside it here turns a mid-turn 400 into an error
// the caller gets before a request is sent.
var validThinkEfforts = map[string]bool{"low": true, "medium": true, "high": true, "max": true}

type Client struct {
	baseURL string
	http    *http.Client
}

type Option func(*Client)

// WithBaseURL points the client at a server. An empty address is not an
// address: it leaves whatever the constructor resolved from the environment
// in place, so a caller that passes an unset flag through does not silently
// overwrite OLLAMA_HOST with the built-in default.
func WithBaseURL(raw string) Option {
	return func(c *Client) {
		if strings.TrimSpace(raw) != "" {
			c.baseURL = normalizeHost(raw)
		}
	}
}

func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

func New(opts ...Option) *Client {
	c := &Client{
		baseURL: normalizeHost(os.Getenv("OLLAMA_HOST")),
		// No overall timeout: a long generation is not a stuck connection, and
		// the caller's context governs cancellation.
		http: &http.Client{},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// normalizeHost accepts the shapes OLLAMA_HOST is written in the wild:
// "host:port", "http://host:port", and empty.
func normalizeHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultHost
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	return strings.TrimSuffix(raw, "/")
}

func (c *Client) Name() string { return Name }

// BaseURL reports the resolved server address, so a caller that reaches the
// same server through a different protocol does not have to redo the
// environment and flag precedence this client already settled.
func (c *Client) BaseURL() string { return c.baseURL }

// Target builds a RouteTarget for a model served by this client.
func Target(model string) provider.RouteTarget {
	return provider.RouteTarget{Provider: Name, Surface: SurfaceLocal, ModelID: model}
}

func (c *Client) Stream(ctx context.Context, target provider.RouteTarget, req provider.Request) (provider.EventStream, error) {
	body, err := c.buildRequest(target, req, true)
	if err != nil {
		return nil, provider.MarkUnissued(err)
	}
	resp, err := c.do(ctx, http.MethodPost, "/api/chat", body)
	if err != nil {
		return nil, err
	}
	return newStream(ctx, resp.Body), nil
}

// CountTokens has no exact answer here: Ollama exposes no tokenizer endpoint,
// and the true count depends on the model's chat template. The estimate is
// deliberately crude and flagged inexact so budget checks widen their margin
// rather than trusting it.
func (c *Client) CountTokens(_ context.Context, _ provider.RouteTarget, req provider.Request) (provider.TokenEstimate, error) {
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
	case provider.Image:
		return len(v.Data) / 3
	case provider.Document:
		return len(v.Data) / 3
	default:
		return 0
	}
}

// Models lists what this server has pulled, so the CLI can name real choices
// instead of telling the user their model was not found.
func (c *Client) Models(ctx context.Context) ([]string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/tags", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var tags tagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, &provider.ProtocolError{Provider: Name, Detail: "decoding /api/tags", Err: err}
	}
	names := make([]string, 0, len(tags.Models))
	for _, m := range tags.Models {
		names = append(names, m.Name)
	}
	return names, nil
}

func (c *Client) Probe(ctx context.Context, target provider.RouteTarget) (provider.ProbeResult, error) {
	var res provider.ProbeResult

	tagsResp, err := c.do(ctx, http.MethodGet, "/api/tags", nil)
	if err != nil {
		// An APIError means the server answered, so it is reachable even though
		// this call failed. A transport error means it is not.
		var apiErr *provider.APIError
		res.Reachable = errors.As(err, &apiErr)
		res.Detail = err.Error()
		return res, nil
	}
	defer tagsResp.Body.Close()

	var tags tagsResponse
	if err := json.NewDecoder(tagsResp.Body).Decode(&tags); err != nil {
		return res, &provider.ProtocolError{Provider: Name, Detail: "decoding /api/tags", Err: err}
	}
	res.Reachable = true
	for _, m := range tags.Models {
		if m.Name == target.ModelID {
			res.ModelPresent = true
			break
		}
	}
	if !res.ModelPresent {
		res.Detail = fmt.Sprintf("model %q is not pulled on this server", target.ModelID)
		return res, nil
	}

	showBody, err := json.Marshal(map[string]string{"model": target.ModelID})
	if err != nil {
		return res, err
	}
	showResp, err := c.do(ctx, http.MethodPost, "/api/show", showBody)
	if err != nil {
		res.Detail = "model is present but /api/show failed: " + err.Error()
		return res, nil
	}
	defer showResp.Body.Close()

	var show showResponse
	if err := json.NewDecoder(showResp.Body).Decode(&show); err != nil {
		return res, &provider.ProtocolError{Provider: Name, Detail: "decoding /api/show", Err: err}
	}
	for _, capability := range show.Capabilities {
		switch capability {
		case "tools":
			// Ollama reports that the template can emit tool calls. Whether the
			// model calls tools well enough to drive an agent loop is a separate
			// question that only evaluation answers (§4).
			res.Tools = provider.ToolsParallel
		case "vision":
			res.Vision = true
		}
	}
	if res.Tools == "" {
		res.Tools = provider.ToolsNone
		res.Detail = "model does not advertise tool support"
	}
	res.ContextWindow, res.WindowEnforced = show.contextWindow()
	return res, nil
}

// numCtx reads a num_ctx set in the Modelfile's parameter block. That number
// is what the server allocates, so it wins over the architecture's maximum:
// a 262k model served at 8k will refuse a 100k request, and a window that
// over-reports is worse than none, because everything downstream trusts it.
var numCtx = regexp.MustCompile(`(?m)^\s*num_ctx\s+(\d+)\s*$`)

// contextWindow reports what this server will accept for the model, or zero
// when it said nothing this can be read from. The second return is whether
// the number is the allocation the server enforces (a Modelfile num_ctx)
// rather than the architecture's ceiling, which is metadata that does not
// outrank the number the user declared.
func (s showResponse) contextWindow() (int, bool) {
	if m := numCtx.FindStringSubmatch(s.Parameters); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n, true
		}
	}
	// Keyed by architecture, so the name is matched by suffix rather than
	// guessed at: "qwen3.context_length", "llama.context_length", and so on.
	for key, raw := range s.ModelInfo {
		if !strings.HasSuffix(key, ".context_length") {
			continue
		}
		var n int
		if json.Unmarshal(raw, &n) == nil && n > 0 {
			return n, false
		}
	}
	return 0, false
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}

	endpoint, err := url.JoinPath(c.baseURL, path)
	if err != nil {
		return nil, fmt.Errorf("building %s url: %w", path, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s: %s unreachable at %s: %w", Name, path, c.baseURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		msg := strings.TrimSpace(string(raw))
		var wireErr errorResponse
		if json.Unmarshal(raw, &wireErr) == nil && wireErr.Error != "" {
			msg = wireErr.Error
		}
		return nil, &provider.APIError{Provider: Name, StatusCode: resp.StatusCode, Body: msg}
	}
	return resp, nil
}

func (c *Client) buildRequest(target provider.RouteTarget, req provider.Request, stream bool) ([]byte, error) {
	if req.CachePlan != nil && req.CachePlan.RoutingKey != "" {
		return nil, &provider.CapabilityError{
			Target: target.ID(), Capability: "cache routing key",
			Detail: "Ollama exposes no prompt cache affinity key",
		}
	}
	if req.CachePlan != nil && len(req.CachePlan.Breakpoints) > 0 {
		return nil, &provider.CapabilityError{
			Target:     target.ID(),
			Capability: "explicit cache breakpoints",
			Detail:     "Ollama manages its KV cache internally and exposes no breakpoint control",
		}
	}

	out := chatRequest{Model: target.ModelID, Stream: stream}

	if system := blocksText(req.System); system != "" {
		out.Messages = append(out.Messages, wireMessage{Role: "system", Content: system})
	}
	for _, m := range req.Messages {
		converted, err := toWireMessages(target, m)
		if err != nil {
			return nil, err
		}
		out.Messages = append(out.Messages, converted...)
	}

	for _, t := range req.Tools {
		schema := t.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, wireTool{
			Type:     "function",
			Function: wireToolFunc{Name: t.Name, Description: t.Description, Parameters: schema},
		})
	}

	if r := target.Params.Reasoning; r != nil {
		switch {
		case !r.Enabled:
			out.Think = false
		case r.Effort == "":
			out.Think = true
		case validThinkEfforts[r.Effort]:
			out.Think = r.Effort
		default:
			return nil, &provider.CapabilityError{
				Target:     target.ID(),
				Capability: "reasoning effort " + r.Effort,
				Detail:     `Ollama accepts only "low", "medium", "high", or "max"`,
			}
		}
	}

	options := map[string]any{}
	if n := target.Params.MaxOutputTokens; n > 0 {
		options["num_predict"] = n
	}
	if temp := target.Params.Temperature; temp != nil {
		options["temperature"] = *temp
	}
	if len(options) > 0 {
		out.Options = options
	}

	return json.Marshal(out)
}

// toWireMessages flattens one canonical message. A tool message carrying
// several results becomes several wire messages, because Ollama correlates a
// result to its call through a message-level tool_name rather than through
// blocks.
func toWireMessages(target provider.RouteTarget, m provider.Message) ([]wireMessage, error) {
	if m.Incomplete {
		return nil, nil
	}

	if m.Role == provider.RoleTool {
		var out []wireMessage
		for _, b := range m.Content {
			r, ok := b.(provider.ToolResult)
			if !ok {
				return nil, fmt.Errorf("%s: tool message carries a %s block", Name, b.Kind())
			}
			out = append(out, wireMessage{
				Role:       "tool",
				ToolName:   r.Name,
				ToolCallID: r.ToolUseID,
				Content:    r.Content,
			})
		}
		return out, nil
	}

	wm := wireMessage{Role: string(m.Role)}
	var text strings.Builder
	for _, b := range m.Content {
		switch v := b.(type) {
		case provider.Text:
			text.WriteString(v.Text)
		case provider.Thinking:
			wm.Thinking += v.Text
		case provider.ToolUse:
			wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
				ID:       v.ID,
				Function: wireToolCallFunc{Name: v.Name, Arguments: v.Input},
			})
		case provider.Image:
			wm.Images = append(wm.Images, base64.StdEncoding.EncodeToString(v.Data))
		case provider.Document:
			return nil, &provider.CapabilityError{
				Target:     target.ID(),
				Capability: "document input",
				Detail:     "Ollama accepts images but has no document content type",
			}
		default:
			return nil, fmt.Errorf("%s: cannot render a %s block", Name, b.Kind())
		}
	}
	wm.Content = text.String()
	return []wireMessage{wm}, nil
}

func blocksText(blocks []provider.Block) string {
	var b strings.Builder
	for _, block := range blocks {
		if t, ok := block.(provider.Text); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}
