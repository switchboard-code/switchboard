package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/lsp"
	"github.com/switchboard-code/switchboard/internal/workspace"
)

const (
	lspQueryTimeout = 20 * time.Second
	lspRowLimit     = 1_000
)

// lspRuntime is deliberately narrow so the TUI can exercise lazy-start and
// capability failures without launching a process in tests. The production
// value is the one *lsp.Server assembled before the provider's frozen schema.
type lspRuntime interface {
	Status() lsp.ServerStatus
	Problems() *lsp.ProblemStore
	DocumentSymbols(context.Context, string, int) ([]lsp.Symbol, bool, error)
	WorkspaceSymbols(context.Context, string, int) ([]lsp.Symbol, bool, error)
	DefinitionAtSymbol(context.Context, string, int, string) ([]lsp.Location, error)
	ReferencesAtSymbol(context.Context, string, int, string) ([]lsp.Location, error)
	Close()
}

func optionalLSPRuntime(server *lsp.Server) lspRuntime {
	if server == nil {
		return nil
	}
	return server
}

type lspResolver interface {
	Resolve(string) (string, error)
	Display(string) string
}

type lspPanelKind uint8

const (
	lspOutline lspPanelKind = iota
	lspSymbols
	lspProblems
	lspDefinition
	lspReferences
)

type lspRow struct {
	id         string
	label      string
	detail     string
	path       string
	copyPath   string
	line       int
	navigable  bool
	freshness  lsp.Freshness
	UTF16Start int
}

type lspView struct {
	server     lspRuntime
	resolver   lspResolver
	kind       lspPanelKind
	title      string
	query      string
	path       string
	line       int
	symbol     string
	filter     lsp.ProblemFilter
	sessionID  string
	generation uint64
	request    uint64
	ctx        context.Context
	cancel     context.CancelFunc

	loading   string
	err       error
	rows      []lspRow
	selected  int
	page      int
	truncated bool
	problems  lsp.ProblemSnapshot
	status    lsp.ServerStatus
	stale     bool
}

type lspLoadedMsg struct {
	view       *lspView
	sessionID  string
	generation uint64
	request    uint64
	rows       []lspRow
	truncated  bool
	problems   lsp.ProblemSnapshot
	status     lsp.ServerStatus
	err        error
}

type lspProblemsChangedMsg struct {
	server     lspRuntime
	source     <-chan uint64
	generation uint64
	open       bool
}

type lspCopiedMsg struct {
	view       *lspView
	sessionID  string
	generation uint64
	text       string
	err        error
}

type lspEditorReadyMsg struct {
	view       *lspView
	sessionID  string
	generation uint64
	path       string
	argv       []string
	err        error
}

type lspEditorDoneMsg struct {
	view       *lspView
	sessionID  string
	generation uint64
	path       string
	err        error
}

func cmdLSP(m *tuiModel, _ string) tea.Cmd {
	if m.app.lsp == nil {
		m.addInfo(lspUnavailable(m))
		return nil
	}
	status := m.app.lsp.Status() // The status command must remain non-starting.
	name := status.Executable
	if name == "" {
		name = "language server"
	}
	line := fmt.Sprintf("LSP · %s · %s", name, status.State)
	switch status.State {
	case lsp.ServerConfigured:
		line += " (lazy; no process is started by /lsp)"
	case lsp.ServerStarting:
		line += " (an explicit semantic request is starting it)"
	case lsp.ServerRunning:
		features := lspCapabilityNames(status.Capabilities)
		if len(features) == 0 {
			line += " · no supported semantic capabilities advertised"
		} else {
			line += " · " + strings.Join(features, ", ")
		}
	case lsp.ServerClosed:
		line += " · restart Switchboard to use semantic commands"
	}
	if status.LastError != "" {
		line += "\nlast startup failed: " + status.LastError + " (the next semantic request retries)"
	}
	line += "\ndiagnostics use partial push coverage; an empty store does not prove the workspace clean"
	m.addInfo(line)
	return nil
}

func cmdOutline(m *tuiModel, args string) tea.Cmd {
	path := strings.TrimSpace(args)
	if path == "" {
		return noticeCmd("warn", "usage: /outline <path>")
	}
	abs, err := m.app.loop.Tools.Resolve(path)
	if err != nil {
		return noticeCmd("error", "outline: "+err.Error())
	}
	return m.openLSPView(lspOutline, "outline "+m.app.loop.Tools.Display(abs), func(v *lspView) {
		v.path = abs
	})
}

