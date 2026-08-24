package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/switchboard-code/switchboard/internal/provider"
)

const maxStreamFrameBytes = 8 << 20

// stream decodes Ollama's newline-delimited chat response. Each complete
// frame is bounded independently: a response may contain arbitrarily many
// legitimate chunks, while one missing delimiter cannot grow an allocation
// without limit.
type stream struct {
	ctx    context.Context
	body   io.ReadCloser
	reader *bufio.Reader

	pending []provider.Event

	// Ollama sends no block delimiters, so block boundaries are inferred: a
	// change of output kind starts a new block.
	blockIndex   int
	blockKind    provider.EventType
	blockOpen    bool
	toolCallSeq  int
	sawToolCalls bool

	finished bool
}

func newStream(ctx context.Context, body io.ReadCloser) *stream {
	return &stream{ctx: ctx, body: body, reader: bufio.NewReader(body)}
}

func (s *stream) Next() (provider.Event, error) {
	for {
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}
		if s.finished {
			return provider.Event{}, io.EOF
		}
		if err := s.readChunk(); err != nil {
			return provider.Event{}, err
		}
	}
}

func (s *stream) Close() error { return s.body.Close() }

func (s *stream) readChunk() error {
	frame, atEOF, err := readStreamFrame(s.reader)
	if err != nil {
		if ctxErr := s.ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if errors.Is(err, io.EOF) {
			return provider.ErrStreamIncomplete
		}
		return &provider.ProtocolError{Provider: Name, Detail: "reading chat chunk", Err: err}
	}

	var chunk chatChunk
	if err := json.Unmarshal(frame, &chunk); err != nil {
		if atEOF {
			// A final unterminated frame may be valid JSON, but malformed JSON at
			// EOF is a dropped connection rather than a provider-format claim.
			return provider.ErrStreamIncomplete
		}
		return &provider.ProtocolError{Provider: Name, Detail: "decoding chat chunk", Err: err}
	}

	if chunk.Error != "" {
		return &provider.APIError{Provider: Name, StatusCode: 0, Body: provider.SanitizeAPIErrorText(chunk.Error)}
	}

	if t := chunk.Message.Thinking; t != "" {
		s.pending = append(s.pending, provider.Event{
			Type:  provider.EventThinkingDelta,
			Index: s.indexFor(provider.EventThinkingDelta),
			Text:  t,
		})
	}
	if c := chunk.Message.Content; c != "" {
		s.pending = append(s.pending, provider.Event{
			Type:  provider.EventTextDelta,
			Index: s.indexFor(provider.EventTextDelta),
			Text:  c,
		})
	}
	for _, call := range chunk.Message.ToolCalls {
		use, err := s.toolUse(call)
		if err != nil {
			return err
		}
		s.sawToolCalls = true
		s.pending = append(s.pending, provider.Event{
			Type:    provider.EventToolUse,
			Index:   s.newBlock(),
			ToolUse: use,
		})
	}

	if chunk.Done {
		s.finished = true
		s.pending = append(s.pending, provider.Event{
			Type:       provider.EventDone,
			StopReason: s.stopReason(chunk.DoneReason),
			Usage: provider.Usage{
				InputTokens:  chunk.PromptEvalCount,
				OutputTokens: chunk.EvalCount,
			},
		})
	}
	return nil
}

func readStreamFrame(r *bufio.Reader) ([]byte, bool, error) {
	frame := make([]byte, 0, 64<<10)
	for {
		fragment, err := r.ReadSlice('\n')
		if len(frame)+len(fragment) > maxStreamFrameBytes {
			return nil, false, fmt.Errorf("chat chunk exceeded the %d-byte frame limit", maxStreamFrameBytes)
		}
		frame = append(frame, fragment...)
		switch {
		case err == nil:
			frame = bytes.TrimSuffix(frame, []byte{'\n'})
			frame = bytes.TrimSuffix(frame, []byte{'\r'})
			if len(bytes.TrimSpace(frame)) == 0 {
				frame = frame[:0]
				continue
			}
			return frame, false, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(frame) == 0 {
				return nil, true, io.EOF
			}
			return frame, true, nil
		default:
			return nil, false, err
		}
	}
}

func (s *stream) toolUse(call wireToolCall) (*provider.ToolUse, error) {
	if call.Function.Name == "" {
		return nil, &provider.ProtocolError{Provider: Name, Detail: "tool call with no function name"}
	}
	args := call.Function.Arguments
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if !json.Valid(args) {
		return nil, &provider.ProtocolError{
			Provider: Name,
			Detail:   fmt.Sprintf("tool call %q carried malformed arguments", call.Function.Name),
		}
	}

	id := call.ID
	if id == "" {
		// Some models and older servers omit the ID. The loop correlates results
		// to calls by ID, so one is synthesized from the call's position, which
		// is stable for the life of the stream and of the session log.
		id = fmt.Sprintf("call_%s_%d", call.Function.Name, s.toolCallSeq)
	}
	s.toolCallSeq++

	return &provider.ToolUse{ID: id, Name: call.Function.Name, Input: args}, nil
}

// stopReason corrects for Ollama reporting done_reason "stop" on a turn that
// ended in a tool call. Treating that as end_turn would end the agent loop with
// the call unexecuted.
func (s *stream) stopReason(doneReason string) provider.StopReason {
	if s.sawToolCalls {
		return provider.StopToolUse
	}
	if doneReason == "length" {
		return provider.StopMaxTokens
	}
	return provider.StopEndTurn
}

func (s *stream) indexFor(kind provider.EventType) int {
	if s.blockOpen && s.blockKind == kind {
		return s.blockIndex
	}
	if s.blockOpen {
		s.blockIndex++
	}
	s.blockOpen = true
	s.blockKind = kind
	return s.blockIndex
}

// newBlock allocates an index for a block that arrives complete, and leaves no
// block open so the next delta of any kind starts one of its own.
func (s *stream) newBlock() int {
	if s.blockOpen {
		s.blockIndex++
	}
	idx := s.blockIndex
	s.blockIndex++
	s.blockOpen = false
	return idx
}
