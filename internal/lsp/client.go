// Package lsp speaks the Language Server Protocol to one server over stdio.
// Every position query is sent against the exact saved bytes used to locate
// the symbol, and every feature is checked against initialize capabilities.
package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/safeexec"
	workspacefs "github.com/switchboard-code/switchboard/internal/workspace"
)

const maxLSPMessageBytes = 16 << 20

// Feature is one server capability Switchboard can request.
type Feature string

const (
	FeatureDefinition       Feature = "definition"
	FeatureReferences       Feature = "references"
	FeatureDocumentSymbols  Feature = "document symbols"
	FeatureWorkspaceSymbols Feature = "workspace symbols"
	FeatureHover            Feature = "hover"
)

// UnsupportedCapabilityError reports an initialize mismatch without silently
// substituting a less precise tool.
type UnsupportedCapabilityError struct {
	Feature Feature
	Server  string
}

func (e *UnsupportedCapabilityError) Error() string {
	server := e.Server
	if server == "" {
		server = "the language server"
	}
	return fmt.Sprintf("%s does not advertise %s support", server, e.Feature)
}

// Client is one running server. Methods are safe to call concurrently;
// responses route by request id, while synchronization is serialized so a
// didChange and its dependent request retain wire order.
type Client struct {
	writeMu sync.Mutex
	in      io.WriteCloser

	mu           sync.Mutex
	pending      map[int64]chan *response
	nextID       int64
	closing      bool
	closed       bool
	capabilities Capabilities

	documentsMu sync.Mutex
	documents   map[string]*documentState

	closeOnce sync.Once
	problems  *ProblemStore
	cmd       *exec.Cmd
	root      string
	// documentRoot is the exact physical workspace identity selected before
	// the server starts. Every initial and retained-document read goes through
	// it; root is only the LSP wire/cwd spelling and is never file authority.
	documentRoot    *workspacefs.Root
	documentRootErr error
	// closeGrace is a test seam; zero uses closeTimeout.
	closeGrace time.Duration
}

type documentState struct {
	version      int
	savedVersion int
	text         []byte
	open         bool
}

type response struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("server error %d: %s", e.Code, e.Message) }

type pendingCall struct {
	id int64
	ch chan *response
}

// Start spawns the server and completes initialize. ctx bounds the handshake
// only; the process then lives until Close.
func Start(ctx context.Context, argv []string, root string) (*Client, error) {
	return startWithProblems(ctx, argv, root, NewProblemStore(root))
}

func startWithProblems(ctx context.Context, argv []string, root string, problems *ProblemStore) (*Client, error) {
	return startWithCommand(ctx, argv, root, problems, func(argv []string, root string) (*exec.Cmd, error) {
		return languageServerCommand(argv, root), nil
	})
}

func startBoundWithProblems(
	ctx context.Context,
	executable safeexec.Executable,
	argv []string,
	root string,
	environment []string,
	problems *ProblemStore,
) (*Client, error) {
	return startWithCommand(ctx, argv, root, problems, func(argv []string, root string) (*exec.Cmd, error) {
		if len(argv) == 0 || argv[0] != executable.Path() {
			return nil, errors.New("language server executable binding does not match argv")
		}
		if len(environment) == 0 {
			return nil, errors.New("language server has no trusted child environment")
		}
		cmd, err := executable.Command(argv[1:]...)
		if err != nil {
			return nil, fmt.Errorf("binding language server executable: %w", err)
		}
		cmd.Dir = root
		cmd.Env = append([]string(nil), environment...)
		return cmd, nil
	})
}

