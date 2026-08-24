package anthropic

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
	body := "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{}}\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"" + strings.Repeat("x", limit) + "\"}}\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"x\"}}\n"
	stream := newStream(context.Background(), io.NopCloser(strings.NewReader(body)), 1)
	defer stream.Close()
	if err := stream.readLine(); err != nil {
		t.Fatalf("block start: %v", err)
	}
	if err := stream.readLine(); err != nil {
		t.Fatalf("exact byte limit: %v", err)
	}
	if got := stream.blocks[0].input.Len(); got != limit {
		t.Fatalf("arguments at exact byte limit = %d, want %d", got, limit)
	}
	requireAnthropicStreamLimit(t, stream.readLine())
	if got := stream.blocks[0].input.Len(); got != limit {
		t.Fatalf("arguments after refusal = %d, want %d", got, limit)
	}
}

func TestStreamWireLimitRejectsEventOverCount(t *testing.T) {
	body := strings.Repeat("data: {}\n", provider.ProviderStreamMaxEvents+1)
	stream := newStream(context.Background(), io.NopCloser(strings.NewReader(body)), 0)
	defer stream.Close()

	for i := 0; i < provider.ProviderStreamMaxEvents; i++ {
		if err := stream.readLine(); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
	}
	requireAnthropicStreamLimit(t, stream.readLine())
}

func TestStreamWireEnvelopeLimitAcceptsExactBytesAndRejectsOneMore(t *testing.T) {
	const line = "data: {}"
	stream := newStream(context.Background(), io.NopCloser(strings.NewReader(line+"\n"+line+"\n")), 0)
	defer stream.Close()
	stream.wireBytes = maxAccumulatedWireBytes - len(line) - 1

	if err := stream.readLine(); err != nil {
		t.Fatalf("exact wire byte limit: %v", err)
	}
	if stream.wireBytes != maxAccumulatedWireBytes {
		t.Fatalf("wire bytes = %d, want %d", stream.wireBytes, maxAccumulatedWireBytes)
	}
	requireAnthropicStreamLimit(t, stream.readLine())
	if stream.wireBytes != maxAccumulatedWireBytes {
		t.Fatalf("wire bytes after refusal = %d, want %d", stream.wireBytes, maxAccumulatedWireBytes)
	}
}

func TestStreamWireLimitBoundsIgnoredLines(t *testing.T) {
	stream := newStream(context.Background(), io.NopCloser(strings.NewReader(": ping\n: ping\n")), 0)
	defer stream.Close()
	stream.wireLines = maxAccumulatedWireLines - 1

	if err := stream.readLine(); err != nil {
		t.Fatalf("exact wire line limit: %v", err)
	}
	requireAnthropicStreamLimit(t, stream.readLine())
}

func TestStreamLimitRejectsDistinctBlocksBeforeMapGrowth(t *testing.T) {
	var body strings.Builder
	for i := 0; i <= maxAccumulatedBlocks; i++ {
		fmt.Fprintf(&body, "data: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call-%d\",\"name\":\"read\"}}\n", i, i)
	}
	stream := newStream(context.Background(), io.NopCloser(strings.NewReader(body.String())), 0)
	defer stream.Close()

	for i := 0; i < maxAccumulatedBlocks; i++ {
		if err := stream.readLine(); err != nil {
			t.Fatalf("block %d: %v", i, err)
		}
	}
	if got := len(stream.blocks); got != maxAccumulatedBlocks {
		t.Fatalf("blocks at exact limit = %d, want %d", got, maxAccumulatedBlocks)
	}
	requireAnthropicStreamLimit(t, stream.readLine())
	if got := len(stream.blocks); got != maxAccumulatedBlocks {
		t.Fatalf("blocks after refusal = %d, want %d", got, maxAccumulatedBlocks)
	}
}

func TestStreamLimitBoundsFragmentedToolArguments(t *testing.T) {
	const start = "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call\",\"name\":\"read\"}}\n"
	delta := "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"" + strings.Repeat("x", 1024) + "\"}}\n"
	body := start + strings.Repeat(delta, 100)
	stream := newStream(context.Background(), io.NopCloser(strings.NewReader(body)), 1)
	defer stream.Close()

	_, err := stream.Next()
	requireAnthropicStreamLimit(t, err)
	acc := stream.blocks[0]
	if acc == nil || acc.input.Len() == 0 {
		t.Fatal("tool arguments were not accumulated before the limit")
	}
	if got, limit := acc.input.Len(), provider.StreamByteLimit(1); got >= limit {
		t.Fatalf("retained tool arguments = %d bytes, want less than %d", got, limit)
	}
}

func requireAnthropicStreamLimit(t *testing.T, err error) {
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
