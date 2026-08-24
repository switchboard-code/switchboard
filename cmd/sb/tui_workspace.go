package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/switchboard-code/switchboard/internal/terminaltext"
	"github.com/switchboard-code/switchboard/internal/workspace"
)

const (
	workspaceFileMatches = 500
	workspaceSplitWidth  = 92
	workspaceFilterRunes = 128
)

type workspacePanelKind uint8

const (
	workspaceFiles workspacePanelKind = iota
	workspaceSearch
)

// workspaceRuntime is shared by successive workspace panels. Initialization
// and every filesystem operation happen in tea.Cmd goroutines. Invalidation is
// an atomic epoch as well as an Index flag so a tool batch that lands during a
// refresh cannot be accidentally cleared by that refresh's final assignment.
type workspaceRuntime struct {
	path string

	initMu sync.Mutex
	root   atomic.Pointer[workspace.Root]
	index  atomic.Pointer[workspace.Index]
	epoch  atomic.Uint64
}

func newWorkspaceRuntime(path string) *workspaceRuntime {
	return &workspaceRuntime{path: path}
}

func (r *workspaceRuntime) resources(ctx context.Context) (*workspace.Root, *workspace.Index, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if root, index := r.root.Load(), r.index.Load(); root != nil && index != nil {
		return root, index, nil
	}
	r.initMu.Lock()
	defer r.initMu.Unlock()
	if root, index := r.root.Load(), r.index.Load(); root != nil && index != nil {
		return root, index, nil
	}
	root, err := workspace.Open(r.path)
	if err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	index := workspace.NewIndex(root, workspace.DefaultFileLimit)
	r.root.Store(root)
	r.index.Store(index)
	return root, index, nil
}

func (r *workspaceRuntime) refresh(ctx context.Context) (workspace.Snapshot, error) {
	_, index, err := r.resources(ctx)
	if err != nil {
		return workspace.Snapshot{}, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		epoch := r.epoch.Load()
		snapshot, err := index.Refresh(ctx)
		if err != nil {
			return workspace.Snapshot{}, err
		}
		if epoch == r.epoch.Load() {
			return snapshot, nil
		}
		index.Invalidate()
	}
	return workspace.Snapshot{}, errors.New("workspace changed while its file index was refreshing; reopen the panel")
}

func (r *workspaceRuntime) search(ctx context.Context, literal string) ([]workspace.TextMatch, workspace.SearchStatus, error) {
	_, index, err := r.resources(ctx)
	if err != nil {
		return nil, workspace.SearchStatus{}, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		epoch := r.epoch.Load()
		if _, err := index.Refresh(ctx); err != nil {
			return nil, workspace.SearchStatus{}, err
		}
		matches, status, err := index.Search(ctx, literal, workspace.SearchOptions{
			Limit:        workspace.DefaultSearchLimit,
			MaxFileBytes: workspace.DefaultSearchBytes,
		})
		if err != nil {
			return nil, workspace.SearchStatus{}, err
		}
		if epoch == r.epoch.Load() {
			return matches, status, nil
		}
		index.Invalidate()
	}
	return nil, workspace.SearchStatus{}, errors.New("workspace changed while search was running; run the search again")
}

// completeFiles uses the same revision-aware inventory as /files, including
// its git-aware exclusions and invalidation epoch. Filtering stays off the UI
// goroutine too: a 200k-file checkout should not make one typed rune miss a
// frame. The returned epoch binds the matches to the inventory that produced
// them so a tool batch cannot publish stale completion after an edit.
func (r *workspaceRuntime) completeFiles(ctx context.Context, query string, limit int, forceRefresh bool) ([]string, uint64, error) {
	_, index, err := r.resources(ctx)
	if err != nil {
		return nil, 0, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		epoch := r.epoch.Load()
		var snapshot workspace.Snapshot
		if forceRefresh {
			snapshot, err = index.Refresh(ctx)
		} else {
			snapshot, err = index.Ensure(ctx)
		}
		if err != nil {
			return nil, 0, err
		}
		matches := snapshot.Filter(query, limit)
		if epoch == r.epoch.Load() {
			paths := make([]string, len(matches))
			for i := range matches {
				paths[i] = matches[i].File.Path
			}
			return paths, epoch, nil
		}
		index.Invalidate()
		forceRefresh = false
	}
	return nil, 0, errors.New("workspace changed while file completion was refreshing; type another character to retry")
}

