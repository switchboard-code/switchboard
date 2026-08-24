package provider

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestStreamLimiterAcceptsExactDerivedByteLimitAndRefusesOneMore(t *testing.T) {
	limiter := NewStreamLimiter(1)
	limit := StreamByteLimit(1)
	if limit != ProviderStreamMinimumKnownBytes {
		t.Fatalf("derived limit = %d, want %d", limit, ProviderStreamMinimumKnownBytes)
	}
	if err := limiter.Admit(Event{Type: EventThinkingDelta, Text: strings.Repeat("x", limit-1), Signature: "s"}); err != nil {
		t.Fatalf("exact limit: %v", err)
	}
	if err := limiter.Admit(Event{Type: EventThinkingDelta, Text: "x"}); !errors.Is(err, ErrStreamLimit) {
		t.Fatalf("one byte over: err = %v, want ErrStreamLimit", err)
	}
}

func TestStreamLimiterCountsIgnoredToolPayloadBeforeMutation(t *testing.T) {
	limiter := NewStreamLimiter(1)
	limit := StreamByteLimit(1)
	tool := &ToolUse{ID: "id", Name: "tool", Input: json.RawMessage(strings.Repeat("x", limit-len("id")-len("tool")))}
	if err := limiter.Admit(Event{Type: EventToolUse, ToolUse: tool}); err != nil {
		t.Fatalf("exact tool payload: %v", err)
	}
	tool.Input = append(tool.Input, 'x')
	if err := NewStreamLimiter(1).Admit(Event{Type: EventToolUse, ToolUse: tool}); !errors.Is(err, ErrStreamLimit) {
		t.Fatalf("oversize tool payload: err = %v, want ErrStreamLimit", err)
	}
}

func TestStreamLimiterRefusesEndlessTinyIgnoredEvents(t *testing.T) {
	limiter := NewStreamLimiter(0)
	for i := 0; i < ProviderStreamMaxEvents; i++ {
		if err := limiter.Admit(Event{Type: EventThinkingDelta}); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
	}
	if err := limiter.Admit(Event{Type: EventThinkingDelta}); !errors.Is(err, ErrStreamLimit) {
		t.Fatalf("event over limit: err = %v, want ErrStreamLimit", err)
	}
}

func TestStreamLimiterAdmitsPayloadPartsAsOneEvent(t *testing.T) {
	limiter := NewStreamLimiter(1)
	limit := StreamByteLimit(1)
	if err := limiter.AdmitPayloadBytes(limit/2, limit-limit/2); err != nil {
		t.Fatalf("exact multipart payload: %v", err)
	}
	if err := limiter.AdmitPayloadBytes(1); !errors.Is(err, ErrStreamLimit) {
		t.Fatalf("one byte over: err = %v, want ErrStreamLimit", err)
	}

	limiter = NewStreamLimiter(0)
	if err := limiter.AdmitPayloadBytes(1, 1, 1); err != nil {
		t.Fatalf("multipart event: %v", err)
	}
	for i := 1; i < ProviderStreamMaxEvents; i++ {
		if err := limiter.AdmitPayloadBytes(); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
	}
	if err := limiter.AdmitPayloadBytes(); !errors.Is(err, ErrStreamLimit) {
		t.Fatalf("event after multipart admission = %v, want ErrStreamLimit", err)
	}
}
