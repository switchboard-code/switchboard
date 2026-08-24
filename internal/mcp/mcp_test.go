package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// fakeTransport is an in-memory wire, with a scripted server on the far end.
type fakeTransport struct {
	toServer   chan []byte
	fromServer chan []byte
	closed     chan struct{}
}

type fatalReadTransport struct {
	fail      chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
}

type blockedServerReplyTransport struct {
	incoming  chan []byte
	closed    chan struct{}
	started   chan struct{}
	sendDone  chan error
	startOnce sync.Once
	closeOnce sync.Once
}

func newBlockedServerReplyTransport(capacity int) *blockedServerReplyTransport {
	return &blockedServerReplyTransport{
		incoming: make(chan []byte, capacity),
		closed:   make(chan struct{}),
		started:  make(chan struct{}),
		sendDone: make(chan error, capacity),
	}
}

func (t *blockedServerReplyTransport) Send(ctx context.Context, _ []byte) error {
	t.startOnce.Do(func() { close(t.started) })
	select {
	case <-ctx.Done():
		t.sendDone <- ctx.Err()
		return ctx.Err()
	case <-t.closed:
		err := errors.New("closed")
		t.sendDone <- err
		return err
	}
}

func (t *blockedServerReplyTransport) Recv() ([]byte, error) {
	select {
	case message := <-t.incoming:
		return message, nil
	case <-t.closed:
		return nil, errors.New("closed")
	}
}

func (t *blockedServerReplyTransport) Close() error {
	t.closeOnce.Do(func() { close(t.closed) })
	return nil
}

func (t *fatalReadTransport) Send(context.Context, []byte) error { return nil }
func (t *fatalReadTransport) Recv() ([]byte, error) {
	<-t.fail
	return nil, errors.New("fatal read")
}
func (t *fatalReadTransport) Close() error {
	t.closeOnce.Do(func() { close(t.closed) })
	return nil
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		toServer:   make(chan []byte, 16),
		fromServer: make(chan []byte, 16),
		closed:     make(chan struct{}),
	}
}

func (f *fakeTransport) Send(ctx context.Context, msg []byte) error {
	select {
	case f.toServer <- append([]byte(nil), msg...):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-f.closed:
		return errors.New("closed")
	}
}

func (f *fakeTransport) Recv() ([]byte, error) {
	select {
	case msg := <-f.fromServer:
		return msg, nil
	case <-f.closed:
		return nil, errors.New("closed")
	}
}

func (f *fakeTransport) Close() error {
	select {
	case <-f.closed:
	default:
		close(f.closed)
	}
	return nil
}

// serveScript answers initialize and tools/list with canned data and
// tools/call through the supplied function. A nil onCall, or one returning
// the empty string, leaves the call unanswered, which is how a test models a
// server that hangs. It stops when the transport closes.
func serveScript(f *fakeTransport, tools []ToolInfo, onCall func(name string, args json.RawMessage) string) {
	for {
		var raw []byte
		select {
		case raw = <-f.toServer:
		case <-f.closed:
			return
		}
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(raw, &req) != nil || req.ID == nil {
			continue // notification
		}
		var result string
		switch req.Method {
		case "initialize":
			result = `{"protocolVersion":"2025-06-18","serverInfo":{"name":"fake","version":"1.0"}}`
		case "tools/list":
			b, _ := json.Marshal(struct {
				Tools []ToolInfo `json:"tools"`
			}{tools})
			result = string(b)
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if onCall == nil {
				continue
			}
			if result = onCall(p.Name, p.Arguments); result == "" {
				continue
			}
		default:
			result = "{}"
		}
		var msg string
		if rpcError, ok := strings.CutPrefix(result, "ERROR:"); ok {
			msg = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":%s}`, *req.ID, rpcError)
		} else {
			msg = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, *req.ID, result)
		}
		f.fromServer <- []byte(msg)
	}
}

func connectFake(t *testing.T, tools []ToolInfo, onCall func(string, json.RawMessage) string) (*Client, *fakeTransport) {
	return connectFakeSpec(t, Spec{Name: "fake", Command: "unused"}, tools, onCall)
}