func (r *workspaceRuntime) read(ctx context.Context, path string) (workspace.Document, error) {
	root, _, err := r.resources(ctx)
	if err != nil {
		return workspace.Document{}, err
	}
	if err := ctx.Err(); err != nil {
		return workspace.Document{}, err
	}
	return root.Read(path, workspace.DefaultDocumentLimit)
}

func (r *workspaceRuntime) verify(ctx context.Context, location workspace.Location) (string, error) {
	root, _, err := r.resources(ctx)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := root.Verify(location); err != nil {
		return "", err
	}
	return root.Resolve(location.Path)
}

func (r *workspaceRuntime) invalidate() {
	r.epoch.Add(1)
	if index := r.index.Load(); index != nil {
		index.Invalidate()
	}
}

type workspaceRow struct {
	location workspace.Location
	preview  string
	size     int64
}

func (r workspaceRow) line() int {
	if r.location.Range.Start.Line > 0 {
		return r.location.Range.Start.Line
	}
	return 1
}

func (r workspaceRow) column() int {
	if r.location.Range.Start.Column > 0 {
		return r.location.Range.Start.Column
	}
	return 1
}

func (r workspaceRow) label() string {
	if r.location.Range.Start.Line > 0 {
		return r.location.String()
	}
	return r.location.Path
}

type workspaceView struct {
	runtime    *workspaceRuntime
	kind       workspacePanelKind
	literal    string
	sessionID  string
	generation uint64
	ctx        context.Context
	cancel     context.CancelFunc

	loading   string
	err       error
	stale     bool
	truncated bool
	skipped   int
	oversized int
	snapshot  workspace.Snapshot
	allRows   []workspaceRow
	rows      []workspaceRow
	selected  int
	page      int

	filter        string
	filterBefore  string
	filterEditing bool
	filterRequest uint64

	previewRequest uint64
	previewPath    string
	preview        workspace.Document
	previewErr     error
	previewStale   bool
	openWhenReady  bool

	source       bool
	sourceCursor int
}

type workspaceOpenedMsg struct {
	view       *workspaceView
	sessionID  string
	generation uint64
	snapshot   workspace.Snapshot
	matches    []workspace.TextMatch
	status     workspace.SearchStatus
	err        error
}

type workspaceFilteredMsg struct {
	view       *workspaceView
	sessionID  string
	generation uint64
	request    uint64
	deferred   bool
	snapshot   workspace.Snapshot
	query      string
	matches    []workspace.FileMatch
}

type workspacePreviewMsg struct {
	view       *workspaceView
	sessionID  string
	generation uint64
	request    uint64
	expected   workspace.Location
	document   workspace.Document
	stale      bool
	err        error
}

type workspaceCopiedMsg struct {
	view       *workspaceView
	sessionID  string
	generation uint64
	text       string
	err        error
}

type workspaceEditorReadyMsg struct {
	view       *workspaceView
	sessionID  string
	generation uint64
	path       string
	argv       []string
	err        error
}

type workspaceEditorDoneMsg struct {
	view       *workspaceView
	sessionID  string
	generation uint64
	path       string
	err        error
}

// workspaceInvalidatedMsg is sent only from Observer.ToolBatchEnd: that hook
// runs after the ordered tool-result message is durable in the session log.
type workspaceInvalidatedMsg struct{}

func cmdFiles(m *tuiModel, args string) tea.Cmd {
	return m.openWorkspace(workspaceFiles, strings.TrimSpace(args))
}

func cmdWorkspaceSearch(m *tuiModel, args string) tea.Cmd {
	literal := strings.TrimSpace(args)
	if literal == "" {
		return noticeCmd("error", "usage: /search <literal>")
	}
	return m.openWorkspace(workspaceSearch, literal)
}

func (m *tuiModel) openWorkspace(kind workspacePanelKind, query string) tea.Cmd {
	if m.workspaceRuntime == nil {
		m.workspaceRuntime = newWorkspaceRuntime(m.app.workspace)
	}
	sessionID := currentSessionID(m)
	if sessionID == "" {
		return noticeCmd("error", "workspace navigation needs an active session")
	}
	m.closeFullscreen()
	m.workspaceGeneration++
	ctx, cancel := context.WithCancel(context.Background())
	view := &workspaceView{
		runtime: m.workspaceRuntime, kind: kind, literal: query,
		sessionID: sessionID, generation: m.workspaceGeneration,
		ctx: ctx, cancel: cancel, page: 20,
		loading: "refreshing workspace…",
	}
	if kind == workspaceFiles {
		view.filter = query
	}
	m.full = view
	return view.openCmd()
}

