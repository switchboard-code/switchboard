package openaicompat

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/switchboard-code/switchboard/internal/provider"
)

// maxLineBytes bounds one SSE line. A single chunk carrying large tool
// arguments is legitimate, so the ceiling is generous; it exists to stop a
// server that never sends a newline from consuming memory without limit.
const maxLineBytes = 8 << 20

const maxAccumulatedBlocks = 4096

const (
	// Wire envelopes are not output bytes, so they get independent headroom
	// while remaining finite even when a server sends only ignored fields.
	maxAccumulatedWireBytes = 8 * provider.ProviderStreamHardBytes
	maxAccumulatedWireLines = 4 * provider.ProviderStreamMaxEvents
)

// stream decodes server-sent events into canonical events.
//
// Tool calls are the hard part. The format streams them as fragments tagged
// with an index: the name may arrive in one chunk and the arguments across
// several more, so nothing can be emitted until the choice finishes and the
// accumulated argument text parses as JSON.
type stream struct {
	ctx       context.Context
	body      io.ReadCloser
	scanner   *bufio.Scanner
	profile   Profile
	events    *provider.StreamLimiter
	accum     *provider.StreamLimiter
	wireBytes int
	wireLines int

	// name is the provider a failure is attributed to. A profile that names its
	// vendor reports under that name, so an error from OpenAI does not arrive
	// blamed on a generic adapter.
	name string

	pending []provider.Event

	blockIndex int
	blockKind  provider.EventType
	blockOpen  bool
	blocks     int

	tools        map[int]*toolAccum
	sawToolCalls bool
	finishReason string
	usage        provider.Usage

	toolsEmitted bool
	sawFinish    bool
	finished     bool

	// err is raised only after every event already decoded has been handed to
	// the caller, so a malformed tool call does not discard the text that
	// arrived before it.
	err error
}

type toolAccum struct {
	id   string
	name string
	args strings.Builder
}

func newStream(ctx context.Context, body io.ReadCloser, profile Profile, outputTokenAllowance int) *stream {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	return &stream{
		ctx:     ctx,
		body:    body,
		scanner: sc,
		profile: profile,
		events:  provider.NewStreamLimiter(0),
		accum:   provider.NewStreamLimiter(outputTokenAllowance),
		name:    providerName(profile),
		tools:   map[int]*toolAccum{},
	}
}

func (s *stream) Next() (provider.Event, error) {
	for {
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}
		if s.err != nil {
			return provider.Event{}, s.err
		}
		if s.finished {
			return provider.Event{}, io.EOF
		}
		if err := s.readLine(); err != nil {
			return provider.Event{}, err
		}
	}
}

func (s *stream) Close() error { return s.body.Close() }