func cmdSymbols(m *tuiModel, args string) tea.Cmd {
	query := strings.TrimSpace(args)
	if query == "" {
		return noticeCmd("warn", "usage: /symbols <query>")
	}
	return m.openLSPView(lspSymbols, "symbols “"+query+"”", func(v *lspView) {
		v.query = query
	})
}

func cmdProblems(m *tuiModel, args string) tea.Cmd {
	path := strings.TrimSpace(args)
	var filter lsp.ProblemFilter
	title := "problems"
	if path != "" {
		abs, err := m.app.loop.Tools.Resolve(path)
		if err != nil {
			return noticeCmd("error", "problems: "+err.Error())
		}
		filter.Path = abs
		title += " " + m.app.loop.Tools.Display(abs)
	}
	return m.openLSPView(lspProblems, title, func(v *lspView) {
		v.filter = filter
	})
}

func cmdDefinition(m *tuiModel, args string) tea.Cmd {
	return cmdLSPLocate(m, lspDefinition, args)
}

func cmdReferences(m *tuiModel, args string) tea.Cmd {
	return cmdLSPLocate(m, lspReferences, args)
}

func cmdLSPLocate(m *tuiModel, kind lspPanelKind, args string) tea.Cmd {
	path, line, symbol, err := parseLSPLocation(args)
	name := "definition"
	if kind == lspReferences {
		name = "references"
	}
	if err != nil {
		return noticeCmd("warn", "usage: /"+name+" <path>:<line> <symbol> ("+err.Error()+")")
	}
	abs, err := m.app.loop.Tools.Resolve(path)
	if err != nil {
		return noticeCmd("error", name+": "+err.Error())
	}
	title := fmt.Sprintf("%s %s at %s:%d", name, symbol, m.app.loop.Tools.Display(abs), line)
	return m.openLSPView(kind, title, func(v *lspView) {
		v.path, v.line, v.symbol = abs, line, symbol
	})
}

func parseLSPLocation(args string) (string, int, string, error) {
	args = strings.TrimSpace(args)
	split := strings.LastIndexAny(args, " \t")
	if split < 1 {
		return "", 0, "", errors.New("path, line, and symbol are required")
	}
	location, symbol := strings.TrimSpace(args[:split]), strings.TrimSpace(args[split+1:])
	colon := strings.LastIndex(location, ":")
	if colon < 1 || symbol == "" {
		return "", 0, "", errors.New("path, line, and symbol are required")
	}
	line, err := strconv.Atoi(strings.TrimSpace(location[colon+1:]))
	if err != nil || line < 1 {
		return "", 0, "", errors.New("line must be a positive integer")
	}
	path := strings.TrimSpace(location[:colon])
	if path == "" {
		return "", 0, "", errors.New("path is required")
	}
	return path, line, symbol, nil
}

func (m *tuiModel) openLSPView(kind lspPanelKind, title string, configure func(*lspView)) tea.Cmd {
	if m.app.lsp == nil {
		return noticeCmd("warn", lspUnavailable(m))
	}
	m.closeFullscreen()
	m.lspGeneration++
	ctx, cancel := context.WithCancel(context.Background())
	view := &lspView{
		server: m.app.lsp, resolver: m.app.loop.Tools, kind: kind, title: title,
		sessionID: currentSessionID(m), generation: m.lspGeneration,
		ctx: ctx, cancel: cancel, loading: "asking the language server…",
	}
	configure(view)
	m.full = view
	return view.loadCmd()
}

func lspUnavailable(m *tuiModel) string {
	if m != nil && m.app != nil && strings.TrimSpace(m.app.lspNote) != "" {
		return m.app.lspNote
	}
	return "no trusted language server is configured for this workspace; install the ecosystem server, add its project marker, then use /trust grant when prompted"
}

func lspCapabilityNames(capabilities lsp.Capabilities) []string {
	var names []string
	for _, item := range []struct {
		name string
		on   bool
	}{
		{"definition", capabilities.Definition},
		{"references", capabilities.References},
		{"outline", capabilities.DocumentSymbols},
		{"symbols", capabilities.WorkspaceSymbols},
		{"hover", capabilities.Hover},
	} {
		if item.on {
			names = append(names, item.name)
		}
	}
	return names
}