func (v *workspaceView) openCmd() tea.Cmd {
	return func() tea.Msg {
		msg := workspaceOpenedMsg{view: v, sessionID: v.sessionID, generation: v.generation}
		if v.kind == workspaceSearch {
			msg.matches, msg.status, msg.err = v.runtime.search(v.ctx, v.literal)
			return msg
		}
		msg.snapshot, msg.err = v.runtime.refresh(v.ctx)
		msg.status = workspace.SearchStatus{Truncated: msg.snapshot.Truncated, Skipped: msg.snapshot.Skipped}
		return msg
	}
}

func (v *workspaceView) filterCmd() tea.Cmd {
	v.filterRequest++
	request := v.filterRequest
	query := v.filter
	snapshot := v.snapshot // immutable until a later open result is installed
	v.loading = "filtering files…"
	return func() tea.Msg {
		timer := time.NewTimer(30 * time.Millisecond)
		defer timer.Stop()
		var done <-chan struct{}
		if v.ctx != nil {
			done = v.ctx.Done()
		}
		select {
		case <-timer.C:
		case <-done:
		}
		return workspaceFilteredMsg{
			view: v, sessionID: v.sessionID, generation: v.generation, request: request,
			deferred: true, snapshot: snapshot, query: query,
		}
	}
}

func (v *workspaceView) runFilterCmd(msg workspaceFilteredMsg) tea.Cmd {
	return func() tea.Msg {
		msg.deferred = false
		// One sentinel beyond the display cap distinguishes exactly 500
		// matches from a truncated list without scanning twice.
		msg.matches = msg.snapshot.Filter(msg.query, workspaceFileMatches+1)
		msg.snapshot = workspace.Snapshot{}
		return msg
	}
}

func (v *workspaceView) previewCmd() tea.Cmd {
	if len(v.rows) == 0 {
		v.clearPreview()
		return nil
	}
	v.clampSelection()
	expected := v.rows[v.selected].location
	v.previewRequest++
	request := v.previewRequest
	v.previewPath = expected.Path
	v.preview = workspace.Document{}
	v.previewErr = nil
	v.previewStale = false
	return func() tea.Msg {
		document, err := v.runtime.read(v.ctx, expected.Path)
		stale := err == nil && expected.Revision.SHA256 != "" && document.Location.Revision != expected.Revision
		return workspacePreviewMsg{
			view: v, sessionID: v.sessionID, generation: v.generation,
			request: request, expected: expected, document: document, stale: stale, err: err,
		}
	}
}

func (v *workspaceView) copyCmd() tea.Cmd {
	location, ok := v.activeLocation()
	if !ok {
		return noticeCmd("warn", "there is no source location to copy")
	}
	text := fmt.Sprintf("%s:%d", location.Path, location.Range.Start.Line)
	return func() tea.Msg {
		err := workspaceClipboardWrite(text)
		return workspaceCopiedMsg{view: v, sessionID: v.sessionID, generation: v.generation, text: text, err: err}
	}
}

func (v *workspaceView) editorCmd() tea.Cmd {
	location, ok := v.activeLocation()
	if !ok || v.preview.Location.Path != location.Path {
		return noticeCmd("warn", "source is still loading; try again when its revision appears")
	}
	if v.previewStale {
		return noticeCmd("warn", "this source view is stale; reopen it before editing")
	}
	location.Revision = v.preview.Location.Revision
	return func() tea.Msg {
		abs, err := v.runtime.verify(v.ctx, location)
		msg := workspaceEditorReadyMsg{view: v, sessionID: v.sessionID, generation: v.generation, path: location.Path, err: err}
		if err == nil {
			msg.argv, msg.err = workspaceEditorArgv(abs, location.Range.Start)
		}
		return msg
	}
}

func currentSessionID(m *tuiModel) string {
	if m == nil || m.app == nil || m.app.loop == nil || m.app.loop.Session == nil {
		return ""
	}
	return m.app.loop.Session.ID()
}

func (m *tuiModel) workspaceViewMatches(view *workspaceView, sessionID string, generation uint64) bool {
	current, ok := m.full.(*workspaceView)
	return ok && current == view && current.generation == generation &&
		generation == m.workspaceGeneration && current.sessionID == sessionID && currentSessionID(m) == sessionID
}

