package openaicompat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func TestStreamWireLimitAcceptsExactBytesAndRejectsOneMore(t *testing.T) {
	limit := provider.StreamByteLimit(1)
	body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"" + strings.Repeat("x", limit) + "\"}}]}}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"x\"}}]}}]}\n"
	stream := newStream(context.Background(), io.NopCloser(strings.NewReader(body)), Profile{}, 1)
	defer stream.Close()
	if err := stream.readLine(); err != nil {
		t.Fatalf("exact byte limit: %v", err)
	}
	if got := stream.tools[0].args.Len(); got != limit {
		t.Fatalf("arguments at exact byte limit = %d, want %d", got, limit)
	}
	_, err := stream.Next()
	requireCompatibleStreamLimit(t, err)
	if got := stream.tools[0].args.Len(); got != limit {
		t.Fatalf("arguments after refusal = %d, want %d", got, limit)
	}
}

func TestStreamWireLimitRejectsEventOverCount(t *testing.T) {
	body := strings.Repeat("data: {}\n", provider.ProviderStreamMaxEvents+1)
	stream := newStream(context.Background(), io.NopCloser(strings.NewReader(body)), Profile{}, 0)
	defer stream.Close()

	for i := 0; i < provider.ProviderStreamMaxEvents; i++ {
		if err := stream.readLine(); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
	}
	requireCompatibleStreamLimit(t, stream.readLine())
}

func TestStreamWireEnvelopeLimitAcceptsExactBytesAndRejectsOneMore(t *testing.T) {
	const line = "data: {}"
	stream := newStream(context.Background(), io.NopCloser(strings.NewReader(line+"\n"+line+"\n")), Profile{}, 0)
	defer stream.Close()
	stream.wireBytes = maxAccumulatedWireBytes - len(line) - 1

	if err := stream.readLine(); err != nil {
		t.Fatalf("exact wire byte limit: %v", err)
	}
	if stream.wireBytes != maxAccumulatedWireBytes {
		t.Fatalf("wire bytes = %d, want %d", stream.wireBytes, maxAccumulatedWireBytes)
	}
	requireCompatibleStreamLimit(t, stream.readLine())
	if stream.wireBytes != maxAccumulatedWireBytes {
		t.Fatalf("wire bytes after refusal = %d, want %d", stream.wireBytes, maxAccumulatedWireBytes)
	}
}

func TestStreamWireLimitBoundsIgnoredLines(t *testing.T) {
	stream := newStream(context.Background(), io.NopCloser(strings.NewReader(": ping\n: ping\n")), Profile{}, 0)
	defer stream.Close()
	stream.wireLines = maxAccumulatedWireLines - 1

	if err := stream.readLine(); err != nil {
		t.Fatalf("exact wire line limit: %v", err)
	}
	requireCompatibleStreamLimit(t, stream.readLine())
}

func TestStreamLimitRejectsDistinctToolsBeforeMapGrowth(t *testing.T) {
	var body strings.Builder
	for i := 0; i <= maxAccumulatedBlocks; i++ {
		fmt.Fprintf(&body, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":%d,\"id\":\"call-%d\",\"function\":{\"name\":\"read\",\"arguments\":\"{}\"}}]}}]}\n", i, i)
	}
	stream := newStream(context.Background(), io.NopCloser(strings.NewReader(body.String())), Profile{}, 0)
	defer stream.Close()

	for i := 0; i < maxAccumulatedBlocks; i++ {
		if err := stream.readLine(); err != nil {
			t.Fatalf("tool %d: %v", i, err)
		}
	}
	if got := len(stream.tools); got != maxAccumulatedBlocks {
		t.Fatalf("tools at exact limit = %d, want %d", got, maxAccumulatedBlocks)
	}
	_, err := stream.Next()
	requireCompatibleStreamLimit(t, err)
	if got := len(stream.tools); got != maxAccumulatedBlocks {
		t.Fatalf("tools after refusal = %d, want %d", got, maxAccumulatedBlocks)
	}
}

func TestStreamLimitRejectsDistinctBlocksAtBoundary(t *testing.T) {
	stream := newStream(context.Background(), io.NopCloser(strings.NewReader("")), Profile{}, 0)
	defer stream.Close()

	for i := 0; i < maxAccumulatedBlocks; i++ {
		kind := provider.EventTextDelta
		if i%2 == 0 {
			kind = provider.EventThinkingDelta
		}
		if _, err := stream.indexFor(kind); err != nil {
			t.Fatalf("block %d: %v", i, err)
		}
	}
	if stream.blocks != maxAccumulatedBlocks {
		t.Fatalf("blocks at exact limit = %d, want %d", stream.blocks, maxAccumulatedBlocks)
	}
	_, err := stream.indexFor(provider.EventThinkingDelta)
	requireCompatibleStreamLimit(t, err)
	if stream.blocks != maxAccumulatedBlocks {
		t.Fatalf("blocks after refusal = %d, want %d", stream.blocks, maxAccumulatedBlocks)
	}
}

func TestStreamLimitBoundsFragmentedToolArguments(t *testing.T) {
	delta := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call\",\"function\":{\"name\":\"read\",\"arguments\":\"" + strings.Repeat("x", 1024) + "\"}}]}}]}\n"
	stream := newStream(context.Background(), io.NopCloser(strings.NewReader(strings.Repeat(delta, 100))), Profile{}, 1)
	defer stream.Close()

	_, err := stream.Next()
	requireCompatibleStreamLimit(t, err)
	acc := stream.tools[0]
	if acc == nil || acc.args.Len() == 0 {
		t.Fatal("tool arguments were not accumulated before the limit")
	}
	if got, limit := acc.args.Len(), provider.StreamByteLimit(1); got >= limit {
		t.Fatalf("retained tool arguments = %d bytes, want less than %d", got, limit)
	}
}

func TestStreamLimitDoesNotDoubleChargeRepeatedToolMetadata(t *testing.T) {
	stream := newStream(context.Background(), io.NopCloser(strings.NewReader("")), Profile{}, 1)
	defer stream.Close()

	const id, name = "call", "read"
	arguments := strings.Repeat("x", provider.StreamByteLimit(1)-len(id)-len(name))
	call := wireToolCall{Index: 0, ID: id, Function: wireToolCallFunc{Name: name, Arguments: arguments}}
	if err := stream.accumulate(call); err != nil {
		t.Fatalf("exact retained bytes: %v", err)
	}
	call.Function.Arguments = ""
	if err := stream.accumulate(call); err != nil {
		t.Fatalf("repeated retained metadata: %v", err)
	}
	call.ID = ""
	call.Function.Name = ""
	call.Function.Arguments = "x"
	requireCompatibleStreamLimit(t, stream.accumulate(call))
	if got := stream.tools[0].args.Len(); got != len(arguments) {
		t.Fatalf("arguments after refusal = %d, want %d", got, len(arguments))
	}
}

func requireCompatibleStreamLimit(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, provider.ErrStreamLimit) {
		t.Fatalf("err = %v, want ErrStreamLimit", err)
	}
	var protocolErr *provider.ProtocolError
	if !errors.As(err, &protocolErr) {
		t.Fatalf("err type = %T, want *provider.ProtocolError", err)
	}
	if protocolErr.Provider != providerName(Profile{}) {
		t.Fatalf("error provider = %q, want %q", protocolErr.Provider, providerName(Profile{}))
	}
}
