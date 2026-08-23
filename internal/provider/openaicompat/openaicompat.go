// Package openaicompat adapts servers that speak the OpenAI chat-completions
// format to the canonical provider interface.
//
// "OpenAI-compatible" is a spectrum rather than a specification, so this
// adapter works from named profiles that have actually been tested against a
// server rather than from one assumed feature set. An unknown endpoint starts
// from the lowest common set and says so (§5.2).
//
// It exists as much to keep the canonical types honest as to reach any
// particular server: the same conversation has to round-trip through this and
// through a native adapter and mean the same thing. Where the two disagree,
// the canonical layer is not yet canonical.
package openaicompat

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
	"slices"
	"strings"

	"github.com/switchboard-code/switchboard/internal/provider"
)

// Name is the provider component of a RouteTarget bound to this adapter.
const Name = "openaicompat"

// Profile records what a particular server actually does. Fields are set from
// observed behavior, not from what the server claims to support.
// The profile name is also the serving surface in a RouteTarget, so the same
// model reached through this adapter is a different target from the same model
// reached natively: it gets its own catalog entry, price sheet, cache
// behavior, and quirks.
type Profile struct {
	// Provider overrides the provider component of a RouteTarget. It exists so
	// that a vendor with its own name and its own catalog entry is not filed
	// under "openaicompat" merely because this adapter speaks its format. An
	// empty value means the target really is a generic compatible endpoint.
	Provider string

	BaseURL string

	Tools bool

	// Reasoning reports whether the server returns reasoning text alongside
	// content. The field name differs between servers, so both spellings are
	// read; this only says whether to expect any.
	Reasoning bool

	// StreamUsage reports whether the server honors stream_options and sends a
	// final usage chunk. Without it a streamed turn reports no token counts at
	// all, which the cost model has to be told rather than left to infer.
	StreamUsage bool

	// EffortLevels are the values the server accepts for reasoning_effort. An
	// empty list means the parameter is not supported and requesting it is a
	// capability error rather than something quietly dropped.
	EffortLevels []string
}

// Profiles are the servers this adapter has been tested against.
//
// Only Ollama is exercised by the test suite today. The others are absent on
// purpose: a profile nobody has run is a guess wearing a name, and the whole
// reason this type exists is that "OpenAI-compatible" does not tell you what a
// server does.
var Profiles = map[string]Profile{
	"ollama": {
		BaseURL:      "http://localhost:11434/v1",
		Tools:        true,
		Reasoning:    true,
		StreamUsage:  true,
		EffortLevels: []string{"low", "medium", "high", "max"},
	},

	// generic is the floor for an endpoint nobody has characterized: tools,
	// because that is what the adapter is for, and nothing else assumed.
	"generic": {
		Tools: true,
	},
}

type Client struct {
	profile     Profile
	profileName string
	apiKey      string
	http        *http.Client
}

type Option func(*Client)

func WithBaseURL(raw string) Option {
	return func(c *Client) {
		if raw != "" {
			c.profile.BaseURL = strings.TrimSuffix(strings.TrimSpace(raw), "/")
		}
	}
}

// WithAPIKey supplies a bearer token. It is passed in rather than read from the
// environment here, so credential resolution stays in one place (§5.3).
func WithAPIKey(key string) Option {
	return func(c *Client) { c.apiKey = key }
}

func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// New builds a client for a named profile. An unknown name is an error rather
// than a silent fall back to the generic floor, because a typo would otherwise
// quietly disable the capabilities the user asked for.
func New(profileName string, opts ...Option) (*Client, error) {
	profile, ok := Profiles[profileName]
	if !ok {
		return nil, fmt.Errorf("no tested profile named %q; known profiles are %s",
			profileName, strings.Join(profileNames(), ", "))
	}
	return NewFor(profileName, profile, opts...), nil
}