func (m *tuiModel) onWorkspaceOpened(msg workspaceOpenedMsg) tea.Cmd {
	if !m.workspaceViewMatches(msg.view, msg.sessionID, msg.generation) {
		return nil
	}
	v := msg.view
	v.loading = ""
	v.err = msg.err
	v.truncated = msg.status.Truncated
	v.skipped = msg.status.Skipped
	v.oversized = msg.status.Oversized
	if msg.err != nil {
		return nil
	}
	if v.kind == workspaceFiles {
		v.snapshot = msg.snapshot
		return v.filterCmd()
	}
	v.allRows = make([]workspaceRow, 0, len(msg.matches))
	for _, match := range msg.matches {
		v.allRows = append(v.allRows, workspaceRow{location: match.Location, preview: match.Preview})
	}
	v.applySearchFilter()
	return v.previewCmd()
}

func (m *tuiModel) onWorkspaceFiltered(msg workspaceFilteredMsg) tea.Cmd {
	if !m.workspaceViewMatches(msg.view, msg.sessionID, msg.generation) || msg.request != msg.view.filterRequest {
		return nil
	}
	v := msg.view
	if msg.deferred {
		return v.runFilterCmd(msg)
	}
	v.loading = ""
	filterTruncated := len(msg.matches) > workspaceFileMatches
	if filterTruncated {
		msg.matches = msg.matches[:workspaceFileMatches]
	}
	v.truncated = v.snapshot.Truncated || filterTruncated
	v.skipped = v.snapshot.Skipped
	v.oversized = 0
	v.rows = make([]workspaceRow, 0, len(msg.matches))
	for _, match := range msg.matches {
		v.rows = append(v.rows, workspaceRow{
			location: workspace.Location{Path: match.File.Path}, size: match.File.Size,
		})
	}
	v.selected = 0
	return v.previewCmd()
}

func (m *tuiModel) onWorkspacePreview(msg workspacePreviewMsg) tea.Cmd {
	if !m.workspaceViewMatches(msg.view, msg.sessionID, msg.generation) || msg.request != msg.view.previewRequest {
		return nil
	}
	v := msg.view
	if len(v.rows) == 0 || v.rows[v.selected].location.Path != msg.expected.Path {
		return nil
	}
	v.preview = msg.document
	v.previewErr = msg.err
	v.previewStale = msg.stale
	if msg.err == nil && v.openWhenReady {
		v.openWhenReady = false
		v.source = true
		v.sourceCursor = v.rows[v.selected].line()
	}
	return nil
}

func (m *tuiModel) onWorkspaceCopied(msg workspaceCopiedMsg) {
	if !m.workspaceViewMatches(msg.view, msg.sessionID, msg.generation) {
		return
	}
	if msg.err != nil {
		m.addNotice("error", "copy: "+msg.err.Error())
		return
	}
	m.addNotice("", "copied "+msg.text)
}

func (m *tuiModel) onWorkspaceEditorReady(msg workspaceEditorReadyMsg) tea.Cmd {
	if !m.workspaceViewMatches(msg.view, msg.sessionID, msg.generation) {
		return nil
	}
	if msg.err != nil {
		msg.view.previewStale = errors.Is(msg.err, workspace.ErrStaleLocation)
		return noticeCmd("error", "editor: "+msg.err.Error())
	}
	if len(msg.argv) == 0 {
		return noticeCmd("error", "editor: no command was configured")
	}
	command := sanitizedCommand(msg.argv[0], msg.argv[1:]...)
	return tea.ExecProcess(command, func(err error) tea.Msg {
		return workspaceEditorDoneMsg{
			view: msg.view, sessionID: msg.sessionID, generation: msg.generation, path: msg.path, err: err,
		}
	})
}

func (m *tuiModel) onWorkspaceEditorDone(msg workspaceEditorDoneMsg) tea.Cmd {
	if !m.workspaceViewMatches(msg.view, msg.sessionID, msg.generation) {
		return nil
	}
	// An editor can save before exiting non-zero, and preview/index work may
	// still be in flight from before it launched. Advance the shared boundary
	// on every return, rebind this panel for a successful refresh, and leave a
	// failed return visibly stale instead of presenting old source as current.
	invalidateRestoredWorkspace(m)
	msg.view.generation = m.workspaceGeneration
	msg.view.filterRequest++
	msg.view.previewRequest++
	if msg.err != nil {
		return noticeCmd("error", "editor: "+msg.err.Error())
	}
	msg.view.stale = false
	msg.view.previewStale = false
	msg.view.loading = "refreshing after editor…"
	msg.view.err = nil
	return msg.view.openCmd()
}

