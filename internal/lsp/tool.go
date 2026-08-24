package lsp

// Language-server tools over one lazy shared runtime. Definition and
// references take {path, line, symbol} and find the column from the exact
// document snapshot they synchronize. Outline and symbols expose bounded,
// deterministic structural context without making callers understand LSP.
//
// The server starts on the first call, not at assembly: a session that
// never asks pays nothing. Tool presence is still decided at assembly,
// which is what the frozen zone requires; a server that then fails to
// start is a tool error the model reads, not a broken session.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/safeexec"
	"github.com/switchboard-code/switchboard/internal/tools"
)

const (
	startTimeout = 30 * time.Second
	queryTimeout = 20 * time.Second
	closeTimeout = 3 * time.Second
)

// Server lazily starts one language server and shares it across the tools
// and every registry they are added to.
type Server struct {
	Argv []string
	Root string
	// Executable and Environment bind a fixed machine language server and its
	// interpreter search path outside both the target workspace and the process
	// launch checkout. A zero Executable retains the legacy direct-start API for
	// explicit library callers and tests; Switchboard assembly always sets it.
	Executable  safeexec.Executable
	Environment []string
	// OpenCloseSync is a verified server-profile correction for a runtime
	// whose legacy numeric textDocumentSync advertisement omits the separate
	// openClose behavior it actually requires. Generic numeric decoding stays
	// change-only; only a live-tested candidate should set this.
	OpenCloseSync bool

	mu           sync.Mutex
	starting     *serverStart
	client       *Client
	closed       bool
	lastError    string
	problemsOnce sync.Once
	problems     *ProblemStore
	// startClient is a test seam; nil uses startWithProblems.
	startClient func(context.Context, []string, string, *ProblemStore) (*Client, error)
}

type serverStart struct {
	done   chan struct{}
	cancel context.CancelFunc
	client *Client
	err    error
}

func (s *Server) get(ctx context.Context) (*Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("language server is closed")
	}
	if s.client != nil {
		client := s.client
		s.mu.Unlock()
		return client, nil
	}
	if attempt := s.starting; attempt != nil {
		s.mu.Unlock()
		select {
		case <-attempt.done:
			return attempt.client, attempt.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	startCtx, cancel := context.WithTimeout(ctx, startTimeout)
	attempt := &serverStart{done: make(chan struct{}), cancel: cancel}
	s.starting = attempt
	starter := s.startClient
	argv := append([]string(nil), s.Argv...)
	root := s.Root
	executable := s.Executable
	environment := append([]string(nil), s.Environment...)
	if starter == nil {
		if executable.Path() != "" {
			starter = func(ctx context.Context, argv []string, root string, problems *ProblemStore) (*Client, error) {
				return startBoundWithProblems(ctx, executable, argv, root, environment, problems)
			}
		} else {
			starter = startWithProblems
		}
	}
	openCloseSync := s.OpenCloseSync
	problems := s.Problems()
	s.mu.Unlock()

	startedClient, startErr := starter(startCtx, argv, root, problems)
	cancel()
	if startErr == nil && startedClient != nil && openCloseSync {
		capabilities := startedClient.Capabilities()
		capabilities.Sync.OpenClose = true
		startedClient.setCapabilities(capabilities)
	}
	if startErr != nil {
		problems.unavailable()
	}

	s.mu.Lock()
	closed := s.closed
	attempt.client, attempt.err = startedClient, startErr
	if closed {
		// Publish the terminal server state before waking callers that shared
		// this attempt. The successfully started client was never installed and
		// is closed below; returning it to a waiter would hand out exactly that
		// discarded runtime while Close is tearing it down.
		attempt.client = nil
		attempt.err = fmt.Errorf("language server is closed")
	} else if attempt.err == nil {
		s.client = attempt.client
		s.lastError = ""
		problems.markAvailable()
	} else {
		s.lastError = boundedStatusError(attempt.err)
	}
	if s.starting == attempt {
		s.starting = nil
	}
	close(attempt.done)
	s.mu.Unlock()

	if closed && startedClient != nil {
		startedClient.Close()
	}
	// Failures are intentionally not cached. A caller-owned deadline, a
	// process race, or a temporarily unavailable binary must not disable LSP
	// for the rest of the session; the next fresh call starts one new attempt.
	return attempt.client, attempt.err
}

// Problems returns the stable diagnostics store for this server, even before
// the runtime is started. A TUI can subscribe during assembly and keep the
// same store through startup, failure, restart UI, and shutdown states.
func (s *Server) Problems() *ProblemStore {
	s.problemsOnce.Do(func() { s.problems = NewProblemStore(s.Root) })
	return s.problems
}

// Capabilities starts the runtime when necessary and returns its normalized
// initialize result.
func (s *Server) Capabilities(ctx context.Context) (Capabilities, error) {
	client, err := s.get(ctx)
	if err != nil {
		return Capabilities{}, err
	}
	return client.Capabilities(), nil
}

// DocumentSymbols returns one bounded, deterministic file outline.
func (s *Server) DocumentSymbols(ctx context.Context, path string, limit int) ([]Symbol, bool, error) {
	client, err := s.get(ctx)
	if err != nil {
		return nil, false, err
	}
	return client.DocumentSymbolsBounded(ctx, path, limit)
}

// WorkspaceSymbols searches the runtime's workspace symbol index.
func (s *Server) WorkspaceSymbols(ctx context.Context, query string, limit int) ([]Symbol, bool, error) {
	client, err := s.get(ctx)
	if err != nil {
		return nil, false, err
	}
	return client.WorkspaceSymbols(ctx, query, limit)
}

// DefinitionAtSymbol synchronizes path from one exact disk snapshot, locates
// symbol on the 1-based line, and asks the server for its definition.
func (s *Server) DefinitionAtSymbol(ctx context.Context, path string, line int, symbol string) ([]Location, error) {
	client, err := s.get(ctx)
	if err != nil {
		return nil, err
	}
	return client.DefinitionAtSymbol(ctx, path, line, symbol)
}

// ReferencesAtSymbol synchronizes path from one exact disk snapshot, locates
// symbol on the 1-based line, and asks the server for its references.
func (s *Server) ReferencesAtSymbol(ctx context.Context, path string, line int, symbol string) ([]Location, error) {
	client, err := s.get(ctx)
	if err != nil {
		return nil, err
	}
	return client.ReferencesAtSymbol(ctx, path, line, symbol)
}

// Close shuts the server down if a call ever started it.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	client := s.client
	if s.starting != nil {
		s.starting.cancel()
	}
	s.mu.Unlock()
	s.Problems().unavailable()
	if client != nil {
		client.Close()
	}
}

