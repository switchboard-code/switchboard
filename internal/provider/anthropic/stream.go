package anthropic

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/switchboard-code/switchboard/internal/provider"
)

// maxLineBytes bounds one event. A single event carrying large tool arguments
// is legitimate, so the ceiling is generous; it exists to stop a server that
// never sends a newline from consuming memory without limit.
const maxLineBytes = 8 << 20

// maxAccumulatedBlocks matches the agent's durable draft bound. Wire events
// which have not produced a canonical event yet still need their own ceiling:
// otherwise a server can grow the adapter's block map indefinitely while the
// outer stream limiter has nothing to observe.
const maxAccumulatedBlocks = 4096

const (
	// Transport syntax gets a separate generous budget: it is not model output,
	// but an endless stream of ignored envelopes still needs a finite bound.
	maxAccumulatedWireBytes = 8 * provider.ProviderStreamHardBytes
	maxAccumulatedWireLines = 4 * provider.ProviderStreamMaxEvents
)

// stream decodes server-sent events into canonical events.
//
// This format is kinder than the compatible one in two ways that matter. Blocks
// carry an explicit index, so nothing has to be inferred from the order deltas
// arrive in. And the terminal event states its own stop reason, so it does not
// have to be recovered from whether tool calls were seen.
//
// The trap is elsewhere: usage arrives in pieces. message_start carries the
// input and cache counts with a placeholder output count, and message_delta
// carries the final figures. Taking either alone reports a turn wrong.
type stream struct {
	ctx       context.Context
	body      io.ReadCloser
	scanner   *bufio.Scanner
	events    *provider.StreamLimiter
	accum     *provider.StreamLimiter
	wireBytes int
	wireLines int

	pending []provider.Event

	// blocks tracks the open content block at each index. Tool calls are
	// assembled here and emitted whole at content_block_stop, because partial
	// JSON is not a tool call.
	blocks map[int]*blockAccum

	usage      provider.Usage
	stopReason string

	sawMessageStop bool
	finished       bool
	err            error
}

type blockAccum struct {
	kind  string
	id    string
	name  string
	input strings.Builder
}

func newStream(ctx context.Context, body io.ReadCloser, outputTokenAllowance int) *stream {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	return &stream{
		ctx:     ctx,
		body:    body,
		scanner: sc,
		events:  provider.NewStreamLimiter(0),
		accum:   provider.NewStreamLimiter(outputTokenAllowance),
		blocks:  map[int]*blockAccum{},
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
			return &provider.ProtocolError{Provider: Name, Detail: "reading the event stream", Err: err}
		}
		if ctxErr := s.ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if s.sawMessageStop {
			s.finish()
			return nil
		}
		// The connection ended mid-turn. Whatever the caller already consumed is
		// real output and must not be discarded.
		return provider.ErrStreamIncomplete
	}

	rawLine := s.scanner.Text()
	if err := s.admitWireLine(rawLine); err != nil {
		return err
	}
	line := strings.TrimSpace(rawLine)
	if line == "" || strings.HasPrefix(line, ":") {
		return nil
	}
	payload, ok := strings.CutPrefix(line, "data:")
	if !ok {
		// The `event:` line names a type the payload also carries, so the body
		// is what gets trusted and the rest is skipped rather than parsed twice.
		return nil
	}
	payload = strings.TrimSpace(payload)
	// Count the wire event before decoding it. Bytes are charged separately,
	// immediately before a decoded value is retained; charging JSON syntax here
	// would reject valid highly-fragmented output on envelope overhead alone.
	if err := s.events.AdmitPayloadBytes(); err != nil {
		return s.limitError("event count")
	}

	var ev streamEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return &provider.ProtocolError{Provider: Name, Detail: "decoding a stream event", Err: err}
	}
	return s.handle(ev)
}