func (v *workspaceView) key(msg tea.KeyMsg) (bool, tea.Cmd) {
	if v.stale {
		switch msg.String() {
		case "q", "esc":
			v.close()
			return true, nil
		default:
			return false, noticeCmd("warn", "the workspace changed; close and reopen this panel for current source")
		}
	}
	if v.filterEditing {
		return false, v.filterKey(msg)
	}
	switch msg.String() {
	case "q", "esc":
		v.close()
		return true, nil
	case "/":
		if v.source {
			return false, nil
		}
		v.filterEditing = true
		v.filterBefore = v.filter
		return false, nil
	case "enter":
		if v.source {
			v.source = false
			return false, nil
		}
		if len(v.rows) == 0 {
			return false, nil
		}
		if v.preview.Location.Path == v.rows[v.selected].location.Path && v.previewErr == nil {
			v.source = true
			v.sourceCursor = v.rows[v.selected].line()
			return false, nil
		}
		v.openWhenReady = true
		return false, v.previewCmd()
	case "e":
		return false, v.editorCmd()
	case "c":
		return false, v.copyCmd()
	case "up", "k":
		return false, v.move(-1)
	case "down", "j":
		return false, v.move(1)
	case "pgup", "ctrl+u":
		return false, v.move(-max(v.page, 1))
	case "pgdown", "ctrl+d":
		return false, v.move(max(v.page, 1))
	case "g", "home":
		return false, v.goTo(false)
	case "G", "end":
		return false, v.goTo(true)
	}
	return false, nil
}

func (v *workspaceView) filterKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		v.filter = v.filterBefore
		v.filterEditing = false
		return v.applyFilter()
	case "enter":
		v.filterEditing = false
		return nil
	case "backspace":
		runes := []rune(v.filter)
		if len(runes) > 0 {
			v.filter = string(runes[:len(runes)-1])
			return v.applyFilter()
		}
	case "ctrl+u":
		v.filter = ""
		return v.applyFilter()
	default:
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 0 {
			runes := append([]rune(v.filter), msg.Runes...)
			if len(runes) > workspaceFilterRunes {
				runes = runes[:workspaceFilterRunes]
			}
			v.filter = string(runes)
			return v.applyFilter()
		}
	}
	return nil
}

func (v *workspaceView) applyFilter() tea.Cmd {
	v.selected = 0
	if v.kind == workspaceFiles {
		return v.filterCmd()
	}
	v.applySearchFilter()
	return v.previewCmd()
}

func (v *workspaceView) applySearchFilter() {
	needle := strings.ToLower(strings.TrimSpace(v.filter))
	v.rows = v.rows[:0]
	for _, row := range v.allRows {
		if needle == "" || strings.Contains(strings.ToLower(row.location.String()+" "+row.preview), needle) {
			v.rows = append(v.rows, row)
		}
	}
	v.clampSelection()
}

func (v *workspaceView) move(delta int) tea.Cmd {
	if v.source {
		lines := workspaceSourceLines(v.preview.Content)
		v.sourceCursor = workspaceClamp(v.sourceCursor+delta, 1, max(len(lines), 1))
		return nil
	}
	if len(v.rows) == 0 {
		return nil
	}
	v.selected = workspaceClamp(v.selected+delta, 0, len(v.rows)-1)
	v.openWhenReady = false
	return v.previewCmd()
}

func (v *workspaceView) goTo(end bool) tea.Cmd {
	if v.source {
		v.sourceCursor = 1
		if end {
			v.sourceCursor = max(len(workspaceSourceLines(v.preview.Content)), 1)
		}
		return nil
	}
	if len(v.rows) == 0 {
		return nil
	}
	v.selected = 0
	if end {
		v.selected = len(v.rows) - 1
	}
	return v.previewCmd()
}

func (v *workspaceView) mouse(msg tea.MouseMsg) tea.Cmd {
	if v.stale {
		return nil
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		return v.move(-3)
	case tea.MouseButtonWheelDown:
		return v.move(3)
	}
	return nil
}

func (v *workspaceView) close() {
	if v.cancel != nil {
		v.cancel()
		v.cancel = nil
	}
}

