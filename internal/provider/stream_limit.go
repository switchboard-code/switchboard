package provider

import (
	"errors"
	"math"
)

const (
	// ProviderStreamHardBytes is the final backstop for normalized streams. A
	// finite output allowance earns a tighter limit, but unknown and
	// non-conforming providers never get an unbounded byte budget.
	ProviderStreamHardBytes         = 8 << 20
	ProviderStreamMinimumKnownBytes = 64 << 10
	ProviderStreamBytesPerToken     = 64
	ProviderStreamMaxEvents         = 131072
)

// ErrStreamLimit marks a local refusal of an event stream that exceeded its
// cumulative resource budget. Its fixed text deliberately omits provider
// content and the rejected event's shape.
var ErrStreamLimit = errors.New("provider stream exceeded the local safety limit")

// StreamLimiter accounts for a normalized provider stream without buffering
// it. Call Admit immediately after Next and before inspecting, discarding, or
// appending the event. That ordering makes ignored reasoning and forbidden tool
// calls consume the same finite budget as visible text.
type StreamLimiter struct {
	byteLimit  int
	totalBytes int
	events     int
}

// NewStreamLimiter derives a conservative byte budget from a finite output
// allowance. Zero and MaxInt mean unknown; the hard cap still applies.
func NewStreamLimiter(outputTokenAllowance int) *StreamLimiter {
	return &StreamLimiter{byteLimit: StreamByteLimit(outputTokenAllowance)}
}

// StreamByteLimit converts a token allowance into a generous aggregate byte
// ceiling. Signatures and tool arguments are not tokens in every provider's
// accounting, so this intentionally leaves substantial encoding headroom.
func StreamByteLimit(outputTokenAllowance int) int {
	if outputTokenAllowance <= 0 || outputTokenAllowance == math.MaxInt {
		return ProviderStreamHardBytes
	}
	if outputTokenAllowance > ProviderStreamHardBytes/ProviderStreamBytesPerToken {
		return ProviderStreamHardBytes
	}
	limit := outputTokenAllowance * ProviderStreamBytesPerToken
	if limit < ProviderStreamMinimumKnownBytes {
		limit = ProviderStreamMinimumKnownBytes
	}
	if limit > ProviderStreamHardBytes {
		limit = ProviderStreamHardBytes
	}
	return limit
}

// Admit charges every variable-size payload carried by event. It mutates the
// accounting only after the complete event fits, so an over-limit event is
// never partially admitted.
func (l *StreamLimiter) Admit(event Event) error {
	parts := []int{len(event.Text), len(event.Signature)}
	if event.ToolUse != nil {
		parts = append(parts, len(event.ToolUse.ID), len(event.ToolUse.Name), len(event.ToolUse.Input))
	}
	return l.AdmitPayloadBytes(parts...)
}

// AdmitPayloadBytes applies one event admission to several already-decoded
// variable-size fields without joining or copying them. Adapters use this
// before retaining a wire fragment whose fields remain hidden until a later
// normalized event or live in adapter bookkeeping. The mutation is all-or-none.
func (l *StreamLimiter) AdmitPayloadBytes(parts ...int) error {
	if l == nil || l.events >= ProviderStreamMaxEvents {
		return ErrStreamLimit
	}

	remaining := l.byteLimit - l.totalBytes
	eventBytes := 0
	for _, size := range parts {
		if size < 0 || size > remaining-eventBytes {
			return ErrStreamLimit
		}
		eventBytes += size
	}

	l.events++
	l.totalBytes += eventBytes
	return nil
}