func connectFakeSpec(t *testing.T, spec Spec, tools []ToolInfo, onCall func(string, json.RawMessage) string) (*Client, *fakeTransport) {
	t.Helper()
	f := newFakeTransport()
	go serveScript(f, tools, onCall)

	c := &Client{
		spec:      spec,
		logf:      func(string, string) {},
		transport: f,
		pending:   map[int64]chan rpcResponse{},
	}
	go c.readLoop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.listTools(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, f
}

func paginatedToolClient(t *testing.T, page func(cursor string, request int) string) (*Client, *fakeTransport) {
	t.Helper()
	f := newFakeTransport()
	go func() {
		request := 0
		for {
			select {
			case raw := <-f.toServer:
				var req struct {
					ID     *int64 `json:"id"`
					Method string `json:"method"`
					Params struct {
						Cursor string `json:"cursor"`
					} `json:"params"`
				}
				if json.Unmarshal(raw, &req) != nil || req.ID == nil || req.Method != "tools/list" {
					continue
				}
				request++
				f.fromServer <- []byte(fmt.Sprintf(
					`{"jsonrpc":"2.0","id":%d,"result":%s}`,
					*req.ID,
					page(req.Params.Cursor, request),
				))
			case <-f.closed:
				return
			}
		}
	}()
	c := newClient(Spec{Name: "pages"}, f, nil)
	t.Cleanup(func() { _ = c.Close() })
	return c, f
}

var echoTool = []ToolInfo{{
	Name:        "echo",
	Description: "echoes",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
}}

func TestConnectDiscoversTools(t *testing.T) {
	c, _ := connectFake(t, echoTool, nil)

	tools := c.Tools()
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v, want the served echo tool", tools)
	}
	if !strings.Contains(c.ServerLine(), "fake 1.0") || !strings.Contains(c.ServerLine(), "2025-06-18") {
		t.Errorf("ServerLine() = %q, want name, version, and protocol", c.ServerLine())
	}
}

func TestListToolsRejectsRepeatedCursor(t *testing.T) {
	c, _ := paginatedToolClient(t, func(_ string, request int) string {
		return fmt.Sprintf(`{"tools":[{"name":"tool-%d"}],"nextCursor":"loop"}`, request)
	})
	err := c.listToolsWithLimits(context.Background(), listToolsLimits{pages: 10, tools: 10, bytes: 4 << 10})
	if err == nil || !strings.Contains(err.Error(), "repeated cursor") {
		t.Fatalf("listTools error = %v, want repeated-cursor rejection", err)
	}
	if got := c.Tools(); len(got) != 0 {
		t.Fatalf("failed pagination published partial tools: %+v", got)
	}
}

func TestListToolsBoundsPagesBeforeAnotherRequest(t *testing.T) {
	requests := 0
	c, _ := paginatedToolClient(t, func(_ string, request int) string {
		requests = request
		return fmt.Sprintf(`{"tools":[],"nextCursor":"cursor-%d"}`, request)
	})
	err := c.listToolsWithLimits(context.Background(), listToolsLimits{pages: 2, tools: 10, bytes: 4 << 10})
	if err == nil || !strings.Contains(err.Error(), "2 pages") {
		t.Fatalf("listTools error = %v, want page bound", err)
	}
	if requests != 2 {
		t.Fatalf("tools/list requests = %d, want no request beyond page bound", requests)
	}
}

func TestListToolsBoundsTotalToolCount(t *testing.T) {
	c, _ := paginatedToolClient(t, func(string, int) string {
		return `{"tools":[{"name":"one"},{"name":"two"},{"name":"three"}]}`
	})
	err := c.listToolsWithLimits(context.Background(), listToolsLimits{pages: 2, tools: 2, bytes: 4 << 10})
	if err == nil || !strings.Contains(err.Error(), "2 tools") {
		t.Fatalf("listTools error = %v, want tool-count bound", err)
	}
}

func TestListToolsBoundsAggregateBytes(t *testing.T) {
	c, _ := paginatedToolClient(t, func(string, int) string {
		return `{"tools":[]}`
	})
	err := c.listToolsWithLimits(context.Background(), listToolsLimits{pages: 2, tools: 2, bytes: 8})
	if err == nil || !strings.Contains(err.Error(), "8 bytes") {
		t.Fatalf("listTools error = %v, want aggregate-byte bound", err)
	}
}

func TestUnsupportedVersionErrorRedactsRawSupportedSecrets(t *testing.T) {
	const secret = "unsupported-version-secret"
	rpcErr := sanitizeRPCError(&RPCError{
		Code:    -32022,
		Message: "unsupported",
		Data:    json.RawMessage(`{"supported":["unsupported-version-secret"]}`),
	}, []string{secret})
	_, err := negotiationFromUnsupported(rpcErr)
	if err == nil {
		t.Fatal("unsupported version unexpectedly negotiated")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("protocol version error leaked configured secret: %v", err)
	}
}