func (v *lspView) loadCmd() tea.Cmd {
	v.request++
	request := v.request
	return func() tea.Msg {
		queryCtx, cancel := context.WithTimeout(v.ctx, lspQueryTimeout)
		defer cancel()
		msg := lspLoadedMsg{view: v, sessionID: v.sessionID, generation: v.generation, request: request}
		switch v.kind {
		case lspOutline:
			var symbols []lsp.Symbol
			symbols, msg.truncated, msg.err = v.server.DocumentSymbols(queryCtx, v.path, lspRowLimit)
			msg.rows = lspSymbolRows(v.resolver, symbols, true)
			msg.truncated = msg.truncated || len(symbols) > len(msg.rows)
		case lspSymbols:
			var symbols []lsp.Symbol
			symbols, msg.truncated, msg.err = v.server.WorkspaceSymbols(queryCtx, v.query, lspRowLimit)
			msg.rows = lspSymbolRows(v.resolver, symbols, false)
			msg.truncated = msg.truncated || len(symbols) > len(msg.rows)
		case lspDefinition, lspReferences:
			var locations []lsp.Location
			if v.kind == lspDefinition {
				locations, msg.err = v.server.DefinitionAtSymbol(queryCtx, v.path, v.line, v.symbol)
			} else {
				locations, msg.err = v.server.ReferencesAtSymbol(queryCtx, v.path, v.line, v.symbol)
			}
			msg.rows = lspLocationRows(v.resolver, locations)
			msg.truncated = len(locations) > len(msg.rows)
		case lspProblems:
			msg.status = v.server.Status()
			msg.problems = v.server.Problems().Snapshot(v.filter)
			msg.rows = lspProblemRows(v.resolver, msg.problems)
			msg.truncated = msg.problems.Dropped > 0 || len(msg.rows) < msg.problems.Total
		}
		if len(msg.rows) > lspRowLimit {
			msg.rows = msg.rows[:lspRowLimit]
			msg.truncated = true
		}
		return msg
	}
}

func lspSymbolRows(resolver lspResolver, symbols []lsp.Symbol, hierarchical bool) []lspRow {
	rows := make([]lspRow, 0, min(len(symbols), lspRowLimit))
	for _, symbol := range symbols {
		display, path, navigable := lspDisplayPath(resolver, symbol.Path)
		line := symbol.SelectionRange.Start.Line + 1
		indent := ""
		if hierarchical {
			indent = strings.Repeat("  ", symbol.Depth)
		}
		label := fmt.Sprintf("%s%s %s — %s:%d", indent, symbol.Kind, symbol.Name, display, line)
		detail := symbol.Detail
		if symbol.Container != "" {
			detail = "in " + symbol.Container + lspJoinDetail(detail)
		}
		rows = append(rows, lspRow{
			id: fmt.Sprintf("symbol\x00%s\x00%d\x00%d\x00%d\x00%s\x00%s\x00%d", path, line,
				symbol.SelectionRange.Start.Character, symbol.Kind, symbol.Name, symbol.Container, symbol.Depth),
			label: label, detail: detail, path: path, copyPath: display, line: line,
			navigable: navigable, UTF16Start: symbol.SelectionRange.Start.Character,
		})
		if len(rows) == lspRowLimit {
			break
		}
	}
	return rows
}

func lspLocationRows(resolver lspResolver, locations []lsp.Location) []lspRow {
	rows := make([]lspRow, 0, min(len(locations), lspRowLimit))
	for _, location := range locations {
		display, path, navigable := lspDisplayPath(resolver, location.Path)
		rows = append(rows, lspRow{
			id:    fmt.Sprintf("location\x00%s\x00%d\x00%d", path, location.Line, location.Character),
			label: fmt.Sprintf("%s:%d", display, location.Line), path: path, copyPath: display,
			line: location.Line, navigable: navigable, UTF16Start: max(location.Character-1, 0),
		})
		if len(rows) == lspRowLimit {
			break
		}
	}
	return rows
}