func (s *stream) readLine() error {
	if !s.scanner.Scan() {
		if err := s.scanner.Err(); err != nil {
			if ctxErr := s.ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return &provider.ProtocolError{Provider: s.name, Detail: "reading the event stream", Err: err}
		}
		if ctxErr := s.ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if s.sawFinish {
			// The turn completed; the server simply did not send [DONE], which
			// several compatible servers omit.
			s.finish()
			return nil
		}
		// The connection ended mid-turn. Whatever the caller already consumed
		// is real output and must not be discarded.
		return provider.ErrStreamIncomplete
	}

	rawLine := s.scanner.Text()
	if err := s.admitWireLine(rawLine); err != nil {
		return err
	}
	line := strings.TrimSpace(rawLine)
	if line == "" || strings.HasPrefix(line, ":") {
		return nil // keep-alive or blank separator
	}
	payload, ok := strings.CutPrefix(line, "data:")
	if !ok {
		// Other SSE fields (event:, id:, retry:) carry nothing this format
		// uses, so they are skipped rather than treated as damage.
		return nil
	}
	payload = strings.TrimSpace(payload)
	if err := s.events.AdmitPayloadBytes(); err != nil {
		return s.limitError("event count")
	}

	if payload == "[DONE]" {
		s.finish()
		return nil
	}

	var chunk chatChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return &provider.ProtocolError{Provider: s.name, Detail: "decoding a stream chunk", Err: err}
	}
	if chunk.Error != nil {
		return &provider.APIError{Provider: s.name, StatusCode: 0, Body: provider.SanitizeAPIErrorText(chunk.Error.Message)}
	}

	if chunk.Usage != nil {
		s.usage.InputTokens = chunk.Usage.PromptTokens
		s.usage.OutputTokens = chunk.Usage.CompletionTokens
		if d := chunk.Usage.PromptTokensDetails; d != nil {
			// The format reports cached prompt tokens as a subset of the
			// prompt count, so the uncached remainder is what is left.
			s.usage.CacheReadTokens = d.CachedTokens
			s.usage.InputTokens -= d.CachedTokens
		}
	}

	for _, choice := range chunk.Choices {
		if reasoning := firstNonEmpty(choice.Delta.Reasoning, choice.Delta.ReasoningContent); reasoning != "" {
			index, err := s.indexFor(provider.EventThinkingDelta)
			if err != nil {
				s.err = err
				return nil
			}
			s.pending = append(s.pending, provider.Event{
				Type:  provider.EventThinkingDelta,
				Index: index,
				Text:  reasoning,
			})
		}
		if choice.Delta.Content != "" {
			index, err := s.indexFor(provider.EventTextDelta)
			if err != nil {
				s.err = err
				return nil
			}
			s.pending = append(s.pending, provider.Event{
				Type:  provider.EventTextDelta,
				Index: index,
				Text:  choice.Delta.Content,
			})
		}
		for _, call := range choice.Delta.ToolCalls {
			if err := s.accumulate(call); err != nil {
				s.err = err
				return nil
			}
		}
		if choice.FinishReason != "" {
			s.finishReason = choice.FinishReason
			s.sawFinish = true
			// Tool calls are complete here, but the terminal event is not
			// emitted yet: the usage chunk arrives after finish_reason on a
			// real server, and reporting a turn's token counts as zero
			// because they had not landed yet is worse than waiting.
			s.emitToolCalls()
		}
	}
	return nil
}

// accumulate folds a tool-call fragment into the call at its index. Ollama
// sends a complete call in one chunk; OpenAI and others split the arguments,
// so both shapes fold the same way.
func (s *stream) accumulate(call wireToolCall) error {
	acc, ok := s.tools[call.Index]
	if !ok {
		if len(s.tools) >= maxAccumulatedBlocks {
			return s.limitError("distinct tool count")
		}
		acc = &toolAccum{}
	}
	if err := s.admitAccumulated(
		growthBeyond(len(acc.id), call.ID),
		growthBeyond(len(acc.name), call.Function.Name),
		call.Function.Arguments,
	); err != nil {
		return err
	}
	if !ok {
		s.tools[call.Index] = acc
	}
	if call.ID != "" {
		acc.id = call.ID
	}
	if call.Function.Name != "" {
		acc.name = call.Function.Name
	}
	acc.args.WriteString(call.Function.Arguments)
	s.sawToolCalls = true
	return nil
}

// emitToolCalls turns the accumulated fragments into canonical calls. It runs
// when the choice finishes, which is the first point at which the argument
// text is known to be complete.
func (s *stream) emitToolCalls() {
	if s.toolsEmitted {
		return
	}
	s.toolsEmitted = true

	indexes := make([]int, 0, len(s.tools))
	for i := range s.tools {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)

	for _, i := range indexes {
		use, err := s.tools[i].toolUse(s.name)
		if err != nil {
			// A malformed call cannot be executed. Reporting it rather than
			// dropping it keeps the turn from continuing as though the model
			// had asked for nothing.
			s.err = err
			return
		}
		index, err := s.newBlock()
		if err != nil {
			s.err = err
			return
		}
		s.pending = append(s.pending, provider.Event{
			Type:    provider.EventToolUse,
			Index:   index,
			ToolUse: use,
		})
	}
}