// Resolver is what the tools need from the registry: workspace containment
// for the path the model names, and the workspace-relative rendering every
// other tool answers with.
type Resolver interface {
	Resolve(path string) (string, error)
	Display(abs string) string
}

// NewDefinition and NewReferences build the two tools over one server.
func NewDefinition(s *Server, r Resolver) tools.Tool {
	return &locateTool{server: s, r: r, name: "definition",
		desc: "Where the symbol is defined. Give the file, the 1-based line, and the symbol " +
			"as it appears on that line — taken straight from a grep or astgrep hit — and " +
			"the language server answers with the defining file:line, precisely, from a " +
			"live syntax model rather than a text search.",
		ask: func(ctx context.Context, c *Client, path string, line int, symbol string) ([]Location, error) {
			return c.DefinitionAtSymbol(ctx, path, line, symbol)
		}}
}

func NewReferences(s *Server, r Resolver) tools.Tool {
	return &locateTool{server: s, r: r, name: "references",
		desc: "Every place the symbol is used, declaration included. Give the file, the " +
			"1-based line, and the symbol as it appears on that line; the language server " +
			"answers with file:line for each use, precisely, which is the whole-picture " +
			"question a text search only approximates.",
		ask: func(ctx context.Context, c *Client, path string, line int, symbol string) ([]Location, error) {
			return c.ReferencesAtSymbol(ctx, path, line, symbol)
		}}
}

// NewOutline builds a bounded document-symbol tool over the shared server.
func NewOutline(s *Server, r Resolver) tools.Tool {
	return &outlineTool{server: s, r: r}
}

// NewSymbols builds a bounded workspace-symbol search tool over the shared
// server. The name stays short because it is part of every model tool list.
func NewSymbols(s *Server, r Resolver) tools.Tool {
	return &symbolsTool{server: s, r: r}
}

type locateTool struct {
	server *Server
	r      Resolver
	name   string
	desc   string
	ask    func(context.Context, *Client, string, int, string) ([]Location, error)
}