// NewFor builds a client for a profile the caller owns rather than one from the
// tested map. It is how a vendor that speaks this format gets its own provider
// name and catalog entry without every such vendor having to appear in a map
// that is meant to describe servers this package has itself run against.
func NewFor(profileName string, profile Profile, opts ...Option) *Client {
	c := &Client{
		profile:     profile,
		profileName: profileName,
		// No overall timeout: a long generation is not a stuck connection, and
		// the caller's context governs cancellation.
		http: &http.Client{},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func profileNames() []string {
	names := make([]string, 0, len(Profiles))
	for n := range Profiles {
		names = append(names, n)
	}
	// A stable order keeps the error message deterministic.
	slices.Sort(names)
	return names
}

// Name reports the provider this client speaks for, which is the profile's
// vendor where it names one and the generic adapter otherwise.
func (c *Client) Name() string { return providerName(c.profile) }

func providerName(p Profile) string {
	if p.Provider != "" {
		return p.Provider
	}
	return Name
}

// Target builds a RouteTarget for a model reached through this adapter.
func Target(profileName, model string) (provider.RouteTarget, error) {
	if _, ok := Profiles[profileName]; !ok {
		return provider.RouteTarget{}, fmt.Errorf("no tested profile named %q; known profiles are %s",
			profileName, strings.Join(profileNames(), ", "))
	}
	return provider.RouteTarget{Provider: Name, Surface: profileName, ModelID: model}, nil
}

// KnownProfile reports whether a serving surface names a tested profile.
func KnownProfile(surface string) bool {
	_, ok := Profiles[surface]
	return ok
}

func (c *Client) Stream(ctx context.Context, target provider.RouteTarget, req provider.Request) (provider.EventStream, error) {
	body, err := c.buildRequest(target, req)
	if err != nil {
		return nil, provider.MarkUnissued(err)
	}
	resp, err := c.do(ctx, http.MethodPost, "/chat/completions", body)
	if err != nil {
		return nil, err
	}
	return newStream(ctx, resp.Body, c.profile), nil
}

// CountTokens has no exact answer on a generic compatible endpoint: there is no
// tokenizer endpoint in the format, and the true count depends on the server's
// chat template. The estimate is deliberately crude and flagged inexact.
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

// BaseURL reports the address this client will call, so a UI can say which
// server it is about to ask rather than describing it as "the endpoint".
func (c *Client) BaseURL() string { return c.profile.BaseURL }

// Models lists what the server serves. /models is the only discovery the
// format has, and a server that answers it can have its model ids offered
// instead of typed from memory. A server that does not answer is not an
// error the caller has to treat as fatal: the id can still be entered by hand.
func (c *Client) Models(ctx context.Context) ([]string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var list modelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, &provider.ProtocolError{Provider: c.Name(), Detail: "decoding /models", Err: err}
	}
	out := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		out = append(out, m.ID)
	}
	return out, nil
}

// Probe reports reachability and what the profile says the server can do. The
// model list is the only capability signal the format offers; everything else
// comes from the profile, which is why profiles have to be tested rather than
// assumed.
//
// The one thing the list does sometimes carry beyond the id is the context
// window, and reading it is what lets a local target have one at all: the
// catalog records zero for this surface because it cannot describe a server it
// has never seen, and a zero window is what leaves auto-compaction off and the
// meter blank. Where the server states nothing, it stays zero and /context
// still takes the number by hand.
func (c *Client) Probe(ctx context.Context, target provider.RouteTarget) (provider.ProbeResult, error) {
	var res provider.ProbeResult

	resp, err := c.do(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		var apiErr *provider.APIError
		res.Reachable = errors.As(err, &apiErr)
		res.Detail = err.Error()
		return res, nil
	}
	defer resp.Body.Close()

	var list modelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return res, &provider.ProtocolError{Provider: c.Name(), Detail: "decoding /models", Err: err}
	}
	res.Reachable = true
	for _, m := range list.Data {
		if m.ID == target.ModelID {
			res.ModelPresent = true
			res.ContextWindow = m.contextWindow()
			// vLLM's number is the limit the server enforces per request.
			// The other names are metadata that can contradict itself — one
			// unsloth-studio reports an allocation and an architecture
			// ceiling side by side — so only vLLM's counts as enforced.
			res.WindowEnforced = m.MaxModelLen > 0 && m.MaxModelLen == res.ContextWindow
			break
		}
	}
	if !res.ModelPresent {
		res.Detail = fmt.Sprintf("model %q is not served at %s", target.ModelID, c.profile.BaseURL)
		return res, nil
	}
	if res.ContextWindow == 0 {
		if allocated := c.slotContextWindow(ctx); allocated > 0 {
			// /props reports what the server allocated, which is an
			// enforced fact rather than a metadata reading.
			res.ContextWindow = allocated
			res.WindowEnforced = true
		}
	}

	if c.profile.Tools {
		res.Tools = provider.ToolsParallel
	} else {
		res.Tools = provider.ToolsNone
	}
	res.Detail = fmt.Sprintf("profile %q; the format reports no per-model capabilities, so this is the profile's word", c.profileName)
	return res, nil
}

