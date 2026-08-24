package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// scriptedServer speaks the wire protocol from the test's goroutine, which
// pins the client's framing, id routing, and its null answer to server
// requests without any subprocess.
type scriptedServer struct {
	in  *io.PipeReader // what the client wrote
	out *io.PipeWriter // what the server says back
	r   *bufio.Reader
}

func newScriptedServer(t *testing.T) (*Client, *scriptedServer, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n\nvar Thing = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clientIn, serverOut := io.Pipe() // server -> client
	serverIn, clientOut := io.Pipe() // client -> server
	s := &scriptedServer{in: serverIn, out: serverOut, r: bufio.NewReader(serverIn)}
	c := newClient(clientOut, clientIn, root)
	t.Cleanup(func() { serverOut.Close() })
	return c, s, c.root
}

func (s *scriptedServer) recv(t *testing.T) map[string]any {
	t.Helper()
	msg, err := readMessage(s.r)
	if err != nil {
		t.Fatalf("reading client message: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(msg, &m); err != nil {
		t.Fatalf("client sent unparseable frame: %v", err)
	}
	return m
}

func (s *scriptedServer) send(t *testing.T, m map[string]any) {
	t.Helper()
	payload, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(s.out, "Content-Length: %d\r\n\r\n%s", len(payload), payload); err != nil {
		t.Fatal(err)
	}
}

func TestCallRoutesResponsesById(t *testing.T) {
	c, s, root := newScriptedServer(t)

	done := make(chan error, 1)
	var got []Location
	go func() {
		locs, err := c.locate(context.Background(), "textDocument/definition", filepath.Join(root, "a.go"), 3, 5, nil)
		got = locs
		done <- err
	}()

	open := s.recv(t)
	if open["method"] != "textDocument/didOpen" {
		t.Fatalf("first frame = %v, want didOpen before the query", open["method"])
	}
	req := s.recv(t)
	if req["method"] != "textDocument/definition" {
		t.Fatalf("second frame = %v", req["method"])
	}
	pos := req["params"].(map[string]any)["position"].(map[string]any)
	if pos["line"].(float64) != 2 {
		t.Errorf("wire line = %v, want the 1-based input made 0-based", pos["line"])
	}

	// Interleave a server-initiated request; the client must answer it with
	// one default value per requested configuration item, rather than leave
	// the server hanging or confuse it with the pending call.
	s.send(t, map[string]any{"jsonrpc": "2.0", "id": 999, "method": "workspace/configuration",
		"params": map[string]any{"items": []map[string]any{{"section": "one"}, {"section": "two"}}}})
	reply := s.recv(t)
	values, ok := reply["result"].([]any)
	if reply["id"].(float64) != 999 || !ok || len(values) != 2 || values[0] != nil || values[1] != nil {
		t.Fatalf("server request got reply %v, want two default values for id 999", reply)
	}

	s.send(t, map[string]any{"jsonrpc": "2.0", "id": req["id"],
		"result": []map[string]any{{
			"uri": "file:///ws/b.go",
			"range": map[string]any{
				"start": map[string]any{"line": 9},
				"end":   map[string]any{"line": 9, "character": 1},
			},
		}}})

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call never completed")
	}
	if len(got) != 1 || got[0].Path != filepath.FromSlash("/ws/b.go") || got[0].Line != 10 {
		t.Fatalf("locations = %+v, want b.go:10 back in 1-based terms", got)
	}
}

func TestNullResultMeansNothingFound(t *testing.T) {
	c, s, root := newScriptedServer(t)

	done := make(chan struct {
		locs []Location
		err  error
	}, 1)
	go func() {
		locs, err := c.locate(context.Background(), "textDocument/definition", filepath.Join(root, "a.go"), 1, 0, nil)
		done <- struct {
			locs []Location
			err  error
		}{locs, err}
	}()

	s.recv(t) // didOpen
	req := s.recv(t)
	s.send(t, map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": nil})

	res := <-done
	if res.err != nil || res.locs != nil {
		t.Fatalf("null result = (%v, %v), want an empty answer with no error", res.locs, res.err)
	}
}

func TestServerDeathFailsPendingCalls(t *testing.T) {
	c, s, root := newScriptedServer(t)

	done := make(chan error, 1)
	go func() {
		_, err := c.locate(context.Background(), "textDocument/definition", filepath.Join(root, "a.go"), 1, 0, nil)
		done <- err
	}()
	s.recv(t) // didOpen
	s.recv(t) // the query
	s.out.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a dead server stream must fail the pending call, not hang it")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the pending call hung on a dead server")
	}
}