func (t *locateTool) Name() string        { return t.name }
func (t *locateTool) Description() string { return t.desc }

// ParallelSafe: queries are read-only and the client routes concurrent
// requests by id.
func (t *locateTool) ParallelSafe() bool { return true }

func (t *locateTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File the symbol appears in, relative to the workspace root."},
    "line": {"type": "integer", "description": "1-based line the symbol appears on."},
    "symbol": {"type": "string", "description": "The symbol's name exactly as written on that line."}
  },
  "required": ["path", "line", "symbol"]
}`)
}

type locateInput struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Symbol string `json:"symbol"`
}

func (t *locateTool) Plan(input json.RawMessage) (tools.Plan, error) {
	var in locateInput
	if err := json.Unmarshal(input, &in); err != nil {
		return tools.Plan{}, fmt.Errorf("%s: %w", t.name, err)
	}
	if in.Path == "" || in.Line < 1 || strings.TrimSpace(in.Symbol) == "" {
		return tools.Plan{}, fmt.Errorf("%s: path, a 1-based line, and the symbol are all required", t.name)
	}
	abs, err := t.r.Resolve(in.Path)
	if err != nil {
		return tools.Plan{}, err
	}
	return tools.Plan{
		Request: permission.Request{
			Tool:   t.name,
			Effect: permission.EffectRead,
			Path:   t.r.Display(abs),
			Detail: fmt.Sprintf("%s at line %d", in.Symbol, in.Line),
		},
		Run: func(ctx context.Context) (tools.Result, error) {
			return t.run(ctx, in, abs)
		},
	}, nil
}

func (t *locateTool) run(ctx context.Context, in locateInput, abs string) (tools.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	client, err := t.server.get(ctx)
	if err != nil {
		return tools.Result{Content: fmt.Sprintf("the language server did not start: %v", err), IsError: true}, nil
	}
	locs, err := t.ask(ctx, client, abs, in.Line, in.Symbol)
	if err != nil {
		return tools.Result{Content: fmt.Sprintf("%s failed: %v", t.name, err), IsError: true}, nil
	}
	if len(locs) == 0 {
		return tools.Result{Content: fmt.Sprintf("the server found nothing for %s at %s:%d",
			in.Symbol, t.r.Display(abs), in.Line)}, nil
	}

	var b strings.Builder
	for _, l := range locs {
		fmt.Fprintf(&b, "%s:%d\n", t.r.Display(l.Path), l.Line)
	}
	return tools.Result{Content: strings.TrimRight(b.String(), "\n")}, nil
}

type outlineTool struct {
	server *Server
	r      Resolver
}

func (t *outlineTool) Name() string { return "outline" }

func (t *outlineTool) Description() string {
	return "Show a file's structural outline from its language server: packages, types, " +
		"methods, functions, fields, and nested declarations with precise locations. " +
		"Use it before reading a large source file or when you need its shape rather than " +
		"every line. Results preserve hierarchy and are bounded."
}

func (t *outlineTool) ParallelSafe() bool { return true }

func (t *outlineTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Source file relative to the workspace root."},
    "limit": {"type": "integer", "minimum": 1, "maximum": 2000, "description": "Maximum declarations to return. Defaults to 1000."}
  },
  "required": ["path"]
}`)
}

type outlineInput struct {
	Path  string `json:"path"`
	Limit int    `json:"limit"`
}

func (t *outlineTool) Plan(input json.RawMessage) (tools.Plan, error) {
	var in outlineInput
	if err := json.Unmarshal(input, &in); err != nil {
		return tools.Plan{}, fmt.Errorf("outline: %w", err)
	}
	if strings.TrimSpace(in.Path) == "" {
		return tools.Plan{}, fmt.Errorf("outline: path is required")
	}
	if in.Limit < 0 || in.Limit > maxDocumentSymbolLimit {
		return tools.Plan{}, fmt.Errorf("outline: limit must be between 1 and %d when set", maxDocumentSymbolLimit)
	}
	abs, err := t.r.Resolve(in.Path)
	if err != nil {
		return tools.Plan{}, err
	}
	return tools.Plan{
		Request: permission.Request{
			Tool: t.Name(), Effect: permission.EffectRead, Path: t.r.Display(abs),
			Detail: "language-server document outline",
		},
		Run: func(ctx context.Context) (tools.Result, error) {
			ctx, cancel := context.WithTimeout(ctx, queryTimeout)
			defer cancel()
			symbols, truncated, err := t.server.DocumentSymbols(ctx, abs, in.Limit)
			if err != nil {
				return tools.Result{Content: fmt.Sprintf("outline failed: %v", err), IsError: true}, nil
			}
			if len(symbols) == 0 {
				return tools.Result{Content: fmt.Sprintf("the server found no declarations in %s", t.r.Display(abs))}, nil
			}
			return tools.Result{Content: renderOutline(symbols, truncated, t.r)}, nil
		},
	}, nil
}