func (m *tuiModel) closeFullscreen() {
	hadFullscreen := m.full != nil
	if closer, ok := m.full.(interface{ close() }); ok {
		closer.close()
	}
	m.full = nil
	if hadFullscreen {
		// A closed newer surface must continue to invalidate any older async
		// fullscreen result; nil alone cannot distinguish "never opened" from
		// "opened and closed before the result arrived."
		m.workspaceGeneration++
	}
}

func (v *workspaceView) clearPreview() {
	v.previewRequest++
	v.previewPath = ""
	v.preview = workspace.Document{}
	v.previewErr = nil
	v.previewStale = false
	v.source = false
}

func (v *workspaceView) clampSelection() {
	if len(v.rows) == 0 {
		v.selected = 0
		return
	}
	v.selected = workspaceClamp(v.selected, 0, len(v.rows)-1)
}

func (v *workspaceView) activeLocation() (workspace.Location, bool) {
	if len(v.rows) == 0 {
		return workspace.Location{}, false
	}
	v.clampSelection()
	location := v.rows[v.selected].location
	line, column := v.rows[v.selected].line(), v.rows[v.selected].column()
	if v.source {
		line, column = max(v.sourceCursor, 1), 1
	}
	location.Range.Start = workspace.Position{Line: line, Column: column}
	return location, true
}

func (v *workspaceView) view(width, height int, th *theme) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	if v.source {
		return v.sourceView(width, height, th)
	}
	header := v.header(width, th)
	footer := v.footer(width, th)
	if height == 1 {
		return header
	}
	if height == 2 {
		return header + "\n" + footer
	}
	bodyHeight := height - 2
	v.page = bodyHeight

	var body string
	if width >= workspaceSplitWidth {
		leftWidth := workspaceClamp(width/3, 30, 52)
		rightWidth := max(width-leftWidth-1, 1)
		left := v.listView(leftWidth, bodyHeight, th, false)
		right := v.previewView(rightWidth, bodyHeight, th)
		separator := strings.Repeat("│\n", max(bodyHeight-1, 0)) + "│"
		body = lipgloss.JoinHorizontal(lipgloss.Top, left, th.border.Render(separator), right)
	} else {
		body = v.listView(width, bodyHeight, th, true)
	}
	return header + "\n" + body + "\n" + footer
}

func (v *workspaceView) header(width int, th *theme) string {
	title := " files"
	if v.kind == workspaceSearch {
		title = " search “" + workspaceSanitize(v.literal) + "”"
	}
	filter := ""
	if v.filter != "" || v.filterEditing {
		filter = "  filter: " + workspaceSanitize(v.filter)
	}
	if v.filterEditing {
		filter += "█"
	}
	title = workspaceFit(title, width)
	remaining := max(width-lipgloss.Width(title), 0)
	return th.bold.Render(title) + th.dim.Render(workspaceFit(filter, remaining))
}

func (v *workspaceView) footer(width int, th *theme) string {
	status := ""
	switch {
	case v.stale:
		status = "stale: workspace changed; close and reopen this panel"
	case v.err != nil:
		status = "error: " + workspaceSanitize(v.err.Error())
	case v.loading != "":
		status = v.loading
	case v.truncated || v.skipped > 0 || v.oversized > 0:
		parts := []string{fmt.Sprintf("%d results · partial", len(v.rows))}
		if v.truncated {
			parts = append(parts, "truncated")
		}
		if v.oversized > 0 {
			parts = append(parts, fmt.Sprintf("%d oversized", v.oversized))
		}
		if v.skipped > 0 {
			parts = append(parts, fmt.Sprintf("%d skipped", v.skipped))
		}
		status = strings.Join(parts, " · ")
	default:
		status = fmt.Sprintf("%d results", len(v.rows))
	}
	keys := "  / filter · enter source · e edit · c copy · q close"
	return th.faint.Render(workspaceFit(" "+status+keys, width))
}