func TestToolFiltersApplyToDiscoveryAndDirectCalls(t *testing.T) {
	tools := []ToolInfo{{Name: "allowed"}, {Name: "denied"}, {Name: "unlisted"}}
	called := make(chan string, 1)
	c, _ := connectFakeSpec(t, Spec{
		Name:             "filtered",
		Command:          "unused",
		EnabledTools:     []string{"allowed", "denied"},
		EnabledToolsSet:  true,
		DisabledTools:    []string{"denied"},
		DisabledToolsSet: true,
	}, tools, func(name string, _ json.RawMessage) string {
		called <- name
		return `{"content":[]}`
	})
	if got := c.Tools(); len(got) != 1 || got[0].Name != "allowed" {
		t.Fatalf("filtered tools = %+v, want only allowed", got)
	}
	for _, name := range []string{"denied", "unlisted"} {
		_, err := c.Call(context.Background(), name, nil)
		var filtered *ToolFilteredError
		if !errors.As(err, &filtered) || filtered.Tool != name {
			t.Fatalf("Call(%q) error = %v, want ToolFilteredError", name, err)
		}
	}
	select {
	case name := <-called:
		t.Fatalf("filtered tool %q reached the server", name)
	default:
	}
}

func TestExplicitEmptyEnabledToolsExposesAndCallsNothing(t *testing.T) {
	c, _ := connectFakeSpec(t, Spec{
		Name:            "empty-allowlist",
		Command:         "unused",
		EnabledToolsSet: true,
	}, echoTool, nil)
	if got := c.Tools(); len(got) != 0 {
		t.Fatalf("tools = %+v, want none", got)
	}
	if _, err := c.Call(context.Background(), "echo", nil); err == nil {
		t.Fatal("call bypassed explicit empty enabled-tool filter")
	}
}