func lspProblemRows(resolver lspResolver, snapshot lsp.ProblemSnapshot) []lspRow {
	rows := make([]lspRow, 0, min(snapshot.Total, lspRowLimit))
	duplicates := make(map[string]int)
	for _, document := range snapshot.Documents {
		for _, problem := range document.Problems {
			display, path, navigable := lspProblemPath(resolver, document, problem)
			severity := lspSeverityName(problem.Severity)
			label := fmt.Sprintf("[%s] %s %s:%d — %s", lspFreshnessName(document.Freshness), severity, display, problem.Line, problem.Message)
			detail := ""
			if problem.Source != "" {
				detail = problem.Source
			}
			if problem.Code != "" {
				detail += " " + problem.Code
			}
			baseID := fmt.Sprintf("problem\x00%s\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%s\x00%s\x00%s", problem.URI,
				problem.Line, problem.Column, problem.EndLine, problem.EndColumn, problem.Severity, len(problem.Related),
				problem.Source, problem.Code, problem.Message)
			ordinal := duplicates[baseID]
			duplicates[baseID] = ordinal + 1
			rows = append(rows, lspRow{
				id:    fmt.Sprintf("%s\x00%d", baseID, ordinal),
				label: label, detail: detail, path: path, copyPath: display, line: problem.Line,
				navigable: navigable, freshness: document.Freshness, UTF16Start: max(problem.Column-1, 0),
			})
			if len(rows) == lspRowLimit {
				return rows
			}
		}
	}
	return rows
}

func lspDisplayPath(resolver lspResolver, candidate string) (display, path string, navigable bool) {
	clean := filepath.Clean(candidate)
	abs, err := resolver.Resolve(clean)
	if err == nil {
		return filepath.ToSlash(resolver.Display(abs)), abs, true
	}
	if clean == "." || clean == "" {
		return "unknown file", clean, false
	}
	return filepath.ToSlash(clean), clean, false
}

func lspProblemPath(resolver lspResolver, document lsp.DocumentProblems, problem lsp.Problem) (string, string, bool) {
	if document.Navigable && problem.Navigable && problem.Path != "" {
		if display, path, ok := lspDisplayPath(resolver, problem.Path); ok {
			return display, path, true
		}
	}
	label := problem.Path
	if label == "" {
		label = problem.URI
	}
	if label == "" {
		label = document.URI
	}
	return workspaceSanitize(label), problem.Path, false
}

func lspJoinDetail(detail string) string {
	if detail == "" {
		return ""
	}
	return " · " + detail
}

func lspSeverityName(severity lsp.Severity) string {
	switch severity {
	case lsp.SeverityError:
		return "error"
	case lsp.SeverityWarning:
		return "warning"
	case lsp.SeverityInformation:
		return "info"
	case lsp.SeverityHint:
		return "hint"
	default:
		return "problem"
	}
}

func lspFreshnessName(freshness lsp.Freshness) string {
	switch freshness {
	case lsp.Fresh:
		return "fresh"
	case lsp.Stale:
		return "stale"
	case lsp.Unversioned:
		return "unversioned"
	default:
		return "pending"
	}
}

func (m *tuiModel) lspViewMatches(view *lspView, sessionID string, generation uint64) bool {
	current, ok := m.full.(*lspView)
	return ok && current == view && current.generation == generation && current.sessionID == sessionID &&
		currentSessionID(m) == sessionID
}

func (m *tuiModel) onLSPLoaded(msg lspLoadedMsg) tea.Cmd {
	if !m.lspViewMatches(msg.view, msg.sessionID, msg.generation) || msg.request != msg.view.request {
		return nil
	}
	v := msg.view
	v.loading = ""
	v.err = friendlyLSPError(msg.err)
	v.truncated = msg.truncated
	if msg.err != nil {
		return nil
	}
	v.problems = msg.problems
	v.status = msg.status
	v.replaceRows(msg.rows)
	return nil
}

func friendlyLSPError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("language-server request timed out; retry or use /lsp to inspect its state")
	}
	if strings.Contains(err.Error(), "does not advertise") {
		return fmt.Errorf("unsupported by this language server: %w", err)
	}
	return err
}

func (v *lspView) replaceRows(rows []lspRow) {
	selectedID, oldIndex := "", v.selected
	if len(v.rows) > 0 {
		v.clampSelection()
		selectedID = v.rows[v.selected].id
	}
	v.rows = rows
	v.selected = workspaceClamp(oldIndex, 0, max(len(v.rows)-1, 0))
	if selectedID != "" {
		for index := range v.rows {
			if v.rows[index].id == selectedID {
				v.selected = index
				break
			}
		}
	}
}

func waitLSPProblems(server lspRuntime, source <-chan uint64) tea.Cmd {
	if server == nil || source == nil {
		return nil
	}
	return func() tea.Msg {
		generation, open := <-source
		return lspProblemsChangedMsg{server: server, source: source, generation: generation, open: open}
	}
}

