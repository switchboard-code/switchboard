package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

const (
	// Checkpoints contain only deltas, so these bounds cap each pending batch
	// and avoid quadratic log growth. The first event and every tool call are
	// committed immediately. The interval is evaluated when another event
	// arrives (and the tail is always flushed on stream end); it is not a
	// wall-clock latency promise while a provider's Next call is stalled.
	assistantDraftCheckpointBytes  = 4 << 10
	assistantDraftCheckpointEvents = 64
	assistantDraftCheckpointEvery  = 250 * time.Millisecond

	// A provider stream is untrusted even when its request declared an output
	// allowance. The token allowance gives a useful tighter byte ceiling, but
	// it is not a byte contract: signatures and tool calls are also output, and
	// tokenizers do not promise a fixed byte ratio. Sixty-four bytes per token
	// leaves ample room for those encodings, while the absolute caps keep an
	// unknown or non-conforming stream from turning the session WAL into an
	// allocation sink.
	assistantDraftMaxEvents         = provider.ProviderStreamMaxEvents
	assistantDraftMaxBlocks         = 4096
	assistantDraftMaxToolInputBytes = 4 << 20
	assistantDraftLimitAuditMessage = "assistant stream stopped locally after exceeding its bounded draft budget; rejected output was not stored"
)

// ErrStreamDraftLimit marks a local refusal of a provider stream whose
// cumulative draft exceeded a safety limit. It is deliberately not retryable:
// issuing the same request again would invite the same unbounded response.
var ErrStreamDraftLimit = errors.New("assistant stream exceeded the local draft safety limit")

type streamDraftLimitError struct {
	kind  string
	limit int
}

func (e *streamDraftLimitError) Error() string {
	return fmt.Sprintf("%v (%s limit %d)", ErrStreamDraftLimit, e.kind, e.limit)
}

func (e *streamDraftLimitError) Unwrap() error { return ErrStreamDraftLimit }

// streamDraft delays observer delivery until the exact deltas are synced in
// the session WAL. A SIGKILL can therefore lose provider bytes the UI never
// showed, but it cannot lose assistant output the user already saw.
type streamDraft struct {
	session  *session.Session
	observer Observer
	builder  *messageBuilder

	id             string
	pending        []provider.Event
	pendingBytes   int
	lastCheckpoint time.Time
	wrote          bool
	suppressed     bool

	byteLimit    int
	totalBytes   int
	events       int
	blocks       map[int]provider.BlockKind
	outputTokens int
}

func newStreamDraft(sess *session.Session, observer Observer, builder *messageBuilder, outputTokenAllowance int) *streamDraft {
	return &streamDraft{
		session: sess, observer: observer, builder: builder,
		byteLimit:    assistantDraftByteLimit(outputTokenAllowance),
		blocks:       make(map[int]provider.BlockKind),
		outputTokens: finiteOutputTokenAllowance(outputTokenAllowance),
	}
}

func (d *streamDraft) add(event provider.Event) error {
	eventBytes, err := d.admit(event)
	if err != nil {
		if errors.Is(err, ErrStreamDraftLimit) {
			return d.refuse(err)
		}
		return err
	}
	event = cloneDraftEvent(event)
	d.pending = append(d.pending, event)
	d.pendingBytes += eventBytes
	if !d.wrote || event.Type == provider.EventToolUse || len(d.pending) >= assistantDraftCheckpointEvents ||
		d.pendingBytes >= assistantDraftCheckpointBytes || time.Since(d.lastCheckpoint) >= assistantDraftCheckpointEvery {
		return d.flush()
	}
	return nil
}

func (d *streamDraft) flush() error {
	if len(d.pending) == 0 {
		return nil
	}
	id, err := d.session.CheckpointAssistantDraft(d.id, d.pending)
	if err != nil {
		return err
	}
	d.id = id
	d.builder.draftID = id
	// The WAL sync above is the visibility barrier. Preserve provider event
	// order when updating the final builder and releasing observer deltas after
	// it. Keeping undurable events out of the builder removes the need to clone
	// the complete growing message at every checkpoint.
	for _, event := range d.pending {
		switch event.Type {
		case provider.EventThinkingDelta:
			d.builder.delta(event.Index, provider.KindThinking, event.Text)
			d.builder.sign(event.Index, event.Signature)
			d.observer.ThinkingDelta(event.Text)
		case provider.EventTextDelta:
			d.builder.delta(event.Index, provider.KindText, event.Text)
			d.observer.TextDelta(event.Text)
		case provider.EventToolUse:
			d.builder.toolUse(event.Index, *event.ToolUse)
		}
	}
	d.pending = d.pending[:0]
	d.pendingBytes = 0
	d.lastCheckpoint = time.Now()
	d.wrote = true
	return nil
}