func TestToolTimeoutAndEarlierCallerDeadline(t *testing.T) {
	c, _ := connectFake(t, echoTool, func(string, json.RawMessage) string { return "" })
	c.spec.ToolTimeout = 40 * time.Millisecond
	started := time.Now()
	_, err := c.Call(context.Background(), "echo", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("configured timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("configured tool timeout took %v", elapsed)
	}

	c.spec.ToolTimeout = 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started = time.Now()
	_, err = c.Call(ctx, "echo", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("earlier caller deadline took %v", elapsed)
	}
}

func TestCallFlattensContentAndPassesErrors(t *testing.T) {
	c, _ := connectFake(t, echoTool, func(name string, args json.RawMessage) string {
		switch name {
		case "mixed":
			// "..." is not base64, which is the shape of a server claiming a
			// picture and not sending one.
			return `{"content":[{"type":"text","text":"hello "},{"type":"image","data":"..."},{"type":"text","text":"world"}]}`
		case "screenshot":
			return `{"content":[{"type":"text","text":"captured"},{"type":"image","mimeType":"image/png","data":"aGVsbG8="}]}`
		case "audio":
			return `{"content":[{"type":"audio","data":"aGVsbG8="}]}`
		case "failing":
			return `{"content":[{"type":"text","text":"it broke"}],"isError":true}`
		}
		return `{"content":[]}`
	})

	ctx := context.Background()
	res, err := c.Call(ctx, "mixed", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "hello [image content the server sent could not be decoded]world" || res.IsError {
		t.Errorf("mixed call = %+v", res)
	}
	if len(res.Images) != 0 {
		t.Errorf("an undecodable block became an image: %+v", res.Images)
	}

	// The case the flattening was losing: a screenshot is the answer to the
	// call that asked for one, and it comes out of the client rather than
	// becoming a note where the picture was.
	res, err = c.Call(ctx, "screenshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 1 {
		t.Fatalf("screenshot call = %+v, want the image carried out", res)
	}
	if res.Images[0].MediaType != "image/png" || string(res.Images[0].Data) != "hello" {
		t.Errorf("image = %+v, want the decoded bytes and its media type", res.Images[0])
	}
	if res.Content != "captured" {
		t.Errorf("content = %q, want the text without a placeholder for the picture", res.Content)
	}

	// Everything else still says what it was rather than being guessed at.
	res, err = c.Call(ctx, "audio", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "[audio content omitted]" {
		t.Errorf("audio call = %+v, want the block named", res)
	}

	res, err = c.Call(ctx, "failing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || res.Content != "it broke" {
		t.Errorf("failing call = %+v, want the server's error text with IsError", res)
	}
}

func TestProtocolRefusalIsAToolError(t *testing.T) {
	f := newFakeTransport()
	go func() {
		for {
			var raw []byte
			select {
			case raw = <-f.toServer:
			case <-f.closed:
				return
			}
			var req struct {
				ID     *int64 `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(raw, &req) != nil || req.ID == nil {
				continue
			}
			switch req.Method {
			case "initialize":
				f.fromServer <- []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"fake"}}}`, *req.ID))
			case "tools/list":
				f.fromServer <- []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"tools":[]}}`, *req.ID))
			default:
				f.fromServer <- []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32602,"message":"unknown tool"}}`, *req.ID))
			}
		}
	}()

	c := &Client{spec: Spec{Name: "fake"}, logf: func(string, string) {}, transport: f, pending: map[int64]chan rpcResponse{}}
	go c.readLoop()
	t.Cleanup(func() { c.Close() })

	ctx := context.Background()
	if err := c.initialize(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := c.Call(ctx, "nope", nil)
	if err != nil {
		t.Fatalf("a protocol refusal must not be a transport error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "unknown tool") {
		t.Errorf("res = %+v, want the refusal as a tool error the model can read", res)
	}
}

func TestProtocolRefusalPreservesTypedErrorData(t *testing.T) {
	c, _ := connectFake(t, echoTool, func(string, json.RawMessage) string {
		return `ERROR:{"code":-32602,"message":"invalid arguments","data":{"field":"path"}}`
	})

	result, err := c.Call(context.Background(), "echo", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || result.RPCError == nil || result.RPCError.Code != -32602 {
		t.Fatalf("result = %+v, want typed JSON-RPC refusal", result)
	}
	var data struct {
		Field string `json:"field"`
	}
	if err := json.Unmarshal(result.RPCError.Data, &data); err != nil || data.Field != "path" {
		t.Fatalf("RPC error data = %s, %v", result.RPCError.Data, err)
	}
}

func TestServerRequestsAreAnswered(t *testing.T) {
	f := newFakeTransport()
	// Serve exactly the handshake, then stop reading: after this the test
	// owns both channels, so nothing races it for the client's replies.
	served := make(chan struct{})
	go func() {
		defer close(served)
		answered := 0
		for answered < 2 {
			raw := <-f.toServer
			var req struct {
				ID     *int64 `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(raw, &req) != nil || req.ID == nil {
				continue
			}
			switch req.Method {
			case "initialize":
				f.fromServer <- []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"fake"}}}`, *req.ID))
			case "tools/list":
				f.fromServer <- []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"tools":[]}}`, *req.ID))
			}
			answered++
		}
	}()

	c := &Client{spec: Spec{Name: "fake"}, logf: func(string, string) {}, transport: f, pending: map[int64]chan rpcResponse{}}
	go c.readLoop()
	t.Cleanup(func() { c.Close() })

	ctx := context.Background()
	if err := c.initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.listTools(ctx); err != nil {
		t.Fatal(err)
	}
	<-served

	// A ping is answered with an empty result.
	f.fromServer <- []byte(`{"jsonrpc":"2.0","id":900,"method":"ping"}`)
	reply := <-f.toServer
	if !strings.Contains(string(reply), `"id":900`) || !strings.Contains(string(reply), `"result"`) {
		t.Errorf("ping reply = %s, want an empty result", reply)
	}

	// Sampling would spend the user's model budget on the server's behalf;
	// it is refused, not ignored, so the server is not left hanging.
	f.fromServer <- []byte(`{"jsonrpc":"2.0","id":901,"method":"sampling/createMessage","params":{}}`)
	reply = <-f.toServer
	if !strings.Contains(string(reply), `"id":901`) || !strings.Contains(string(reply), "-32601") {
		t.Errorf("sampling reply = %s, want method-not-found", reply)
	}

	// String IDs are valid JSON-RPC IDs and must be echoed as strings, not
	// coerced into the client's integer request-id namespace.
	f.fromServer <- []byte(`{"jsonrpc":"2.0","id":"server/ping-雪","method":"ping"}`)
	reply = <-f.toServer
	var stringReply struct {
		ID     string         `json:"id"`
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(reply, &stringReply); err != nil {
		t.Fatalf("string-id ping reply = %s: %v", reply, err)
	}
	if stringReply.ID != "server/ping-雪" || stringReply.Result == nil {
		t.Fatalf("string-id ping reply = %s, want exact string ID and result", reply)
	}
}

func TestJSONRPCIDValidationSeparatesClientAndServerNamespaces(t *testing.T) {
	for _, test := range []struct {
		raw          string
		serverValid  bool
		clientValid  bool
		clientNumber int64
	}{
		{raw: `"string-id"`, serverValid: true},
		{raw: `42`, serverValid: true, clientValid: true, clientNumber: 42},
		{raw: `-7`, serverValid: true, clientValid: true, clientNumber: -7},
		{raw: `1.5`, serverValid: true},
		{raw: `1e2`, serverValid: true},
		{raw: `null`},
		{raw: `true`},
		{raw: `{}`},
		{raw: `[]`},
	} {
		t.Run(test.raw, func(t *testing.T) {
			if got := validServerRequestID(json.RawMessage(test.raw)); got != test.serverValid {
				t.Errorf("validServerRequestID(%s) = %t, want %t", test.raw, got, test.serverValid)
			}
			number, ok := clientResponseID(json.RawMessage(test.raw))
			if ok != test.clientValid || ok && number != test.clientNumber {
				t.Errorf("clientResponseID(%s) = (%d, %t), want (%d, %t)", test.raw, number, ok, test.clientNumber, test.clientValid)
			}
		})
	}
}

func TestBlockedServerReplyDoesNotWedgeResponseDispatchAndTimesOut(t *testing.T) {
	transport := newBlockedServerReplyTransport(8)
	c := newClient(Spec{Name: "blocked-server-reply"}, transport, nil)
	c.answerTimeout = 100 * time.Millisecond
	t.Cleanup(func() { _ = c.Close() })

	response := make(chan rpcResponse, 1)
	c.mu.Lock()
	c.pending[77] = response
	c.mu.Unlock()
	transport.incoming <- []byte(`{"jsonrpc":"2.0","id":"server-request","method":"ping"}`)
	select {
	case <-transport.started:
	case <-time.After(time.Second):
		t.Fatal("server-request reply did not start")
	}

	transport.incoming <- []byte(`{"jsonrpc":"2.0","id":77,"result":{"ok":true}}`)
	select {
	case got := <-response:
		if string(got.Result) != `{"ok":true}` || got.fatal != nil {
			t.Fatalf("dispatched response = %+v", got)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("blocked server-request reply wedged the receive loop")
	}
	select {
	case err := <-transport.sendDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("server-request reply error = %v, want bounded deadline", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server-request reply did not honor its deadline")
	}
}

func TestServerReplyQueueOverflowFailsClosed(t *testing.T) {
	requests := serverReplyQueueSize + serverReplyWorkers + 8
	transport := newBlockedServerReplyTransport(requests)
	c := newClient(Spec{Name: "reply-overflow"}, transport, nil)
	c.answerTimeout = 10 * time.Second
	t.Cleanup(func() { _ = c.Close() })
	for i := range requests {
		transport.incoming <- []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":"request-%d","method":"ping"}`, i))
	}
	select {
	case <-transport.closed:
	case <-time.After(time.Second):
		t.Fatal("overflowing the server-response queue did not close the connection")
	}
	if err := c.Err(); err == nil || !strings.Contains(err.Error(), "queue exceeds") {
		t.Fatalf("client error = %v, want bounded queue failure", err)
	}
}

