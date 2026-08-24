package openai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func TestResponsesStreamWireLimitAcceptsExactBytesAndRejectsOneMore(t *testing.T) {
	limit := provider.StreamByteLimit(1)
	body := "data: {\"type\":\"response.output_item.added\",\"item\":{\"arguments\":\"" + strings.Repeat("x", limit-1) + "\"}}\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"x\"}\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"x\"}\n"
	stream := newResponsesStream(context.Background(), io.NopCloser(strings.NewReader(body)), 1)
	defer stream.Close()
	if err := stream.readLine(); err != nil {
		t.Fatalf("item start: %v", err)
	}
	if err := stream.readLine(); err != nil {
		t.Fatalf("exact byte limit: %v", err)
	}
	if got := stream.items[""].arguments.Len(); got != limit {
		t.Fatalf("arguments at exact byte limit = %d, want %d", got, limit)
	}
	requireResponsesStreamLimit(t, stream.readLine())
	if got := stream.items[""].arguments.Len(); got != limit {
		t.Fatalf("arguments after refusal = %d, want %d", got, limit)
	}
}

func TestResponsesStreamWireLimitRejectsEventOverCount(t *testing.T) {
	body := strings.Repeat("data: {}\n", provider.ProviderStreamMaxEvents+1)
	stream := newResponsesStream(context.Background(), io.NopCloser(strings.NewReader(body)), 0)
	defer stream.Close()

	for i := 0; i < provider.ProviderStreamMaxEvents; i++ {
		if err := stream.readLine(); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
	}
	requireResponsesStreamLimit(t, stream.readLine())
}

func TestResponsesStreamWireEnvelopeLimitAcceptsExactBytesAndRejectsOneMore(t *testing.T) {
	const line = "data: {}"
	stream := newResponsesStream(context.Background(), io.NopCloser(strings.NewReader(line+"\n"+line+"\n")), 0)
	defer stream.Close()
	stream.wireBytes = maxAccumulatedWireBytes - len(line) - 1

	if err := stream.readLine(); err != nil {
		t.Fatalf("exact wire byte limit: %v", err)
	}
	if stream.wireBytes != maxAccumulatedWireBytes {
		t.Fatalf("wire bytes = %d, want %d", stream.wireBytes, maxAccumulatedWireBytes)
	}
	requireResponsesStreamLimit(t, stream.readLine())
	if stream.wireBytes != maxAccumulatedWireBytes {
		t.Fatalf("wire bytes after refusal = %d, want %d", stream.wireBytes, maxAccumulatedWireBytes)
	}
}

func TestResponsesStreamWireLimitBoundsIgnoredLines(t *testing.T) {
	stream := newResponsesStream(context.Background(), io.NopCloser(strings.NewReader(": ping\n: ping\n")), 0)
	defer stream.Close()
	stream.wireLines = maxAccumulatedWireLines - 1

	if err := stream.readLine(); err != nil {
		t.Fatalf("exact wire line limit: %v", err)
	}
	requireResponsesStreamLimit(t, stream.readLine())
}

func TestResponsesStreamLimitRejectsDistinctItemsBeforeMapGrowth(t *testing.T) {
	var body strings.Builder
	for i := 0; i <= maxAccumulatedResponseItems; i++ {
		fmt.Fprintf(&body, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"item-%d\",\"type\":\"function_call\",\"call_id\":\"call-%d\",\"name\":\"read\",\"arguments\":\"{}\"}}\n", i, i)
	}
	stream := newResponsesStream(context.Background(), io.NopCloser(strings.NewReader(body.String())), 0)
	defer stream.Close()

	for i := 0; i < maxAccumulatedResponseItems; i++ {
		if err := stream.readLine(); err != nil {
			t.Fatalf("item %d: %v", i, err)
		}
	}
	if got := len(stream.items); got != maxAccumulatedResponseItems {
		t.Fatalf("items at exact limit = %d, want %d", got, maxAccumulatedResponseItems)
	}
	if got := len(stream.index); got != maxAccumulatedResponseItems {
		t.Fatalf("indexes at exact limit = %d, want %d", got, maxAccumulatedResponseItems)
	}
	requireResponsesStreamLimit(t, stream.readLine())
	if got := len(stream.items); got != maxAccumulatedResponseItems {
		t.Fatalf("items after refusal = %d, want %d", got, maxAccumulatedResponseItems)
	}
	if got := len(stream.index); got != maxAccumulatedResponseItems {
		t.Fatalf("indexes after refusal = %d, want %d", got, maxAccumulatedResponseItems)
	}
}