func (m *tuiModel) onLSPProblemsChanged(msg lspProblemsChangedMsg) tea.Cmd {
	if m.app == nil || m.app.lsp != msg.server || m.app.lspProblems != msg.source || !msg.open {
		return nil
	}
	commands := []tea.Cmd{waitLSPProblems(msg.server, msg.source)}
	if view, ok := m.full.(*lspView); ok && view.kind == lspProblems && view.server == msg.server {
		view.loading = "refreshing diagnostics…"
		commands = append(commands, view.loadCmd())
	}
	return tea.Batch(commands...)
}

func (v *lspView) key(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "q", "esc":
		v.close()
		return true, nil
	case "enter", "e":
		return false, v.editorCmd()
	case "c":
		return false, v.copyCmd()
	case "up", "k":
		v.move(-1)
	case "down", "j":
		v.move(1)
	case "pgup", "ctrl+u":
		v.move(-max(v.page, 1))
	case "pgdown", "ctrl+d":
		v.move(max(v.page, 1))
	case "g", "home":
		v.goTo(false)
	case "G", "end":
		v.goTo(true)
	}
	return false, nil
}

func (v *lspView) mouse(msg tea.MouseMsg) tea.Cmd {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		v.move(-3)
	case tea.MouseButtonWheelDown:
		v.move(3)
	}
	return nil
}

func (v *lspView) close() {
	if v.cancel != nil {
		v.cancel()
		v.cancel = nil
	}
}

func (v *lspView) clampSelection() {
	if len(v.rows) == 0 {
		v.selected = 0
		return
	}
	v.selected = workspaceClamp(v.selected, 0, len(v.rows)-1)
}

func (v *lspView) move(delta int) {
	if len(v.rows) == 0 {
		return
	}
	v.selected = workspaceClamp(v.selected+delta, 0, len(v.rows)-1)
}

func (v *lspView) goTo(end bool) {
	if len(v.rows) == 0 {
		return
	}
	v.selected = 0
	if end {
		v.selected = len(v.rows) - 1
	}
}

func (v *lspView) activeRow() (lspRow, bool) {
	if len(v.rows) == 0 {
		return lspRow{}, false
	}
	v.clampSelection()
	return v.rows[v.selected], true
}

func (v *lspView) copyCmd() tea.Cmd {
	row, ok := v.activeRow()
	if !ok {
		return noticeCmd("warn", "there is no language-server location to copy")
	}
	text := fmt.Sprintf("%s:%d", row.copyPath, max(row.line, 1))
	return func() tea.Msg {
		return lspCopiedMsg{view: v, sessionID: v.sessionID, generation: v.generation, text: text, err: workspaceClipboardWrite(text)}
	}
}

func (v *lspView) editorCmd() tea.Cmd {
	row, ok := v.activeRow()
	if !ok {
		return noticeCmd("warn", "there is no language-server location to open")
	}
	if !row.navigable {
		return noticeCmd("warn", "that result is outside the workspace and is not navigable")
	}
	if v.stale {
		return noticeCmd("warn", "the workspace changed after this result; rerun the semantic command before opening it")
	}
	return func() tea.Msg {
		abs, err := v.resolver.Resolve(row.path) // Re-check symlink containment at Enter.
		msg := lspEditorReadyMsg{view: v, sessionID: v.sessionID, generation: v.generation, path: row.copyPath, err: err}
		if err == nil {
			// Raw LSP columns are UTF-16. Line-only navigation is honest until an
			// editor text model can convert the exact snapshot.
			msg.argv, msg.err = workspaceEditorArgv(abs, workspace.Position{Line: max(row.line, 1), Column: 1})
		}
		return msg
	}
}

func (m *tuiModel) onLSPCopied(msg lspCopiedMsg) {
	if !m.lspViewMatches(msg.view, msg.sessionID, msg.generation) {
		return
	}
	if msg.err != nil {
		m.addNotice("error", "copy: "+msg.err.Error())
		return
	}
	m.addNotice("", "copied "+msg.text)
}

func (m *tuiModel) onLSPEditorReady(msg lspEditorReadyMsg) tea.Cmd {
	if !m.lspViewMatches(msg.view, msg.sessionID, msg.generation) {
		return nil
	}
	if msg.err != nil {
		return noticeCmd("error", "editor: "+msg.err.Error())
	}
	if len(msg.argv) == 0 {
		return noticeCmd("error", "editor: no command was configured")
	}
	if msg.view.stale {
		return noticeCmd("warn", "the workspace changed before the editor could open; rerun the semantic command")
	}
	command := sanitizedCommand(msg.argv[0], msg.argv[1:]...)
	return tea.ExecProcess(command, func(err error) tea.Msg {
		return lspEditorDoneMsg{view: msg.view, sessionID: msg.sessionID, generation: msg.generation, path: msg.path, err: err}
	})
}