func (d *streamDraft) message() provider.Message {
	if !d.wrote || d.suppressed {
		return provider.Message{Role: provider.RoleAssistant}
	}
	// message is materialized once at the provider-call boundary, rather than
	// once per WAL checkpoint.
	return provider.CloneMessage(d.builder.message())
}

func assistantDraftByteLimit(outputTokenAllowance int) int {
	return provider.StreamByteLimit(outputTokenAllowance)
}

func finiteOutputTokenAllowance(outputTokenAllowance int) int {
	if outputTokenAllowance <= 0 || outputTokenAllowance == math.MaxInt {
		return 0
	}
	return outputTokenAllowance
}

func (d *streamDraft) admit(event provider.Event) (int, error) {
	if event.Index < 0 {
		return 0, fmt.Errorf("assistant stream event has a negative block index")
	}

	var kind provider.BlockKind
	parts := []int{len(event.Text), len(event.Signature)}
	switch event.Type {
	case provider.EventTextDelta:
		if event.Text == "" || event.Signature != "" || event.ToolUse != nil {
			return 0, fmt.Errorf("assistant text stream event has invalid fields")
		}
		kind = provider.KindText
	case provider.EventThinkingDelta:
		if event.Text == "" && event.Signature == "" || event.ToolUse != nil {
			return 0, fmt.Errorf("assistant thinking stream event has invalid fields")
		}
		kind = provider.KindThinking
	case provider.EventToolUse:
		if event.ToolUse == nil || event.ToolUse.ID == "" || event.ToolUse.Name == "" || event.Text != "" || event.Signature != "" {
			return 0, fmt.Errorf("assistant tool stream event is incomplete")
		}
		if len(event.ToolUse.Input) > assistantDraftMaxToolInputBytes {
			return 0, &streamDraftLimitError{kind: "tool argument bytes", limit: assistantDraftMaxToolInputBytes}
		}
		kind = provider.KindToolUse
		parts = append(parts, len(event.ToolUse.ID), len(event.ToolUse.Name), len(event.ToolUse.Input))
	default:
		return 0, fmt.Errorf("unsupported assistant stream event %q", event.Type)
	}

	if d.events >= assistantDraftMaxEvents {
		return 0, &streamDraftLimitError{kind: "event count", limit: assistantDraftMaxEvents}
	}
	if existing, ok := d.blocks[event.Index]; ok {
		if existing != kind {
			return 0, fmt.Errorf("assistant stream block %d changed kind", event.Index)
		}
		if kind == provider.KindToolUse {
			return 0, fmt.Errorf("assistant stream tool block %d was emitted twice", event.Index)
		}
	} else if len(d.blocks) >= assistantDraftMaxBlocks {
		return 0, &streamDraftLimitError{kind: "distinct block count", limit: assistantDraftMaxBlocks}
	}

	remaining := d.byteLimit - d.totalBytes
	eventBytes := 0
	for _, size := range parts {
		if size > remaining-eventBytes {
			return 0, &streamDraftLimitError{kind: "aggregate bytes", limit: d.byteLimit}
		}
		eventBytes += size
	}

	d.events++
	d.totalBytes += eventBytes
	if _, ok := d.blocks[event.Index]; !ok {
		d.blocks[event.Index] = kind
	}
	return eventBytes, nil
}

func (d *streamDraft) admitDone(usage provider.Usage) error {
	if err := usage.Validate(); err != nil {
		return fmt.Errorf("assistant stream terminal usage is invalid: %w", err)
	}
	if d.events >= assistantDraftMaxEvents {
		return d.refuse(&streamDraftLimitError{kind: "event count", limit: assistantDraftMaxEvents})
	}
	if d.outputTokens > 0 && usage.OutputTokens > d.outputTokens {
		return d.refuse(&streamDraftLimitError{kind: "reported output tokens", limit: d.outputTokens})
	}
	d.events++
	return nil
}

// refuse makes the already-admitted prefix durable, records one fixed audit
// fact, and suppresses the ordinary full-message finalization. The active WAL
// draft is already an incomplete message on replay; duplicating its complete
// content in an error frame would make the refusal itself another large write.
func (d *streamDraft) refuse(cause error) error {
	if d.suppressed {
		return cause
	}
	d.suppressed = true
	flushErr := d.flush()
	noteErr := d.session.AppendNote("error", assistantDraftLimitAuditMessage)
	return errors.Join(cause, flushErr, noteErr)
}

func cloneDraftEvent(event provider.Event) provider.Event {
	if event.ToolUse != nil {
		use := *event.ToolUse
		use.Input = append(json.RawMessage(nil), event.ToolUse.Input...)
		event.ToolUse = &use
	}
	return event
}