func startWithCommand(
	ctx context.Context,
	argv []string,
	root string,
	problems *ProblemStore,
	command func([]string, string) (*exec.Cmd, error),
) (*Client, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return nil, fmt.Errorf("language server command is empty")
	}
	documentRoot, err := workspacefs.Open(root)
	if err != nil {
		return nil, fmt.Errorf("binding language-server workspace: %w", err)
	}
	root = documentRoot.Path()
	cmd, err := command(argv, root)
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// A full stderr pipe would block the server in the middle of an answer.
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", argv[0], err)
	}

	c := newClientWithDocumentRoot(stdin, stdout, root, problems, documentRoot, nil)
	c.cmd = cmd
	if err := c.initialize(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func languageServerCommand(argv []string, root string) *exec.Cmd {
	cmd := exec.CommandContext(context.Background(), argv[0], argv[1:]...)
	cmd.Dir = root
	cmd.Env = execution.ScrubbedChildEnv()
	return cmd
}

// newClient wires arbitrary pipes. Tests use it without initialize, so it
// starts with the legacy definition/reference profile; Start replaces that
// profile with the server's decoded result.
func newClient(in io.WriteCloser, out io.Reader, root string) *Client {
	return newClientWithProblems(in, out, root, NewProblemStore(root))
}

func newClientWithProblems(in io.WriteCloser, out io.Reader, root string, problems *ProblemStore) *Client {
	documentRoot, rootErr := workspacefs.Open(root)
	if rootErr == nil {
		root = documentRoot.Path()
	}
	return newClientWithDocumentRoot(in, out, root, problems, documentRoot, rootErr)
}

func newClientWithDocumentRoot(
	in io.WriteCloser,
	out io.Reader,
	root string,
	problems *ProblemStore,
	documentRoot *workspacefs.Root,
	documentRootErr error,
) *Client {
	if problems == nil {
		problems = NewProblemStore(root)
	}
	c := &Client{
		in:              in,
		pending:         map[int64]chan *response{},
		documents:       map[string]*documentState{},
		root:            filepath.Clean(root),
		problems:        problems,
		documentRoot:    documentRoot,
		documentRootErr: documentRootErr,
		capabilities: Capabilities{
			PositionEncoding: PositionEncodingUTF16,
			Sync:             SyncOptions{OpenClose: true, Change: SyncFull},
			Definition:       true,
			References:       true,
		},
	}
	go c.read(out)
	return c
}

func (c *Client) initialize(ctx context.Context) error {
	uri := pathToURI(c.root)
	var symbolKinds []int
	for kind := 1; kind <= 26; kind++ {
		symbolKinds = append(symbolKinds, kind)
	}
	var result json.RawMessage
	err := c.call(ctx, "initialize", map[string]any{
		"processId": os.Getpid(),
		"rootUri":   uri,
		"workspaceFolders": []map[string]any{
			{"uri": uri, "name": filepath.Base(c.root)},
		},
		"capabilities": map[string]any{
			"general": map[string]any{
				"positionEncodings": []string{string(PositionEncodingUTF16)},
			},
			"textDocument": map[string]any{
				"synchronization": map[string]any{
					"dynamicRegistration": false,
					"didSave":             true,
				},
				"definition": map[string]any{
					"dynamicRegistration": false,
					"linkSupport":         true,
				},
				"references": map[string]any{"dynamicRegistration": false},
				"documentSymbol": map[string]any{
					"dynamicRegistration":               false,
					"hierarchicalDocumentSymbolSupport": true,
					"symbolKind":                        map[string]any{"valueSet": symbolKinds},
				},
				"publishDiagnostics": map[string]any{
					"relatedInformation": true,
					"versionSupport":     true,
				},
			},
			"workspace": map[string]any{
				"symbol": map[string]any{
					"dynamicRegistration": false,
					"symbolKind":          map[string]any{"valueSet": symbolKinds},
				},
			},
		},
	}, &result)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	capabilities, err := decodeInitializeResult(result)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	c.setCapabilities(capabilities)
	return c.notify("initialized", map[string]any{})
}

// Capabilities returns the normalized initialize result.
func (c *Client) Capabilities() Capabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capabilities
}

func (c *Client) setCapabilities(capabilities Capabilities) {
	c.mu.Lock()
	c.capabilities = capabilities
	c.mu.Unlock()
}

func (c *Client) require(feature Feature) error {
	capabilities := c.Capabilities()
	var supported bool
	switch feature {
	case FeatureDefinition:
		supported = capabilities.Definition
	case FeatureReferences:
		supported = capabilities.References
	case FeatureDocumentSymbols:
		supported = capabilities.DocumentSymbols
	case FeatureWorkspaceSymbols:
		supported = capabilities.WorkspaceSymbols
	case FeatureHover:
		supported = capabilities.Hover
	}
	if supported {
		return nil
	}
	return &UnsupportedCapabilityError{Feature: feature, Server: capabilities.ServerName}
}

// Problems returns the diagnostics store owned by this client.
func (c *Client) Problems() *ProblemStore { return c.problems }