// finish emits the terminal event. It runs on [DONE], or at end of stream when
// a finish_reason was already seen, so any usage chunk sent after the choice
// finished has been folded in first.
func (s *stream) finish() {
	if s.finished {
		return
	}
	s.emitToolCalls()
	s.finished = true

	if s.err != nil {
		return
	}
	s.pending = append(s.pending, provider.Event{
		Type:       provider.EventDone,
		StopReason: s.stopReason(),
		Usage:      s.usage,
	})
}

func (acc *toolAccum) toolUse(name string) (*provider.ToolUse, error) {
	if acc.name == "" {
		return nil, &provider.ProtocolError{Provider: name, Detail: "tool call with no function name"}
	}

	args := strings.TrimSpace(acc.args.String())
	if args == "" {
		args = "{}"
	}
	if !json.Valid([]byte(args)) {
		// Arguments arrive as a string built from fragments, so an incomplete
		// or malformed accumulation is the failure mode this check exists for.
		return nil, &provider.ProtocolError{
			Provider: name,
			Detail:   fmt.Sprintf("tool call %q carried arguments that are not valid JSON", acc.name),
		}
	}

	id := acc.id
	if id == "" {
		// Some servers omit the ID. The loop correlates results to calls by
		// ID, so one is synthesized from the call's name and position.
		id = "call_" + acc.name
	}
	return &provider.ToolUse{ID: id, Name: acc.name, Input: json.RawMessage(args)}, nil
}

// stopReason maps the format's finish_reason, and corrects for a server that
// reports "stop" on a turn that ended in a tool call. Reporting end_turn there
// would leave the call unexecuted.
func (s *stream) stopReason() provider.StopReason {
	if s.sawToolCalls {
		return provider.StopToolUse
	}
	switch s.finishReason {
	case "tool_calls":
		return provider.StopToolUse
	case "length":
		return provider.StopMaxTokens
	default:
		return provider.StopEndTurn
	}
}

func (s *stream) indexFor(kind provider.EventType) (int, error) {
	if s.blockOpen && s.blockKind == kind {
		return s.blockIndex, nil
	}
	if s.blocks >= maxAccumulatedBlocks {
		return 0, s.limitError("distinct block count")
	}
	if s.blockOpen {
		s.blockIndex++
	}
	s.blockOpen = true
	s.blockKind = kind
	s.blocks++
	return s.blockIndex, nil
}

// newBlock allocates an index for a block that arrives complete, leaving no
// block open so the next delta of any kind starts one of its own.
func (s *stream) newBlock() (int, error) {
	if s.blocks >= maxAccumulatedBlocks {
		return 0, s.limitError("distinct block count")
	}
	if s.blockOpen {
		s.blockIndex++
	}
	idx := s.blockIndex
	s.blockIndex++
	s.blockOpen = false
	s.blocks++
	return idx, nil
}

func (s *stream) limitError(kind string) error {
	return &provider.ProtocolError{
		Provider: s.name,
		Detail:   fmt.Sprintf("event stream exceeded its %s limit", kind),
		Err:      provider.ErrStreamLimit,
	}
}

func (s *stream) admitAccumulated(parts ...string) error {
	sizes := make([]int, len(parts))
	hasPayload := false
	for i, part := range parts {
		sizes[i] = len(part)
		hasPayload = hasPayload || len(part) > 0
	}
	if hasPayload {
		if err := s.accum.AdmitPayloadBytes(sizes...); err != nil {
			return s.limitError("accumulated bytes or fragment count")
		}
	}
	return nil
}

func (s *stream) admitWireLine(line string) error {
	size := len(line) + 1
	if s.wireLines >= maxAccumulatedWireLines {
		return s.limitError("wire line count")
	}
	if s.wireBytes > maxAccumulatedWireBytes || size > maxAccumulatedWireBytes-s.wireBytes {
		return s.limitError("wire bytes")
	}
	s.wireLines++
	s.wireBytes += size
	return nil
}

func growthBeyond(retained int, replacement string) string {
	if len(replacement) <= retained {
		return ""
	}
	return replacement[retained:]
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