func (v *workspaceView) listView(width, height int, th *theme, withPreview bool) string {
	if v.err != nil {
		return workspaceFill([]string{th.err.Render(workspaceFit(" "+workspaceSanitize(v.err.Error()), width))}, width, height)
	}
	if v.loading != "" && len(v.rows) == 0 {
		return workspaceFill([]string{th.dim.Render(workspaceFit(" "+v.loading, width))}, width, height)
	}
	if len(v.rows) == 0 {
		empty := " no files match this filter"
		if v.kind == workspaceSearch {
			empty = " no text matches"
			if v.truncated || v.skipped > 0 || v.oversized > 0 {
				empty = " no text matches in searched files · partial results"
			}
		}
		return workspaceFill([]string{th.dim.Render(workspaceFit(empty, width))}, width, height)
	}
	v.clampSelection()
	start := v.selected - height/2
	start = workspaceClamp(start, 0, max(len(v.rows)-height, 0))
	end := min(start+height, len(v.rows))
	lines := make([]string, 0, height)
	for index := start; index < end; index++ {
		row := v.rows[index]
		text := row.label()
		if withPreview && row.preview != "" {
			text += "  " + row.preview
		}
		prefix := "  "
		style := th.text
		if index == v.selected {
			prefix = "▌ "
			style = th.selected
		}
		line := workspaceFit(prefix+workspaceSanitize(text), width)
		lines = append(lines, style.Render(workspacePad(line, width)))
	}
	return workspaceFill(lines, width, height)
}

func (v *workspaceView) previewView(width, height int, th *theme) string {
	if height <= 0 {
		return ""
	}
	if len(v.rows) == 0 {
		return workspaceFill(nil, width, height)
	}
	row := v.rows[v.selected]
	if v.previewPath != row.location.Path || (v.preview.Location.Path == "" && v.previewErr == nil) {
		return workspaceFill([]string{th.dim.Render(workspaceFit(" loading source…", width))}, width, height)
	}
	if v.previewErr != nil {
		return workspaceFill([]string{th.warn.Render(workspaceFit(" "+workspaceSanitize(v.previewErr.Error()), width))}, width, height)
	}
	header := []string{
		th.bold.Render(workspaceFit(" "+workspaceSanitize(row.label()), width)),
		th.faint.Render(workspaceFit(" sha256:"+v.preview.Location.Revision.SHA256, width)),
	}
	if v.previewStale {
		header = append(header, th.warn.Render(workspaceFit(" stale: file changed since these results", width)))
	}
	if len(header) >= height {
		return lipgloss.JoinVertical(lipgloss.Left, header[:height]...)
	}
	bodyHeight := max(height-len(header), 0)
	body := v.renderSource(width, bodyHeight, row.line(), false, th)
	if body == "" {
		return lipgloss.JoinVertical(lipgloss.Left, header...)
	}
	return lipgloss.JoinVertical(lipgloss.Left, append(header, strings.Split(body, "\n")...)...)
}

func (v *workspaceView) sourceView(width, height int, th *theme) string {
	if v.previewErr != nil || v.preview.Location.Path == "" {
		v.source = false
		return v.view(width, height, th)
	}
	header := []string{
		th.bold.Render(workspaceFit(fmt.Sprintf(" %s:%d", workspaceSanitize(v.preview.Location.Path), max(v.sourceCursor, 1)), width)),
		th.faint.Render(workspaceFit(" revision sha256:"+v.preview.Location.Revision.SHA256, width)),
	}
	if v.previewStale {
		header = append(header, th.warn.Render(workspaceFit(" stale: reopen before editing", width)))
	}
	if len(header) >= height {
		return lipgloss.JoinVertical(lipgloss.Left, header[:height]...)
	}
	bodyHeight := max(height-len(header)-1, 0)
	v.page = bodyHeight
	footer := th.faint.Render(workspaceFit(" enter list · ↑↓/j/k move · e edit · c copy path:line · q close", width))
	if bodyHeight == 0 {
		return lipgloss.JoinVertical(lipgloss.Left, append(header, footer)...)
	}
	body := v.renderSource(width, bodyHeight, v.sourceCursor, true, th)
	return lipgloss.JoinVertical(lipgloss.Left, append(header, body, footer)...)
}

func (v *workspaceView) renderSource(width, height, focus int, cursor bool, th *theme) string {
	lines := workspaceSourceLines(v.preview.Content)
	if len(lines) == 0 {
		lines = []string{""}
	}
	focus = workspaceClamp(focus, 1, len(lines))
	start := workspaceClamp(focus-1-height/2, 0, max(len(lines)-height, 0))
	end := min(start+height, len(lines))
	digits := len(fmt.Sprint(len(lines)))
	rendered := make([]string, 0, height)
	for index := start; index < end; index++ {
		line := fmt.Sprintf(" %*d │ %s", digits, index+1, workspaceSanitize(lines[index]))
		line = workspacePad(workspaceFit(line, width), width)
		if cursor && index+1 == focus {
			rendered = append(rendered, th.selected.Render(line))
		} else {
			rendered = append(rendered, th.text.Render(line))
		}
	}
	return workspaceFill(rendered, width, height)
}