// read is the one stdout consumer. Responses route to callers, supported
// server requests receive their protocol-shaped answer, and diagnostics
// update state.
func (c *Client) read(out io.Reader) {
	r := bufio.NewReader(out)
	for {
		msg, err := readMessage(r)
		if err != nil {
			c.failAll(err)
			return
		}
		var frame struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if err := json.Unmarshal(msg, &frame); err != nil {
			continue
		}
		switch {
		case frame.Method != "" && frame.ID != nil:
			c.replyServerRequest(frame.ID, frame.Method, frame.Params)
		case frame.Method == "textDocument/publishDiagnostics":
			c.publishDiagnostics(frame.Params)
		case frame.Method != "":
			// Notifications without client-owned state are deliberately ignored.
		case frame.ID != nil:
			var id int64
			if err := json.Unmarshal(frame.ID, &id); err != nil {
				continue
			}
			c.mu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ch != nil {
				ch <- &response{Result: frame.Result, Error: frame.Error}
			}
		}
	}
}

func (c *Client) failAll(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	for id, ch := range c.pending {
		delete(c.pending, id)
		ch <- &response{Error: &rpcError{Message: fmt.Sprintf("server stream ended: %v", err)}}
	}
	c.mu.Unlock()
	c.problems.unavailable()
}

func (c *Client) replyServerRequest(id json.RawMessage, method string, params json.RawMessage) {
	var result any
	switch method {
	case "workspace/configuration":
		var request struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(params, &request); err != nil {
			c.replyError(id, -32602, "invalid workspace/configuration parameters")
			return
		}
		// No Switchboard-owned configuration namespace exists yet. One null
		// per requested item is the required result shape and lets the server
		// apply its own default for each section.
		result = make([]any, len(request.Items))
	case "workspace/workspaceFolders":
		result = []map[string]any{{"uri": pathToURI(c.root), "name": filepath.Base(c.root)}}
	case "window/workDoneProgress/create":
		result = nil
	default:
		c.replyError(id, -32601, fmt.Sprintf("unsupported server request %s", method))
		return
	}
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	_ = c.write(payload)
}

func (c *Client) replyError(id json.RawMessage, code int, message string) {
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message},
	})
	_ = c.write(payload)
}

func (c *Client) beginCall(method string, params any) (pendingCall, error) {
	c.mu.Lock()
	if c.closed || (c.closing && method != "shutdown") {
		c.mu.Unlock()
		return pendingCall{}, fmt.Errorf("the language server is shutting down")
	}
	c.nextID++
	handle := pendingCall{id: c.nextID, ch: make(chan *response, 1)}
	c.pending[handle.id] = handle.ch
	c.mu.Unlock()

	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": handle.id, "method": method, "params": params,
	})
	if err == nil {
		err = c.write(payload)
	}
	if err != nil {
		c.mu.Lock()
		if c.pending[handle.id] == handle.ch {
			delete(c.pending, handle.id)
		}
		c.mu.Unlock()
		return pendingCall{}, err
	}
	return handle, nil
}

func (c *Client) ensureRunning() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.closing {
		return fmt.Errorf("the language server is shutting down")
	}
	return nil
}

func (c *Client) awaitCall(ctx context.Context, handle pendingCall, result any) error {
	select {
	case resp := <-handle.ch:
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && len(resp.Result) != 0 && !isNull(resp.Result) {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		_, pending := c.pending[handle.id]
		if pending {
			delete(c.pending, handle.id)
		}
		c.mu.Unlock()
		if pending {
			// Cancellation must return even if a misbehaving server has stopped
			// reading stdin. Closing the client eventually releases this writer.
			go func() { _ = c.notify("$/cancelRequest", map[string]any{"id": handle.id}) }()
		}
		return ctx.Err()
	}
}

func (c *Client) call(ctx context.Context, method string, params any, result any) error {
	handle, err := c.beginCall(method, params)
	if err != nil {
		return err
	}
	return c.awaitCall(ctx, handle, result)
}

func (c *Client) notify(method string, params any) error {
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return err
	}
	return c.write(payload)
}