func TestDeadTransportFailsPendingAndFutureCalls(t *testing.T) {
	c, f := connectFake(t, echoTool, nil)

	done := make(chan error, 1)
	go func() {
		_, err := c.Call(context.Background(), "echo", json.RawMessage(`{"text":"hi"}`))
		done <- err
	}()
	// Give the call a moment to register as pending, then kill the wire.
	time.Sleep(50 * time.Millisecond)
	f.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a call pending on a dead transport must fail")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending call never failed after transport death")
	}

	if c.Err() == nil {
		t.Error("Err() must report the death")
	}
	if _, err := c.Call(context.Background(), "echo", nil); err == nil {
		t.Error("calls after death must fail immediately")
	}
}

func TestFatalReadLoopClosesTransportWithoutDeadlock(t *testing.T) {
	transport := &fatalReadTransport{fail: make(chan struct{}), closed: make(chan struct{})}
	c := newClient(Spec{Name: "fatal"}, transport, nil)
	close(transport.fail)
	select {
	case <-transport.closed:
	case <-time.After(time.Second):
		t.Fatal("fatal read loop did not close its transport")
	}
	if err := c.Err(); err == nil || !strings.Contains(err.Error(), "fatal read") {
		t.Fatalf("client error = %v, want sticky fatal read", err)
	}
}