func TestResponsesStreamLimitRejectsToolsAtBoundary(t *testing.T) {
	stream := newResponsesStream(context.Background(), io.NopCloser(strings.NewReader("")), 0)
	defer stream.Close()

	for i := 0; i < maxAccumulatedResponseItems; i++ {
		ev := responsesEvent{Type: "response.output_item.done"}
		ev.Item = &struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			Status    string `json:"status"`
			Name      string `json:"name"`
			CallID    string `json:"call_id"`
			Arguments string `json:"arguments"`
		}{ID: fmt.Sprintf("item-%d", i), Type: "function_call", Name: "read", CallID: fmt.Sprintf("call-%d", i), Arguments: "{}"}
		if err := stream.handle(ev); err != nil {
			t.Fatalf("tool %d: %v", i, err)
		}
	}
	if stream.tools != maxAccumulatedResponseItems {
		t.Fatalf("tools at exact limit = %d, want %d", stream.tools, maxAccumulatedResponseItems)
	}
	pending := len(stream.pending)
	ev := responsesEvent{Type: "response.output_item.done"}
	ev.Item = &struct {
		ID        string `json:"id"`
		Type      string `json:"type"`
		Status    string `json:"status"`
		Name      string `json:"name"`
		CallID    string `json:"call_id"`
		Arguments string `json:"arguments"`
	}{ID: "one-over", Type: "function_call", Name: "read", CallID: "one-over", Arguments: "{}"}
	requireResponsesStreamLimit(t, stream.handle(ev))
	if stream.tools != maxAccumulatedResponseItems || len(stream.pending) != pending {
		t.Fatalf("tool refusal mutated state: tools=%d pending=%d, want %d/%d", stream.tools, len(stream.pending), maxAccumulatedResponseItems, pending)
	}
}

func TestResponsesStreamLimitBoundsFragmentedToolArguments(t *testing.T) {
	const start = "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"item\",\"type\":\"function_call\",\"call_id\":\"call\",\"name\":\"read\",\"arguments\":\"\"}}\n"
	delta := "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"item\",\"delta\":\"" + strings.Repeat("x", 1024) + "\"}\n"
	body := start + strings.Repeat(delta, 100)
	stream := newResponsesStream(context.Background(), io.NopCloser(strings.NewReader(body)), 1)
	defer stream.Close()

	_, err := stream.Next()
	requireResponsesStreamLimit(t, err)
	acc := stream.items["item"]
	if acc == nil || acc.arguments.Len() == 0 {
		t.Fatal("tool arguments were not accumulated before the limit")
	}
	if got, limit := acc.arguments.Len(), provider.StreamByteLimit(1); got >= limit {
		t.Fatalf("retained tool arguments = %d bytes, want less than %d", got, limit)
	}
}

func TestResponsesStreamLimitDoesNotDoubleChargeCompleteArguments(t *testing.T) {
	const id, callID, name, kind = "i", "c", "n", "function_call"
	argumentBytes := provider.StreamByteLimit(1) - len(id) - len(callID) - len(name) - len(kind)
	arguments := `{"x":"` + strings.Repeat("x", argumentBytes-len(`{"x":""}`)) + `"}`
	quoted := strconv.Quote(arguments)
	body := fmt.Sprintf("data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":%q,\"type\":%q,\"call_id\":%q,\"name\":%q}}\n", id, kind, callID, name) +
		fmt.Sprintf("data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":%q,\"delta\":%s}\n", id, quoted) +
		fmt.Sprintf("data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":%q,\"type\":%q,\"call_id\":%q,\"name\":%q,\"arguments\":%s}}\n", id, kind, callID, name, quoted) +
		fmt.Sprintf("data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":%q,\"delta\":\"x\"}\n", id)
	stream := newResponsesStream(context.Background(), io.NopCloser(strings.NewReader(body)), 1)
	defer stream.Close()

	for i := 0; i < 3; i++ {
		if err := stream.readLine(); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
	}
	if stream.tools != 1 || len(stream.pending) != 1 {
		t.Fatalf("completed tool state: tools=%d pending=%d, want 1/1", stream.tools, len(stream.pending))
	}
	requireResponsesStreamLimit(t, stream.readLine())
	if got := stream.items[id].arguments.Len(); got != argumentBytes {
		t.Fatalf("arguments after refusal = %d, want %d", got, argumentBytes)
	}
}

func requireResponsesStreamLimit(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, provider.ErrStreamLimit) {
		t.Fatalf("err = %v, want ErrStreamLimit", err)
	}
	var protocolErr *provider.ProtocolError
	if !errors.As(err, &protocolErr) {
		t.Fatalf("err type = %T, want *provider.ProtocolError", err)
	}
	if protocolErr.Provider != Name {
		t.Fatalf("error provider = %q, want %q", protocolErr.Provider, Name)
	}
}