func workspaceSourceLines(content []byte) []string {
	lines := strings.Split(string(content), "\n")
	for index := range lines {
		lines[index] = strings.TrimSuffix(lines[index], "\r")
	}
	return lines
}

func workspaceFill(lines []string, width, height int) string {
	for len(lines) < height {
		lines = append(lines, strings.Repeat(" ", max(width, 0)))
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

func workspaceFit(text string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(text) <= width {
		return text
	}
	// Source paths and symbols routinely contain combining sequences and emoji.
	// Rune slicing can split either even while remaining valid UTF-8; ANSI's
	// cell-aware truncator preserves the terminal grapheme as one unit.
	return ansi.Truncate(text, width, "…")
}

func workspacePad(text string, width int) string {
	missing := width - lipgloss.Width(text)
	if missing <= 0 {
		return text
	}
	return text + strings.Repeat(" ", missing)
}

func workspaceSanitize(text string) string {
	// A tab's terminal width depends on the viewer's tab stops; source uses a
	// stable single-cell spelling. Everything else follows the shared terminal
	// boundary, including visible escapes for bidi overrides that could make a
	// repository path or source line appear in a different order.
	return terminaltext.Escape(strings.ReplaceAll(text, "\t", " "))
}

func workspaceClamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

var workspaceClipboardWrite = writeClipboardAll

var workspaceGetenv = os.Getenv

func workspaceEditorArgv(path string, position workspace.Position) ([]string, error) {
	editor := strings.TrimSpace(workspaceGetenv("VISUAL"))
	if editor == "" {
		editor = strings.TrimSpace(workspaceGetenv("EDITOR"))
	}
	if editor == "" {
		return nil, errors.New("set $VISUAL or $EDITOR to edit source")
	}
	parts, err := workspaceSplitArgv(editor)
	if err != nil {
		return nil, err
	}
	line, column := max(position.Line, 1), max(position.Column, 1)
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(parts[0])), ".exe")
	switch name {
	case "code", "code-insiders", "codium", "cursor", "windsurf":
		parts = append(parts, "--goto", fmt.Sprintf("%s:%d:%d", path, line, column))
	case "vi", "vim", "nvim", "view", "gvim", "mvim":
		parts = append(parts, fmt.Sprintf("+%d", line), path)
	case "emacs", "emacsclient":
		parts = append(parts, fmt.Sprintf("+%d:%d", line, column), path)
	case "nano", "pico":
		parts = append(parts, fmt.Sprintf("+%d:%d", line, column), path)
	case "idea", "goland", "pycharm", "webstorm", "zed", "zeditor":
		parts = append(parts, "--line", fmt.Sprint(line), path)
	default:
		parts = append(parts, path)
	}
	return parts, nil
}

// workspaceSplitArgv accepts ordinary quoted editor commands without invoking
// a shell. It deliberately performs no variable, glob, command, or tilde
// expansion: the resulting strings are passed straight to sanitizedCommand.
func workspaceSplitArgv(command string) ([]string, error) {
	var (
		argv  []string
		word  strings.Builder
		quote rune
	)
	runes := []rune(strings.TrimSpace(command))
	flush := func() {
		if word.Len() > 0 {
			argv = append(argv, word.String())
			word.Reset()
		}
	}
	for index := 0; index < len(runes); index++ {
		r := runes[index]
		if quote != 0 {
			if r == quote {
				quote = 0
				continue
			}
			if r == '\\' && index+1 < len(runes) && (runes[index+1] == quote || runes[index+1] == '\\') {
				index++
				word.WriteRune(runes[index])
				continue
			}
			word.WriteRune(r)
			continue
		}
		switch {
		case r == '\'' || r == '"':
			quote = r
		case unicode.IsSpace(r):
			flush()
		case r == '\\' && index+1 < len(runes) && (unicode.IsSpace(runes[index+1]) || runes[index+1] == '\'' || runes[index+1] == '"' || runes[index+1] == '\\'):
			index++
			word.WriteRune(runes[index])
		default:
			word.WriteRune(r)
		}
	}
	if quote != 0 {
		return nil, errors.New("the configured editor command has an unmatched quote")
	}
	flush()
	if len(argv) == 0 {
		return nil, errors.New("the configured editor command is empty")
	}
	return argv, nil
}