func TestBridgedToolCarriesTheExternalEffect(t *testing.T) {
	c, _ := connectFake(t, echoTool, func(name string, args json.RawMessage) string {
		return `{"content":[{"type":"text","text":"echoed"}]}`
	})

	bridged := c.BridgedTools()
	if len(bridged) != 1 {
		t.Fatalf("bridged = %d tools, want 1", len(bridged))
	}
	tool := bridged[0]
	if tool.Name() != "mcp__fake__echo" {
		t.Errorf("Name() = %q, want the namespaced form", tool.Name())
	}
	if tool.ParallelSafe() {
		t.Error("an opaque external effect must not be parallel-safe")
	}
	if !strings.Contains(tool.Description(), "[fake MCP]") {
		t.Errorf("Description() = %q, want the provenance prefix", tool.Description())
	}

	plan, err := tool.Plan(json.RawMessage(`{"text":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Request.Effect != permission.EffectExternal {
		t.Errorf("Effect = %q, want external", plan.Request.Effect)
	}
	if plan.Request.Tool != "mcp__fake__echo" {
		t.Errorf("Request.Tool = %q", plan.Request.Tool)
	}
	if !strings.Contains(plan.Request.Detail, `"text":"hi"`) || !strings.Contains(plan.Request.Detail, "fake server") {
		t.Errorf("Detail = %q, want the arguments and the server", plan.Request.Detail)
	}

	res, err := plan.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "echoed" || res.IsError {
		t.Errorf("run = %+v", res)
	}
}

func TestNamespacedSanitizes(t *testing.T) {
	if got := Namespaced("my server", "read.file"); got != "mcp__my_server__read_file" {
		t.Errorf("Namespaced = %q", got)
	}
}

func TestAllowRulesNameTheNamespacedTool(t *testing.T) {
	c := &Client{spec: Spec{Name: "gh", Allow: []string{"search"}}}
	rules := c.AllowRules()
	if len(rules) != 1 {
		t.Fatalf("rules = %+v", rules)
	}
	r := rules[0]
	if r.Decision != permission.Allow || r.Tool != "mcp__gh__search" || r.Effect != permission.EffectExternal {
		t.Errorf("rule = %+v", r)
	}
}

func TestBridgeSchemaFallsBackToAnObject(t *testing.T) {
	c := &Client{spec: Spec{Name: "s"}, logf: func(string, string) {}, tools: []ToolInfo{{Name: "bare"}}}
	bridged := c.BridgedTools()
	if len(bridged) != 1 {
		t.Fatal("want the bare tool bridged")
	}
	var schema struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(bridged[0].Schema(), &schema); err != nil || schema.Type != "object" {
		t.Errorf("schema = %s, want an object schema", bridged[0].Schema())
	}
}

func TestBridgedToolRedactsCredentialMetadataBeforeProviderDefinitions(t *testing.T) {
	secret := "ghp_" + strings.Repeat("A", 36)
	schema := json.RawMessage(fmt.Sprintf(
		`{"type":"object","properties":{%q:{"type":"string","description":%q,"default":%q,"examples":[%q],"enum":[%q]}}}`,
		secret, "use "+secret, secret, secret, secret,
	))
	info := ToolInfo{
		Name:        "credential_test",
		Description: "server supplied " + secret,
		InputSchema: schema,
	}
	descriptionBefore := info.Description
	schemaBefore := string(info.InputSchema)
	c := &Client{
		spec:  Spec{Name: "metadata"},
		logf:  func(string, string) {},
		tools: []ToolInfo{info},
	}
	bridged := c.BridgedTools()
	if len(bridged) != 1 {
		t.Fatalf("bridged tools = %d, want 1", len(bridged))
	}

	registry, err := tools.NewRegistry(t.TempDir(), execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(registry.StopBackgroundCommands)
	if err := registry.AddExternal(bridged[0]); err != nil {
		t.Fatal(err)
	}
	var rendered string
	for _, definition := range registry.Definitions() {
		if definition.Name != bridged[0].Name() {
			continue
		}
		encoded, err := json.Marshal(definition)
		if err != nil {
			t.Fatal(err)
		}
		rendered = string(encoded)
		if !json.Valid(definition.Schema) {
			t.Fatalf("redacted schema is invalid JSON: %s", definition.Schema)
		}
	}
	if rendered == "" {
		t.Fatal("bridged tool is absent from provider definitions")
	}
	if strings.Contains(rendered, secret) || strings.Contains(rendered, "ghp_") {
		t.Fatalf("provider definition contains raw or partial credential: %s", rendered)
	}
	if !strings.Contains(rendered, "redacted") {
		t.Fatalf("provider definition does not retain a redaction marker: %s", rendered)
	}
	if info.Description != descriptionBefore || string(info.InputSchema) != schemaBefore {
		t.Fatal("rendering provider metadata mutated the discovered source values")
	}
	if c.tools[0].Description != descriptionBefore || string(c.tools[0].InputSchema) != schemaBefore {
		t.Fatal("rendering provider metadata mutated the client's discovered inventory")
	}
}

func TestBridgedToolRedactsCredentialAtMetadataBoundaryPositions(t *testing.T) {
	secret := "ghp_" + strings.Repeat("B", 36)
	for _, offset := range []int{0, 1, 63, 64, 119, 120, 121, 1023} {
		t.Run(fmt.Sprintf("offset_%d", offset), func(t *testing.T) {
			padding := strings.Repeat("x", offset) + "\n"
			originalSchema := json.RawMessage(fmt.Sprintf(
				`{"type":"object","properties":{"value":{"type":"string","description":%q}}}`,
				padding+secret+" end",
			))
			c := &Client{
				spec: Spec{Name: "boundary"},
				logf: func(string, string) {},
				tools: []ToolInfo{{
					Name:        "probe",
					Description: padding + secret + " end",
					InputSchema: originalSchema,
				}},
			}
			tool := c.BridgedTools()[0]
			got := tool.Description() + "\n" + string(tool.Schema())
			if strings.Contains(got, secret) || strings.Contains(got, "ghp_") {
				t.Fatalf("metadata at offset %d contains raw or partial credential", offset)
			}
			if strings.Count(got, "redacted") < 2 {
				t.Fatalf("metadata at offset %d lost its visible redaction markers: %q", offset, got)
			}
			if !json.Valid(tool.Schema()) {
				t.Fatalf("schema at offset %d is invalid after redaction: %s", offset, tool.Schema())
			}
			if string(c.tools[0].InputSchema) != string(originalSchema) {
				t.Fatalf("schema source at offset %d was mutated", offset)
			}
		})
	}
}

func TestBridgedToolRedactsEscapedCredentialInSchema(t *testing.T) {
	escapedSecret := `ghp_` + strings.Repeat(`\u0043`, 36)
	raw := json.RawMessage(`{"type":"object","properties":{"value":{"description":"` + escapedSecret + `"}}}`)
	c := &Client{
		spec:  Spec{Name: "escaped"},
		logf:  func(string, string) {},
		tools: []ToolInfo{{Name: "probe", InputSchema: raw}},
	}
	got := c.BridgedTools()[0].Schema()
	if !json.Valid(got) {
		t.Fatalf("schema is invalid after semantic redaction: %s", got)
	}
	if strings.Contains(string(got), "ghp_") || !strings.Contains(string(got), "redacted") {
		t.Fatalf("escaped credential was not redacted: %s", got)
	}
	if string(c.tools[0].InputSchema) != string(raw) {
		t.Fatal("escaped schema source was mutated")
	}
}

func TestBridgedToolLeavesOrdinarySchemaBytesUnchanged(t *testing.T) {
	raw := json.RawMessage("{\n  \"type\": \"object\", \"maximum\": 900719925474099312345,\n  \"properties\": {\"value\": {\"type\": \"string\"}}\n}")
	c := &Client{
		spec:  Spec{Name: "ordinary"},
		logf:  func(string, string) {},
		tools: []ToolInfo{{Name: "probe", InputSchema: raw}},
	}
	if got := c.BridgedTools()[0].Schema(); string(got) != string(raw) {
		t.Fatalf("ordinary schema changed:\n got %s\nwant %s", got, raw)
	}
}

func TestMCPNamesRedactCompleteComponentsBeforeNamespacing(t *testing.T) {
	secret := "ghp_" + strings.Repeat("D", 36)
	c := &Client{
		spec:  Spec{Name: secret, Allow: []string{secret}},
		logf:  func(string, string) {},
		tools: []ToolInfo{{Name: secret, Description: "ordinary"}},
	}
	bridged := c.BridgedTools()
	if len(bridged) != 1 {
		t.Fatalf("bridged tools = %d, want 1", len(bridged))
	}
	name := bridged[0].Name()
	if strings.Contains(name, secret) || strings.Contains(name, "ghp_") {
		t.Fatalf("provider tool name contains raw or partial credential: %q", name)
	}
	if description := bridged[0].Description(); strings.Contains(description, secret) || strings.Contains(description, "ghp_") {
		t.Fatalf("provider tool description contains raw or partial credential: %q", description)
	}
	plan, err := bridged[0].Plan(json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan.Request.Detail, secret) || strings.Contains(plan.Request.Detail, "ghp_") {
		t.Fatalf("permission detail contains raw or partial server credential: %q", plan.Request.Detail)
	}
	rules := c.AllowRules()
	if len(rules) != 1 || rules[0].Tool != name {
		t.Fatalf("redacted allow rule = %+v, want tool %q", rules, name)
	}
	if c.spec.Name != secret || c.tools[0].Name != secret {
		t.Fatal("namespacing mutated the server or tool source name")
	}
}

func TestBridgePermissionDetailRedactsServerMetadataBeforeTruncation(t *testing.T) {
	secret := "ghp_" + strings.Repeat("E", 36)
	input := json.RawMessage(fmt.Sprintf(`{"value":%q}`, strings.Repeat("x", 105)+" tail"))
	c := &Client{
		spec:  Spec{Name: secret},
		logf:  func(string, string) {},
		tools: []ToolInfo{{Name: "probe"}},
	}
	plan, err := c.BridgedTools()[0].Plan(input)
	if err != nil {
		t.Fatal(err)
	}
	detail := plan.Request.Detail
	if strings.Contains(detail, secret) || strings.Contains(detail, "ghp_") {
		t.Fatalf("permission detail retained a boundary-cut credential: %q", detail)
	}
	if !utf8.ValidString(detail) {
		t.Fatalf("permission detail is not valid UTF-8: %q", detail)
	}
	if len(compactJSON(input)) > 120 {
		t.Fatalf("compact argument detail is %d bytes, want at most 120", len(compactJSON(input)))
	}
}

func TestBridgeRefusesCredentialArgumentsBeforeExternalCall(t *testing.T) {
	secret := "ghp_" + strings.Repeat("G", 36)
	escaped := `\u0067hp_` + strings.Repeat(`\u0048`, 36)
	tests := []struct {
		name  string
		input json.RawMessage
	}{
		{name: "direct value", input: json.RawMessage(fmt.Sprintf(`{"value":%q}`, secret))},
		{name: "escaped key", input: json.RawMessage(`{"` + escaped + `":"ordinary"}`)},
		{name: "past permission detail boundary", input: json.RawMessage(fmt.Sprintf(
			`{"value":%q}`, strings.Repeat("x", 4096)+" "+secret+" tail"))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls int
			c, _ := connectFake(t, echoTool, func(string, json.RawMessage) string {
				calls++
				return `{"content":[{"type":"text","text":"unexpected"}]}`
			})
			plan, err := c.BridgedTools()[0].Plan(test.input)
			if err == nil {
				t.Fatalf("credential arguments produced a runnable plan: %+v", plan)
			}
			if plan.Run != nil || calls != 0 {
				t.Fatalf("credential arguments reached the external call path: run=%v calls=%d", plan.Run != nil, calls)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "ghp_") ||
				!strings.Contains(err.Error(), "credential-shaped data") {
				t.Fatalf("credential refusal was not generic and redacted: %q", err)
			}
		})
	}
}

func TestBridgeCredentialGateLeavesOrdinaryAndSplitValuesAlone(t *testing.T) {
	inputs := []json.RawMessage{
		json.RawMessage(`{"query":"ordinary work","count":3}`),
		// Scan semantic values independently; do not guess a token by joining
		// unrelated fields that the external tool receives separately.
		json.RawMessage(`{"prefix":"ghp_","suffix":"IIIIIIIIIIIIIIIIIIIIIIIIIIIIIIIIIIII"}`),
	}
	for _, input := range inputs {
		input := input
		t.Run(string(input), func(t *testing.T) {
			var called json.RawMessage
			c, _ := connectFake(t, echoTool, func(_ string, args json.RawMessage) string {
				called = append(json.RawMessage(nil), args...)
				return `{"content":[{"type":"text","text":"ok"}]}`
			})
			plan, err := c.BridgedTools()[0].Plan(input)
			if err != nil {
				t.Fatal(err)
			}
			result, err := plan.Run(context.Background())
			if err != nil || result.IsError || result.Content != "ok" {
				t.Fatalf("ordinary MCP call result=%+v err=%v", result, err)
			}
			if string(called) != string(input) {
				t.Fatalf("ordinary arguments changed: got %s want %s", called, input)
			}
		})
	}
}

func TestBridgeRedactsCredentialShapedTransportErrors(t *testing.T) {
	secret := "ghp_" + strings.Repeat("F", 36)
	c := &Client{
		spec:    Spec{Name: secret},
		logf:    func(string, string) {},
		pending: map[int64]chan rpcResponse{},
		dead:    fmt.Errorf("mcp server %s failed", secret),
		tools:   []ToolInfo{{Name: "probe"}},
	}
	plan, err := c.BridgedTools()[0].Plan(json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := plan.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatalf("transport failure result = %+v, want an error result", result)
	}
	if strings.Contains(result.Content, secret) || strings.Contains(result.Content, "ghp_") {
		t.Fatalf("transport failure rendered raw or partial credential: %q", result.Content)
	}
	if !strings.Contains(result.Content, "redacted") {
		t.Fatalf("transport failure lost its redaction marker: %q", result.Content)
	}
}