func (s *stream) handle(ev streamEvent) error {
	switch ev.Type {
	case "error":
		msg := "the server reported an error with no message"
		if ev.Error != nil {
			msg = ev.Error.Message
		}
		return &provider.APIError{Provider: Name, StatusCode: 0, Body: provider.SanitizeAPIErrorText(msg)}

	case "message_start":
		if ev.Message != nil {
			s.applyUsage(ev.Message.Usage)
		}

	case "content_block_start":
		if ev.ContentBlock == nil {
			return nil
		}
		previous, exists := s.blocks[ev.Index]
		if !exists && len(s.blocks) >= maxAccumulatedBlocks {
			return s.limitError("distinct block count")
		}
		parts := []string{ev.ContentBlock.Type, ev.ContentBlock.ID, ev.ContentBlock.Name}
		if previous != nil {
			parts = []string{
				growthBeyond(len(previous.kind), ev.ContentBlock.Type),
				growthBeyond(len(previous.id), ev.ContentBlock.ID),
				growthBeyond(len(previous.name), ev.ContentBlock.Name),
			}
		}
		if err := s.admitAccumulated(parts...); err != nil {
			return err
		}
		acc := &blockAccum{
			kind: ev.ContentBlock.Type,
			id:   ev.ContentBlock.ID,
			name: ev.ContentBlock.Name,
		}
		s.blocks[ev.Index] = acc
		// A text or thinking block may open with content already in it.
		if ev.ContentBlock.Text != "" {
			s.emitText(ev.Index, provider.EventTextDelta, ev.ContentBlock.Text, "")
		}
		if ev.ContentBlock.Thinking != "" {
			s.emitText(ev.Index, provider.EventThinkingDelta, ev.ContentBlock.Thinking, "")
		}

	case "content_block_delta":
		if ev.Delta == nil {
			return nil
		}
		switch ev.Delta.Type {
		case "text_delta":
			s.emitText(ev.Index, provider.EventTextDelta, ev.Delta.Text, "")
		case "thinking_delta":
			s.emitText(ev.Index, provider.EventThinkingDelta, ev.Delta.Thinking, "")
		case "signature_delta":
			// The signature covers the finished block and carries no text. It
			// rides a thinking event with an empty body so the assembled message
			// keeps it: replaying a thinking block without it is rejected.
			s.emitText(ev.Index, provider.EventThinkingDelta, "", ev.Delta.Signature)
		case "input_json_delta":
			if acc, ok := s.blocks[ev.Index]; ok {
				if err := s.admitAccumulated(ev.Delta.PartialJSON); err != nil {
					return err
				}
				acc.input.WriteString(ev.Delta.PartialJSON)
			}
		}

	case "content_block_stop":
		acc, ok := s.blocks[ev.Index]
		if !ok || acc.kind != "tool_use" {
			return nil
		}
		use, err := acc.toolUse()
		if err != nil {
			// A malformed call cannot be executed. Raising it rather than
			// dropping it keeps the turn from continuing as though the model had
			// asked for nothing, and it is raised only after the events already
			// decoded have been handed to the caller.
			s.err = err
			return nil
		}
		s.pending = append(s.pending, provider.Event{
			Type: provider.EventToolUse, Index: ev.Index, ToolUse: use,
		})

	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			s.stopReason = ev.Delta.StopReason
		}
		// The final counts land here, including the input and cache figures that
		// message_start reported with a placeholder output count.
		s.applyUsage(ev.Usage)

	case "message_stop":
		s.sawMessageStop = true
		s.finish()
	}
	return nil
}

func (s *stream) limitError(kind string) error {
	return &provider.ProtocolError{
		Provider: Name,
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
	size := len(line) + 1 // Include the scanner-stripped line ending.
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

func (s *stream) emitText(index int, kind provider.EventType, text, signature string) {
	if text == "" && signature == "" {
		return
	}
	s.pending = append(s.pending, provider.Event{
		Type: kind, Index: index, Text: text, Signature: signature,
	})
}

// applyUsage folds in whichever counts an event carried.
//
// Fields are merged rather than replaced because the two events that report
// usage each carry only part of it. A replace would zero the cache counts from
// message_start when message_delta arrives, which is the difference between
// observing a cache and believing there was none (§6.3).
func (s *stream) applyUsage(u *wireUsage) {
	if u == nil {
		return
	}
	if u.InputTokens != 0 {
		s.usage.InputTokens = u.InputTokens
	}
	if u.OutputTokens != 0 {
		s.usage.OutputTokens = u.OutputTokens
	}
	if u.CacheReadInputTokens != 0 {
		s.usage.CacheReadTokens = u.CacheReadInputTokens
	}
	if u.CacheCreationInputTokens != 0 {
		s.usage.CacheWriteTokens = u.CacheCreationInputTokens
	}
}

func (s *stream) finish() {
	if s.finished {
		return
	}
	s.finished = true
	if s.err != nil {
		return
	}
	s.pending = append(s.pending, provider.Event{
		Type:       provider.EventDone,
		StopReason: s.stop(),
		Usage:      s.usage,
	})
}

// stop maps the reported reason. Unlike the compatible format, this target
// reports tool_use on a turn that ended in a call, so the reason is trusted
// rather than reconstructed.
func (s *stream) stop() provider.StopReason {
	switch s.stopReason {
	case "tool_use":
		return provider.StopToolUse
	case "max_tokens":
		return provider.StopMaxTokens
	default:
		return provider.StopEndTurn
	}
}

func (acc *blockAccum) toolUse() (*provider.ToolUse, error) {
	if acc.name == "" {
		return nil, &provider.ProtocolError{Provider: Name, Detail: "tool call with no name"}
	}
	input := strings.TrimSpace(acc.input.String())
	if input == "" {
		// A call with no arguments streams no partial JSON at all, which is not
		// the same as a call whose arguments failed to arrive.
		input = "{}"
	}
	if !json.Valid([]byte(input)) {
		return nil, &provider.ProtocolError{
			Provider: Name,
			Detail:   fmt.Sprintf("tool call %q carried arguments that are not valid JSON", acc.name),
		}
	}
	return &provider.ToolUse{ID: acc.id, Name: acc.name, Input: json.RawMessage(input)}, nil
}