func (c *Client) write(payload []byte) error {
	if len(payload) > maxLSPMessageBytes {
		return fmt.Errorf("language-server request is %d bytes; limit is %d", len(payload), maxLSPMessageBytes)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := fmt.Fprintf(c.in, "Content-Length: %d\r\n\r\n", len(payload)); err != nil {
		return err
	}
	_, err := c.in.Write(payload)
	return err
}

func readMessage(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if value, ok := strings.CutPrefix(line, "Content-Length: "); ok {
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, fmt.Errorf("bad Content-Length %q", value)
			}
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("message without Content-Length")
	}
	if length > maxLSPMessageBytes {
		return nil, fmt.Errorf("language-server message is %d bytes; limit is %d", length, maxLSPMessageBytes)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (c *Client) syncDocumentLocked(path string, data []byte, capabilities Capabilities) error {
	path = filepath.Clean(path)
	uri := pathToURI(path)
	state := c.documents[path]
	if !capabilities.Sync.OpenClose && capabilities.Sync.Change == SyncNone && !capabilities.Sync.Save {
		return nil
	}
	if state == nil {
		if !capabilities.Sync.OpenClose {
			// A server may advertise change synchronization without asking for
			// didOpen/didClose. A full-content initial change works for either
			// full or incremental synchronization and supplies the exact query
			// snapshot without inventing open/close support the server never
			// claimed.
			if capabilities.Sync.Change != SyncNone {
				if err := c.notify("textDocument/didChange", map[string]any{
					"textDocument":   map[string]any{"uri": uri, "version": 1},
					"contentChanges": []map[string]any{{"text": string(data)}},
				}); err != nil {
					return err
				}
			}
			c.documents[path] = &documentState{
				version: 1, savedVersion: 1, text: append([]byte(nil), data...),
			}
			c.problems.invalidate(uri, 1)
			return nil
		}
		if err := c.notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri": uri, "languageId": languageOf(path), "version": 1, "text": string(data),
			},
		}); err != nil {
			return err
		}
		c.documents[path] = &documentState{
			version: 1, savedVersion: 1, text: append([]byte(nil), data...), open: true,
		}
		c.problems.reopen(uri, 1)
		return nil
	}
	if bytes.Equal(state.text, data) {
		return c.retrySaveLocked(path, data, state, capabilities)
	}
	if capabilities.Sync.Change == SyncNone {
		return fmt.Errorf("%s changed after its synchronization baseline, but the server advertises no textDocument/didChange support", path)
	}

	nextVersion := state.version + 1
	change := map[string]any{"text": string(data)}
	if capabilities.Sync.Change == SyncIncremental {
		change["range"] = map[string]any{"start": Position{}, "end": documentEnd(state.text)}
	}
	if err := c.notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": nextVersion},
		"contentChanges": []map[string]any{change},
	}); err != nil {
		return err
	}
	state.version = nextVersion
	state.text = append(state.text[:0], data...)
	c.problems.invalidate(uri, nextVersion)
	return c.retrySaveLocked(path, data, state, capabilities)
}

func (c *Client) retrySaveLocked(path string, data []byte, state *documentState, capabilities Capabilities) error {
	if !capabilities.Sync.Save || state.savedVersion >= state.version {
		return nil
	}
	params := map[string]any{"textDocument": map[string]any{"uri": pathToURI(path)}}
	if capabilities.Sync.SaveIncludeText {
		params["text"] = string(data)
	}
	if err := c.notify("textDocument/didSave", params); err != nil {
		return err
	}
	state.savedVersion = state.version
	return nil
}

func (c *Client) closeDocumentLocked(path string, capabilities Capabilities) error {
	path = filepath.Clean(path)
	state := c.documents[path]
	if state == nil {
		return nil
	}
	if state.open && capabilities.Sync.OpenClose {
		if err := c.notify("textDocument/didClose", map[string]any{
			"textDocument": map[string]any{"uri": pathToURI(path)},
		}); err != nil {
			return err
		}
	}
	delete(c.documents, path)
	return nil
}

func (c *Client) beginDocumentCall(ctx context.Context, feature Feature, method, path string, params func([]byte) (map[string]any, error)) (pendingCall, error) {
	if err := ctx.Err(); err != nil {
		return pendingCall{}, err
	}
	if err := c.require(feature); err != nil {
		return pendingCall{}, err
	}
	capabilities := c.Capabilities()
	c.documentsMu.Lock()
	defer c.documentsMu.Unlock()
	if err := c.ensureRunning(); err != nil {
		return pendingCall{}, err
	}

	// Position resolution and synchronization consume the same single read.
	data, err := c.readDocumentSnapshot(ctx, filepath.Clean(path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			closeErr := c.closeDocumentLocked(path, capabilities)
			return pendingCall{}, errors.Join(err, closeErr)
		}
		if documentAuthorityChanged(err) {
			closeErr := c.closeDocumentLocked(path, capabilities)
			return pendingCall{}, errors.Join(err, closeErr)
		}
		return pendingCall{}, err
	}
	request, err := params(data)
	if err != nil {
		return pendingCall{}, err
	}
	if err := c.syncDocumentLocked(path, data, capabilities); err != nil {
		return pendingCall{}, err
	}
	return c.beginCall(method, request)
}