// slotContextWindow asks llama.cpp what it allocated, for the servers whose
// model list says nothing.
//
// That one needs asking separately because its discovery response carries only
// meta.n_ctx_train, the length the model was trained at, which is not what the
// server will accept: llama-server allocates -c and defaults it far below the
// trained length, so reading n_ctx_train would over-report by an order of
// magnitude on a default launch. /props carries the allocated number, and it
// sits at the server root rather than under the API path, so the trailing
// version segment comes off the configured address.
//
// Anything other than an answer leaves the window unknown, which is already
// the truth for every server that does not serve this endpoint. It costs one
// GET, and only on a target that reported no window of its own.
func (c *Client) slotContextWindow(ctx context.Context) int {
	endpoint, err := url.JoinPath(strings.TrimSuffix(c.profile.BaseURL, "/v1"), "props")
	if err != nil {
		return 0
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	var props serverProps
	if err := json.NewDecoder(resp.Body).Decode(&props); err != nil {
		return 0
	}
	if n := props.DefaultGenerationSettings.NCtx; n > 0 {
		return n
	}
	return 0
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	if c.profile.BaseURL == "" {
		// A profile with no address cannot be reached, and the failure is a
		// missing setting rather than a server that is down. Saying which
		// setting is the difference between a fix and a guess.
		return nil, fmt.Errorf(
			"%s profile %q has no server address; set it with /setup, or write [providers.%q] base_url in the config",
			Name, c.profileName, Name+"/"+c.profileName)
	}

	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}

	endpoint, err := url.JoinPath(c.profile.BaseURL, path)
	if err != nil {
		return nil, fmt.Errorf("building %s url: %w", path, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s: %s unreachable at %s: %w", Name, path, c.profile.BaseURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, &provider.APIError{Provider: c.Name(), StatusCode: resp.StatusCode, Body: errorMessage(raw)}
	}
	return resp, nil
}

// errorMessage unwraps the nested error object the format uses, falling back to
// the raw body so a server that answers with something else is still legible.
func errorMessage(raw []byte) string {
	var wrapper struct {
		Error wireError `json:"error"`
	}
	if json.Unmarshal(raw, &wrapper) == nil && wrapper.Error.Message != "" {
		return wrapper.Error.Message
	}
	return strings.TrimSpace(string(raw))
}

func (c *Client) buildRequest(target provider.RouteTarget, req provider.Request) ([]byte, error) {
	if req.CachePlan != nil && req.CachePlan.RoutingKey != "" {
		return nil, &provider.CapabilityError{
			Target: target.ID(), Capability: "cache routing key",
			Detail: "this chat-completions profile has no verified field for prompt cache affinity",
		}
	}
	if req.CachePlan != nil && len(req.CachePlan.Breakpoints) > 0 {
		return nil, &provider.CapabilityError{
			Target:     target.ID(),
			Capability: "explicit cache breakpoints",
			Detail:     "the chat-completions format has no way to place them",
		}
	}

	out := chatRequest{Model: target.ModelID, Stream: true}
	if c.profile.StreamUsage {
		out.StreamOptions = &streamOptions{IncludeUsage: true}
	}

	if system := blocksText(req.System); system != "" {
		out.Messages = append(out.Messages, wireMessage{Role: "system", Content: system})
	}
	for _, m := range req.Messages {
		converted, err := c.toWireMessages(target, m)
		if err != nil {
			return nil, err
		}
		out.Messages = append(out.Messages, converted...)
	}

	if len(req.Tools) > 0 && !c.profile.Tools {
		return nil, &provider.CapabilityError{
			Target:     target.ID(),
			Capability: "tool calling",
			Detail:     fmt.Sprintf("profile %q does not report tool support", c.profileName),
		}
	}
	for _, t := range req.Tools {
		schema := t.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, wireTool{
			Type:     "function",
			Function: wireToolFunc{Name: t.Name, Description: t.Description, Parameters: schema, Strict: t.Strict},
		})
	}

	if r := target.Params.Reasoning; r != nil && r.Enabled && r.Effort != "" {
		if !slices.Contains(c.profile.EffortLevels, r.Effort) {
			return nil, &provider.CapabilityError{
				Target:     target.ID(),
				Capability: "reasoning effort " + r.Effort,
				Detail: fmt.Sprintf("profile %q accepts %s", c.profileName,
					describeLevels(c.profile.EffortLevels)),
			}
		}
		out.ReasoningEffort = r.Effort
	}

	if n := target.Params.MaxOutputTokens; n > 0 {
		out.MaxTokens = &n
	}
	if temp := target.Params.Temperature; temp != nil {
		out.Temperature = temp
	}

	return json.Marshal(out)
}