func renderOutline(symbols []Symbol, truncated bool, r Resolver) string {
	var b strings.Builder
	for _, symbol := range symbols {
		fmt.Fprintf(&b, "%s%s %s", strings.Repeat("  ", symbol.Depth), symbol.Kind, symbol.Name)
		if symbol.Detail != "" {
			fmt.Fprintf(&b, " — %s", symbol.Detail)
		}
		// Symbol columns are UTF-16 code units. Model-facing file locations use
		// line-only rendering until the editor boundary can convert against its
		// exact current text; printing the raw number as a human column lies for
		// every astral rune before the declaration.
		fmt.Fprintf(&b, " — %s:%d\n", r.Display(symbol.Path), symbol.SelectionRange.Start.Line+1)
	}
	if truncated {
		b.WriteString("… outline truncated; use a higher limit or inspect a narrower file region\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

type symbolsTool struct {
	server *Server
	r      Resolver
}

func (t *symbolsTool) Name() string { return "symbols" }

func (t *symbolsTool) Description() string {
	return "Search declarations across the workspace by name using the language server's " +
		"semantic index. Prefer this over grep when looking for a type, function, method, or " +
		"other code symbol rather than matching text. Results are sorted and bounded."
}

func (t *symbolsTool) ParallelSafe() bool { return true }

func (t *symbolsTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "Symbol name or fuzzy name fragment to search for."},
    "limit": {"type": "integer", "minimum": 1, "maximum": 200, "description": "Maximum matches to return. Defaults to 50."}
  },
  "required": ["query"]
}`)
}

type symbolsInput struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

func (t *symbolsTool) Plan(input json.RawMessage) (tools.Plan, error) {
	var in symbolsInput
	if err := json.Unmarshal(input, &in); err != nil {
		return tools.Plan{}, fmt.Errorf("symbols: %w", err)
	}
	in.Query = strings.TrimSpace(in.Query)
	if in.Query == "" {
		return tools.Plan{}, fmt.Errorf("symbols: query is required")
	}
	if in.Limit < 0 || in.Limit > maxWorkspaceSymbolLimit {
		return tools.Plan{}, fmt.Errorf("symbols: limit must be between 1 and %d when set", maxWorkspaceSymbolLimit)
	}
	root, err := t.r.Resolve(".")
	if err != nil {
		return tools.Plan{}, err
	}
	return tools.Plan{
		Request: permission.Request{
			Tool: t.Name(), Effect: permission.EffectRead, Path: t.r.Display(root),
			Detail: fmt.Sprintf("workspace symbol search for %q", in.Query),
		},
		Run: func(ctx context.Context) (tools.Result, error) {
			ctx, cancel := context.WithTimeout(ctx, queryTimeout)
			defer cancel()
			symbols, truncated, err := t.server.WorkspaceSymbols(ctx, in.Query, in.Limit)
			if err != nil {
				return tools.Result{Content: fmt.Sprintf("symbols failed: %v", err), IsError: true}, nil
			}
			if len(symbols) == 0 {
				return tools.Result{Content: fmt.Sprintf("the server found no workspace symbols matching %q", in.Query)}, nil
			}
			return tools.Result{Content: renderWorkspaceSymbols(symbols, truncated, t.r)}, nil
		},
	}, nil
}

func renderWorkspaceSymbols(symbols []Symbol, truncated bool, r Resolver) string {
	var b strings.Builder
	for _, symbol := range symbols {
		fmt.Fprintf(&b, "%s %s", symbol.Kind, symbol.Name)
		if symbol.Container != "" {
			fmt.Fprintf(&b, " in %s", symbol.Container)
		}
		fmt.Fprintf(&b, " — %s:%d\n", r.Display(symbol.Path), symbol.SelectionRange.Start.Line+1)
	}
	if truncated {
		b.WriteString("… results truncated; narrow the query or raise the limit\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