func (c *Client) documentCall(ctx context.Context, feature Feature, method, path string, params func([]byte) (map[string]any, error), result any) error {
	handle, err := c.beginDocumentCall(ctx, feature, method, path, params)
	if err != nil {
		return err
	}
	return c.awaitCall(ctx, handle, result)
}

// Location is one answer. Lines and characters are 1-based; Character remains
// a UTF-16 code-unit column and must be converted by an editor navigation
// boundary that indexes text in runes or bytes. Path and Line remain
// source-compatible with the original API.
type Location struct {
	Path      string
	Line      int
	Character int
}

type wireRange struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

type wireLocation struct {
	URI   string    `json:"uri"`
	Range wireRange `json:"range"`
}

// Definition retains the existing position API. Character is a zero-based
// UTF-16 offset. Model tools use DefinitionAtSymbol for exact snapshot lookup.
func (c *Client) Definition(ctx context.Context, path string, line, character int) ([]Location, error) {
	return c.locate(ctx, "textDocument/definition", path, line, character, nil)
}

func (c *Client) DefinitionAtSymbol(ctx context.Context, path string, line int, symbol string) ([]Location, error) {
	return c.locateSymbol(ctx, FeatureDefinition, "textDocument/definition", path, line, symbol, nil)
}

// References includes the declaration, matching the existing tool contract.
func (c *Client) References(ctx context.Context, path string, line, character int) ([]Location, error) {
	return c.locate(ctx, "textDocument/references", path, line, character,
		map[string]any{"includeDeclaration": true})
}

func (c *Client) ReferencesAtSymbol(ctx context.Context, path string, line int, symbol string) ([]Location, error) {
	return c.locateSymbol(ctx, FeatureReferences, "textDocument/references", path, line, symbol,
		map[string]any{"includeDeclaration": true})
}

// locate is retained for package tests and existing callers.
func (c *Client) locate(ctx context.Context, method, path string, line, character int, extra map[string]any) ([]Location, error) {
	feature := FeatureDefinition
	if method == "textDocument/references" {
		feature = FeatureReferences
	}
	if line < 1 || character < 0 {
		return nil, fmt.Errorf("line must be 1-based and character must be non-negative")
	}
	var raw json.RawMessage
	err := c.documentCall(ctx, feature, method, path, func([]byte) (map[string]any, error) {
		return positionParams(path, Position{Line: line - 1, Character: character}, extra), nil
	}, &raw)
	if err != nil {
		return nil, err
	}
	return decodeLocations(method, raw)
}

func (c *Client) locateSymbol(ctx context.Context, feature Feature, method, path string, line int, symbol string, extra map[string]any) ([]Location, error) {
	var raw json.RawMessage
	err := c.documentCall(ctx, feature, method, path, func(data []byte) (map[string]any, error) {
		position, err := symbolPosition(data, line, symbol, 1)
		if err != nil {
			return nil, err
		}
		return positionParams(path, position, extra), nil
	}, &raw)
	if err != nil {
		return nil, err
	}
	return decodeLocations(method, raw)
}

func positionParams(path string, position Position, extra map[string]any) map[string]any {
	params := map[string]any{
		"textDocument": map[string]any{"uri": pathToURI(path)},
		"position":     position,
	}
	if extra != nil {
		params["context"] = extra
	}
	return params
}