func describeLevels(levels []string) string {
	if len(levels) == 0 {
		return "no effort levels"
	}
	return strings.Join(levels, ", ")
}

// toWireMessages flattens one canonical message.
//
// A tool message carrying several results becomes several wire messages, and
// tool arguments are re-encoded as a string, which is the format's shape and
// the main place a round trip through both adapters can go wrong.
func (c *Client) toWireMessages(target provider.RouteTarget, m provider.Message) ([]wireMessage, error) {
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
			out = append(out, wireMessage{Role: "tool", ToolCallID: r.ToolUseID, Content: r.Content})
		}
		return out, nil
	}

	wm := wireMessage{Role: string(m.Role)}
	var text strings.Builder
	var parts []contentPart

	for _, b := range m.Content {
		switch v := b.(type) {
		case provider.Text:
			text.WriteString(v.Text)
		case provider.Thinking:
			wm.Reasoning += v.Text
		case provider.ToolUse:
			wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
				ID:       v.ID,
				Index:    len(wm.ToolCalls),
				Type:     "function",
				Function: wireToolCallFunc{Name: v.Name, Arguments: string(v.Input)},
			})
		case provider.Image:
			parts = append(parts, contentPart{
				Type: "image_url",
				ImageURL: &imageURL{URL: fmt.Sprintf("data:%s;base64,%s",
					v.MediaType, base64.StdEncoding.EncodeToString(v.Data))},
			})
		case provider.Document:
			return nil, &provider.CapabilityError{
				Target:     target.ID(),
				Capability: "document input",
				Detail:     "the chat-completions format has no document content type",
			}
		default:
			return nil, fmt.Errorf("%s: cannot render a %s block", Name, b.Kind())
		}
	}

	// The array form is only used when there is something other than text,
	// because some compatible servers reject it for plain messages.
	if len(parts) > 0 {
		if text.Len() > 0 {
			parts = append([]contentPart{{Type: "text", Text: text.String()}}, parts...)
		}
		wm.Content = parts
	} else {
		wm.Content = text.String()
	}
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