func (m *tuiModel) onLSPEditorDone(msg lspEditorDoneMsg) tea.Cmd {
	if !m.lspViewMatches(msg.view, msg.sessionID, msg.generation) {
		return nil
	}
	// Returning from an external editor is an invalidation boundary even when
	// the process reports an error: it may have saved before failing. The file
	// index and every location in this semantic result must be treated as old.
	invalidateRestoredWorkspace(m)
	m.lspGeneration++
	msg.view.generation = m.lspGeneration
	msg.view.request++ // reject a semantic result launched before the editor
	if msg.err != nil {
		return noticeCmd("error", "editor: "+msg.err.Error())
	}
	return noticeCmd("", "returned from "+msg.path)
}

func (v *lspView) view(width, height int, th *theme) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	header := th.bold.Render(workspaceFit(" "+workspaceSanitize(v.title), width))
	if height == 1 {
		return header
	}
	footer := th.faint.Render(workspaceFit(" "+v.footerText(), width))
	if height == 2 {
		return header + "\n" + footer
	}
	bodyHeight := height - 2
	v.page = bodyHeight
	body := v.listView(width, bodyHeight, th)
	return header + "\n" + body + "\n" + footer
}

func (v *lspView) footerText() string {
	status := fmt.Sprintf("%d results", len(v.rows))
	switch {
	case v.err != nil:
		status = "error: " + v.err.Error()
	case v.loading != "":
		status = v.loading
	case v.truncated:
		status += " · truncated"
	}
	if v.kind == lspProblems {
		availability := "server unavailable; retained results may be stale"
		switch v.status.State {
		case lsp.ServerConfigured:
			availability = "server not started; diagnostics cover only later synchronized files"
		case lsp.ServerStarting:
			availability = "server starting; diagnostics are incomplete"
		case lsp.ServerRunning:
			availability = "server running; push diagnostics"
		case lsp.ServerClosed:
			availability = "server closed; retained results are stale"
		case "":
			// A test double or an older embedding may not expose a state. Store
			// availability is still weaker than workspace coverage.
			if v.problems.Available {
				availability = "push diagnostics"
			}
		}
		if v.status.State == lsp.ServerRunning && !v.problems.Available {
			availability = "server unavailable; retained results may be stale"
		} else if v.status.State == "" && v.problems.Available {
			availability = "push diagnostics"
		}
		status = "partial coverage · silence ≠ clean · " + availability + " · " + status
	}
	if v.stale {
		status = "workspace changed · rerun this command; displayed results may be stale · " + status
	}
	return workspaceSanitize(status) + " · enter open · c copy path:line · q close"
}

func (v *lspView) listView(width, height int, th *theme) string {
	if v.err != nil {
		return workspaceFill([]string{th.err.Render(workspaceFit(" "+workspaceSanitize(v.err.Error()), width))}, width, height)
	}
	if v.loading != "" && len(v.rows) == 0 {
		return workspaceFill([]string{th.dim.Render(workspaceFit(" "+v.loading, width))}, width, height)
	}
	if len(v.rows) == 0 {
		empty := " no results"
		if v.kind == lspProblems {
			empty = " partial coverage: no diagnostics published; silence is not proof the workspace is clean"
		}
		return workspaceFill([]string{th.dim.Render(workspaceFit(empty, width))}, width, height)
	}
	v.clampSelection()
	start := workspaceClamp(v.selected-height/2, 0, max(len(v.rows)-height, 0))
	end := min(start+height, len(v.rows))
	lines := make([]string, 0, height)
	for index := start; index < end; index++ {
		row := v.rows[index]
		text := row.label
		if row.detail != "" {
			text += "  " + row.detail
		}
		if !row.navigable {
			text += "  [external · copy only]"
		}
		prefix := "  "
		style := th.text
		if index == v.selected {
			prefix, style = "▌ ", th.selected
		}
		line := workspacePad(workspaceFit(prefix+workspaceSanitize(text), width), width)
		lines = append(lines, style.Render(line))
	}
	return workspaceFill(lines, width, height)
}