func decodeLocations(method string, raw json.RawMessage) ([]Location, error) {
	if len(raw) == 0 || isNull(raw) {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		items = []json.RawMessage{raw}
	}
	wire := make([]wireLocation, 0, len(items))
	for index, item := range items {
		var location struct {
			URI                  string     `json:"uri"`
			Range                *wireRange `json:"range"`
			TargetURI            string     `json:"targetUri"`
			TargetRange          *wireRange `json:"targetRange"`
			TargetSelectionRange *wireRange `json:"targetSelectionRange"`
		}
		if err := json.Unmarshal(item, &location); err != nil {
			return nil, fmt.Errorf("unreadable %s answer: %s", method, raw)
		}
		switch {
		case location.URI != "":
			if location.Range == nil {
				return nil, fmt.Errorf("unreadable %s answer: location %d omitted range", method, index)
			}
			if err := validateRange(Range(*location.Range)); err != nil {
				return nil, fmt.Errorf("unreadable %s answer: location %d range: %w", method, index, err)
			}
			wire = append(wire, wireLocation{URI: location.URI, Range: *location.Range})
		case location.TargetURI != "":
			if location.TargetRange == nil || location.TargetSelectionRange == nil {
				return nil, fmt.Errorf("unreadable %s answer: location link %d omitted targetRange or targetSelectionRange", method, index)
			}
			if err := validateRange(Range(*location.TargetRange)); err != nil {
				return nil, fmt.Errorf("unreadable %s answer: location link %d targetRange: %w", method, index, err)
			}
			if err := validateRange(Range(*location.TargetSelectionRange)); err != nil {
				return nil, fmt.Errorf("unreadable %s answer: location link %d targetSelectionRange: %w", method, index, err)
			}
			if !rangeContains(Range(*location.TargetRange), Range(*location.TargetSelectionRange)) {
				return nil, fmt.Errorf("unreadable %s answer: location link %d selection is outside targetRange", method, index)
			}
			wire = append(wire, wireLocation{URI: location.TargetURI, Range: *location.TargetSelectionRange})
		default:
			return nil, fmt.Errorf("unreadable %s answer: location has no URI", method)
		}
	}

	locations := make([]Location, 0, min(len(wire), 5_000))
	seen := make(map[string]bool)
	for _, item := range wire {
		path, err := filePath(item.URI)
		if err != nil {
			return nil, fmt.Errorf("unreadable %s URI %q: %w", method, item.URI, err)
		}
		location := Location{Path: path, Line: item.Range.Start.Line + 1, Character: item.Range.Start.Character + 1}
		key := fmt.Sprintf("%s\x00%d\x00%d", location.Path, location.Line, location.Character)
		if seen[key] {
			continue
		}
		seen[key] = true
		locations = append(locations, location)
		if len(locations) == 5_000 {
			break
		}
	}
	sort.Slice(locations, func(i, j int) bool {
		if locations[i].Path != locations[j].Path {
			return locations[i].Path < locations[j].Path
		}
		if locations[i].Line != locations[j].Line {
			return locations[i].Line < locations[j].Line
		}
		return locations[i].Character < locations[j].Character
	})
	return locations, nil
}

type publishDiagnosticsParams struct {
	URI         string           `json:"uri"`
	Version     *int             `json:"version"`
	Diagnostics []wireDiagnostic `json:"diagnostics"`
}

type wireDiagnostic struct {
	Range    *wireRange      `json:"range"`
	Severity Severity        `json:"severity"`
	Code     json.RawMessage `json:"code"`
	Source   string          `json:"source"`
	Message  string          `json:"message"`
	Related  []struct {
		Location struct {
			URI   string     `json:"uri"`
			Range *wireRange `json:"range"`
		} `json:"location"`
		Message string `json:"message"`
	} `json:"relatedInformation"`
}

func (c *Client) publishDiagnostics(raw json.RawMessage) {
	var params publishDiagnosticsParams
	if err := json.Unmarshal(raw, &params); err != nil {
		_ = c.problems.publish(problemPublish{})
		return
	}
	current, known := c.currentDocumentVersion(params.URI)
	problems := make([]Problem, 0, len(params.Diagnostics))
	for index, diagnostic := range params.Diagnostics {
		if diagnostic.Range == nil {
			_ = c.problems.protocolIssue(fmt.Sprintf("publishDiagnostics item %d omitted range", index))
			return
		}
		if err := validateRange(Range(*diagnostic.Range)); err != nil {
			_ = c.problems.protocolIssue(fmt.Sprintf("publishDiagnostics item %d range: %v", index, err))
			return
		}
		problem := Problem{
			Severity: diagnostic.Severity, Code: diagnosticCode(diagnostic.Code),
			Source: diagnostic.Source, Message: diagnostic.Message,
			Line: diagnostic.Range.Start.Line + 1, Column: diagnostic.Range.Start.Character + 1,
			EndLine: diagnostic.Range.End.Line + 1, EndColumn: diagnostic.Range.End.Character + 1,
		}
		for relatedIndex, related := range diagnostic.Related {
			if related.Location.URI == "" || related.Location.Range == nil {
				_ = c.problems.protocolIssue(fmt.Sprintf(
					"publishDiagnostics item %d relatedInformation %d omitted URI or range", index, relatedIndex))
				return
			}
			if err := validateRange(Range(*related.Location.Range)); err != nil {
				_ = c.problems.protocolIssue(fmt.Sprintf(
					"publishDiagnostics item %d relatedInformation %d range: %v", index, relatedIndex, err))
				return
			}
			problem.Related = append(problem.Related, RelatedProblem{
				URI: related.Location.URI, Message: related.Message,
				Line: related.Location.Range.Start.Line + 1, Column: related.Location.Range.Start.Character + 1,
				EndLine: related.Location.Range.End.Line + 1, EndColumn: related.Location.Range.End.Character + 1,
			})
		}
		problems = append(problems, problem)
	}
	_ = c.problems.publish(problemPublish{
		URI: params.URI, Version: params.Version, Problems: problems,
		CurrentVersion: current, CurrentVersionKnown: known,
	})
}

func diagnosticCode(raw json.RawMessage) string {
	if len(raw) == 0 || isNull(raw) {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return number.String()
	}
	return ""
}

func (c *Client) currentDocumentVersion(uri string) (int, bool) {
	path, err := filePath(uri)
	if err != nil {
		return 0, false
	}
	c.documentsMu.Lock()
	defer c.documentsMu.Unlock()
	state := c.documents[filepath.Clean(path)]
	if state == nil || !state.open {
		return 0, false
	}
	return state.version, true
}

// Close prevents new work, cancels pending calls, sends didClose for every
// owned document, shuts down, and reaps the process. It is safe to call
// repeatedly and cannot wait forever for a child that ignores exit.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		grace := c.closeGrace
		if grace <= 0 {
			grace = closeTimeout
		}
		c.mu.Lock()
		c.closing = true
		pendingIDs := make([]int64, 0, len(c.pending))
		for id, ch := range c.pending {
			delete(c.pending, id)
			pendingIDs = append(pendingIDs, id)
			ch <- &response{Error: &rpcError{Code: -32800, Message: "language server is shutting down"}}
		}
		c.mu.Unlock()
		sort.Slice(pendingIDs, func(i, j int) bool { return pendingIDs[i] < pendingIDs[j] })

		// Any individual write can be stuck behind a child that stopped reading
		// stdin. Run the courteous protocol sequence concurrently so the outer
		// deadline can close the pipe and kill the process to release it.
		gracefulDone := make(chan struct{})
		go func() {
			defer close(gracefulDone)
			for _, id := range pendingIDs {
				_ = c.notify("$/cancelRequest", map[string]any{"id": id})
			}

			capabilities := c.Capabilities()
			c.documentsMu.Lock()
			paths := make([]string, 0, len(c.documents))
			for path := range c.documents {
				paths = append(paths, path)
			}
			sort.Strings(paths)
			for _, path := range paths {
				_ = c.closeDocumentLocked(path, capabilities)
			}
			c.documentsMu.Unlock()

			ctx, cancel := context.WithTimeout(context.Background(), grace)
			_ = c.call(ctx, "shutdown", nil, nil)
			cancel()
			_ = c.notify("exit", nil)
		}()

		timedOut := false
		timer := time.NewTimer(grace)
		select {
		case <-gracefulDone:
			timer.Stop()
		case <-timer.C:
			timedOut = true
		}
		_ = c.in.Close()
		if c.cmd != nil {
			if timedOut && c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
			reapProcess(c.cmd, grace)
		}
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		if timedOut {
			select {
			case <-gracefulDone:
			case <-time.After(grace):
			}
		}
		c.problems.unavailable()
	})
}

func reapProcess(cmd *exec.Cmd, grace time.Duration) {
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
	}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	select {
	case <-done:
	case <-time.After(grace):
	}
}

func pathToURI(path string) string { return fileURI(path) }

func uriToPath(uri string) string {
	path, err := filePath(uri)
	if err != nil {
		return uri
	}
	return path
}

func languageOf(path string) string {
	switch filepath.Ext(path) {
	case ".go":
		return "go"
	case ".ts":
		return "typescript"
	case ".tsx":
		return "typescriptreact"
	case ".js":
		return "javascript"
	case ".jsx":
		return "javascriptreact"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".c", ".h":
		return "c"
	case ".cc", ".cpp", ".cxx", ".hh", ".hpp", ".hxx":
		return "cpp"
	default:
		return strings.TrimPrefix(filepath.Ext(path), ".")
	}
}
