package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/switchboard-code/switchboard/internal/workspace"
)

func TestWorkspaceFitPreservesGraphemeClusters(t *testing.T) {
	got := workspaceFit("x🧑🏽‍💻suffix", 4)
	if ansi.StringWidth(got) > 4 {
		t.Fatalf("fitted workspace label is %d cells: %q", ansi.StringWidth(got), got)
	}
	if got != "x🧑🏽‍💻…" {
		t.Fatalf("workspace fit split a grapheme: %q", got)
	}
}

func TestWorkspaceLoadingRowsFitTinyWidths(t *testing.T) {
	for _, width := range []int{1, 2, 10} {
		v := &workspaceView{loading: "loading workspace source"}
		assertTUIViewBounds(t, v.listView(width, 2, darkTheme(), false), width, 2)

		v = &workspaceView{rows: []workspaceRow{{location: workspace.Location{Path: "main.go"}}}}
		assertTUIViewBounds(t, v.previewView(width, 2, darkTheme()), width, 2)
	}
}

func TestWorkspaceSanitizeExposesTerminalAndBidiControls(t *testing.T) {
	got := workspaceSanitize("safe\x1b]2;forged\a\u202e.go\nnext")
	for _, unsafe := range []string{"\x1b", "\a", "\u202e", "\n"} {
		if strings.Contains(got, unsafe) {
			t.Fatalf("workspace text retained control %q: %q", unsafe, got)
		}
	}
	for _, visible := range []string{`\x1b`, `\x07`, `\u202e`, `\x0a`} {
		if !strings.Contains(got, visible) {
			t.Fatalf("workspace text hid %q: %q", visible, got)
		}
	}
}

func workspaceModel(t *testing.T, root string) *tuiModel {
	t.Helper()
	m := testModel(t)
	m.app.workspace = root
	m.workspaceRuntime = newWorkspaceRuntime(root)
	return m
}

func drainWorkspace(t *testing.T, m *tuiModel, cmd tea.Cmd) {
	t.Helper()
	for steps := 0; cmd != nil && steps < 10; steps++ {
		msg := cmd()
		_, cmd = m.Update(msg)
	}
	if cmd != nil {
		t.Fatal("workspace command did not settle")
	}
}

func activeWorkspaceView(t *testing.T, m *tuiModel) *workspaceView {
	t.Helper()
	view, ok := m.full.(*workspaceView)
	if !ok {
		t.Fatalf("fullscreen = %T, want workspace view", m.full)
	}
	return view
}

func TestWorkspaceFilesNonGitSourceLens(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "alpha.go"), []byte("package alpha\nfunc Answer() int { return 42 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := workspaceModel(t, root)
	drainWorkspace(t, m, cmdFiles(m, "alpha"))
	v := activeWorkspaceView(t, m)
	if len(v.rows) != 1 || v.rows[0].location.Path != "alpha.go" {
		t.Fatalf("rows = %+v, want alpha.go", v.rows)
	}
	if len(v.preview.Location.Revision.SHA256) != 64 {
		t.Fatalf("revision = %q, want full sha256", v.preview.Location.Revision.SHA256)
	}
	wide := stripANSI(v.view(110, 18, darkTheme()))
	for _, want := range []string{"alpha.go", "sha256:" + v.preview.Location.Revision.SHA256, "1 │ package alpha"} {
		if !strings.Contains(wide, want) {
			t.Errorf("source lens missing %q:\n%s", want, wide)
		}
	}

	if close, cmd := v.key(tea.KeyMsg{Type: tea.KeyEnter}); close || cmd != nil || !v.source {
		t.Fatalf("enter = (close %v, cmd %v, source %v), want source mode", close, cmd != nil, v.source)
	}
	_, _ = v.key(tea.KeyMsg{Type: tea.KeyDown})
	source := stripANSI(v.view(80, 12, darkTheme()))
	if !strings.Contains(source, "alpha.go:2") || !strings.Contains(source, "2 │ func Answer") {
		t.Fatalf("source cursor did not retain exact line numbers:\n%s", source)
	}
	_, _ = v.key(tea.KeyMsg{Type: tea.KeyEnter})
	if v.source {
		t.Fatal("enter did not return from source to the list")
	}
}

func TestWorkspaceLargeSnapshotFiltersOutsideUpdate(t *testing.T) {
	files := make([]workspace.File, 100_000)
	for index := range files {
		files[index] = workspace.File{Path: fmt.Sprintf("src/file-%06d.go", index)}
	}
	files[78_901] = workspace.File{Path: "src/needle.go"}
	v := &workspaceView{snapshot: workspace.Snapshot{Generation: 1, Files: files}, filter: "needle"}
	cmd := v.filterCmd()
	if cmd == nil || len(v.rows) != 0 {
		t.Fatal("filter scanned or installed rows synchronously")
	}
	deferred, ok := cmd().(workspaceFilteredMsg)
	if !ok || !deferred.deferred || len(deferred.matches) != 0 {
		t.Fatalf("filter debounce result = %#v", deferred)
	}
	msg, ok := v.runFilterCmd(deferred)().(workspaceFilteredMsg)
	if !ok || len(msg.matches) != 1 || msg.matches[0].File.Path != "src/needle.go" {
		t.Fatalf("async filter result = %#v", msg)
	}

	v.filterEditing = true
	before := v.filterRequest
	cmd = v.filterKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if cmd == nil || v.filterRequest != before+1 {
		t.Fatal("typing did not enqueue a new, generation-tagged filter")
	}
}

func TestWorkspaceLiteralSearchAndStaleExternalEdit(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte("first\nTarget here\nthird\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := workspaceModel(t, root)
	cmd := cmdWorkspaceSearch(m, "Target")
	opened, ok := cmd().(workspaceOpenedMsg)
	if !ok || opened.err != nil || len(opened.matches) != 1 {
		t.Fatalf("search result = %#v", opened)
	}
	match := opened.matches[0].Location
	if match.String() != "main.go:2:1" || len(match.Revision.SHA256) != 64 {
		t.Fatalf("location = %+v, want main.go:2:1 with revision", match)
	}
	if err := os.WriteFile(path, []byte("inserted\nfirst\nTarget here\nthird\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, previewCmd := m.Update(opened)
	preview, ok := previewCmd().(workspacePreviewMsg)
	if !ok || preview.err != nil || !preview.stale {
		t.Fatalf("preview after external edit = %#v, want stale", preview)
	}
	_, _ = m.Update(preview)
	view := stripANSI(activeWorkspaceView(t, m).view(110, 15, darkTheme()))
	if !strings.Contains(view, "stale: file changed") {
		t.Fatalf("stale result was not visible:\n%s", view)
	}
}

func TestWorkspaceSearchQualifiesEmptyResultsWhenFilesWereNotSearched(t *testing.T) {
	root := t.TempDir()
	large := make([]byte, workspace.DefaultSearchBytes+1)
	copy(large, "needle in oversized text")
	if err := os.WriteFile(filepath.Join(root, "large.txt"), large, 0o644); err != nil {
		t.Fatal(err)
	}
	m := workspaceModel(t, root)
	drainWorkspace(t, m, cmdWorkspaceSearch(m, "needle"))
	v := activeWorkspaceView(t, m)
	if len(v.rows) != 0 || v.oversized != 1 {
		t.Fatalf("partial search state = rows %d, truncated=%v skipped=%d oversized=%d", len(v.rows), v.truncated, v.skipped, v.oversized)
	}
	rendered := stripANSI(v.view(110, 8, darkTheme()))
	for _, want := range []string{"no text matches in searched files", "0 results · partial", "1 oversized"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("partial search did not render %q:\n%s", want, rendered)
		}
	}
}

func TestWorkspaceBinaryErrorIsVisible(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "image.bin"), []byte{'a', 0, 'b'}, 0o644); err != nil {
		t.Fatal(err)
	}
	m := workspaceModel(t, root)
	drainWorkspace(t, m, cmdFiles(m, "image"))
	v := activeWorkspaceView(t, m)
	if !errors.Is(v.previewErr, workspace.ErrBinary) {
		t.Fatalf("preview error = %v, want ErrBinary", v.previewErr)
	}
	if rendered := stripANSI(v.view(100, 12, darkTheme())); !strings.Contains(rendered, "binary") {
		t.Fatalf("binary refusal not visible:\n%s", rendered)
	}
}

func TestWorkspaceEachOpenRefreshesExternalFiles(t *testing.T) {
	root := t.TempDir()
	m := workspaceModel(t, root)
	drainWorkspace(t, m, cmdFiles(m, ""))
	if rows := activeWorkspaceView(t, m).rows; len(rows) != 0 {
		t.Fatalf("empty workspace rows = %+v", rows)
	}
	if cmd := m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}); cmd != nil || m.full != nil {
		t.Fatal("q did not safely close the workspace panel")
	}
	if err := os.WriteFile(filepath.Join(root, "external.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	drainWorkspace(t, m, cmdFiles(m, "external"))
	rows := activeWorkspaceView(t, m).rows
	if len(rows) != 1 || rows[0].location.Path != "external.txt" {
		t.Fatalf("reopened rows = %+v, want external file", rows)
	}
}

func TestWorkspaceLateResultsCannotInstallAfterCloseOrReopen(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := workspaceModel(t, root)
	oldCmd := cmdFiles(m, "a")
	oldView := activeWorkspaceView(t, m)
	m.closeFullscreen()
	_, _ = m.Update(oldCmd())
	if m.full != nil || len(oldView.rows) != 0 {
		t.Fatal("closed panel accepted a late refresh")
	}

	newCmd := cmdFiles(m, "a")
	newView := activeWorkspaceView(t, m)
	stale := workspaceOpenedMsg{
		view: oldView, sessionID: oldView.sessionID, generation: oldView.generation,
		snapshot: workspace.Snapshot{Generation: 99, Files: []workspace.File{{Path: "wrong"}}},
	}
	_, _ = m.Update(stale)
	if newView.snapshot.Generation != 0 {
		t.Fatal("a prior panel generation installed into its replacement")
	}
	drainWorkspace(t, m, newCmd)
	if len(newView.rows) != 1 || newView.rows[0].location.Path != "a.txt" {
		t.Fatalf("current generation did not install: %+v", newView.rows)
	}
}

func TestWorkspaceLateResultCannotCrossSessionSwap(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := workspaceModel(t, root)
	cmd := cmdFiles(m, "a")
	v := activeWorkspaceView(t, m)
	opened := cmd().(workspaceOpenedMsg)
	replacement, err := m.app.store.Create(root, m.app.loop.Binding().Target.ID(), "replacement")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { replacement.Close() })
	m.app.loop.Session = replacement
	_, _ = m.Update(opened)
	if v.snapshot.Generation != 0 || len(v.rows) != 0 {
		t.Fatal("old-session workspace result installed after the session changed")
	}
}

func TestWorkspaceNarrowEmptyErrorAndTruncationRendering(t *testing.T) {
	v := &workspaceView{kind: workspaceFiles, rows: nil, truncated: true}
	for _, size := range [][2]int{{1, 1}, {3, 2}, {20, 3}, {80, 5}} {
		got := v.view(size[0], size[1], darkTheme())
		if lines := strings.Count(got, "\n") + 1; lines > size[1] {
			t.Errorf("view %dx%d rendered %d lines", size[0], size[1], lines)
		}
	}
	if got := stripANSI(v.view(80, 5, darkTheme())); !strings.Contains(got, "truncated") {
		t.Fatalf("truncation not rendered:\n%s", got)
	}

	m := workspaceModel(t, filepath.Join(t.TempDir(), "missing"))
	drainWorkspace(t, m, cmdFiles(m, ""))
	view := activeWorkspaceView(t, m)
	if view.err == nil || !strings.Contains(view.err.Error(), "no such file") || !strings.Contains(stripANSI(view.view(60, 6, darkTheme())), "error:") {
		t.Fatalf("workspace error = %v, view=%q", view.err, stripANSI(view.view(60, 6, darkTheme())))
	}
}

func TestWorkspaceNavigationCopyAndEditorArgv(t *testing.T) {
	v := &workspaceView{
		rows: []workspaceRow{
			{location: workspace.Location{Path: "a.go", Range: workspace.Range{Start: workspace.Position{Line: 4, Column: 2}}}},
			{location: workspace.Location{Path: "b.go", Range: workspace.Range{Start: workspace.Position{Line: 9, Column: 1}}}},
		},
		page: 5,
	}
	_, _ = v.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("G")})
	if v.selected != 1 {
		t.Fatalf("G selected %d, want last row", v.selected)
	}
	_, _ = v.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	if v.selected != 0 {
		t.Fatalf("g selected %d, want first row", v.selected)
	}

	oldWrite := workspaceClipboardWrite
	defer func() { workspaceClipboardWrite = oldWrite }()
	copied := ""
	workspaceClipboardWrite = func(text string) error { copied = text; return nil }
	msg := v.copyCmd()().(workspaceCopiedMsg)
	if msg.err != nil || copied != "a.go:4" {
		t.Fatalf("copied %q (%v), want a.go:4", copied, msg.err)
	}

	oldGetenv := workspaceGetenv
	defer func() { workspaceGetenv = oldGetenv }()
	workspaceGetenv = func(key string) string {
		if key == "VISUAL" {
			return `"/Applications/Visual Studio Code.app/Contents/MacOS/code" --wait`
		}
		return ""
	}
	argv, err := workspaceEditorArgv(filepath.Join("tmp", "space name.go"), workspace.Position{Line: 8, Column: 3})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/Applications/Visual Studio Code.app/Contents/MacOS/code", "--wait", "--goto", filepath.Join("tmp", "space name.go") + ":8:3"}
	if fmt.Sprint(argv) != fmt.Sprint(want) {
		t.Fatalf("editor argv = %q, want %q", argv, want)
	}
	if strings.Contains(strings.Join(argv, " "), "sh -c") {
		t.Fatal("source editor was routed through a shell")
	}

	for name, wantTail := range map[string][]string{
		"nvim":         {"+8", filepath.Join("tmp", "space name.go")},
		"emacsclient":  {"+8:3", filepath.Join("tmp", "space name.go")},
		"nano":         {"+8:3", filepath.Join("tmp", "space name.go")},
		"unknown-edit": {filepath.Join("tmp", "space name.go")},
	} {
		workspaceGetenv = func(key string) string {
			if key == "VISUAL" {
				return name
			}
			return ""
		}
		got, err := workspaceEditorArgv(filepath.Join("tmp", "space name.go"), workspace.Position{Line: 8, Column: 3})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if fmt.Sprint(got[1:]) != fmt.Sprint(wantTail) {
			t.Errorf("%s argv tail = %q, want %q", name, got[1:], wantTail)
		}
	}
}

func TestWorkspaceWheelDoesNotStrandPreview(t *testing.T) {
	v := &workspaceView{
		rows: []workspaceRow{
			{location: workspace.Location{Path: "a.go"}},
			{location: workspace.Location{Path: "b.go"}},
		},
		previewPath: "a.go",
		preview:     workspace.Document{Location: workspace.Location{Path: "a.go"}, Content: []byte("a\n")},
	}
	cmd := v.mouse(tea.MouseMsg{Button: tea.MouseButtonWheelDown})
	if v.selected != 1 || v.previewPath != "b.go" || cmd == nil {
		t.Fatalf("wheel = selected %d preview %q cmd %v, want b.go with preview command", v.selected, v.previewPath, cmd != nil)
	}
}

func TestWorkspaceFilterIsBounded(t *testing.T) {
	v := &workspaceView{filterEditing: true}
	_, _ = v.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(strings.Repeat("x", workspaceFilterRunes+40))})
	if got := len([]rune(v.filter)); got != workspaceFilterRunes {
		t.Fatalf("filter length = %d, want %d", got, workspaceFilterRunes)
	}
}

func TestWorkspaceEditorRefusesChangedRevision(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := workspaceModel(t, root)
	drainWorkspace(t, m, cmdFiles(m, "main"))
	v := activeWorkspaceView(t, m)
	if err := os.WriteFile(path, []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ready, ok := v.editorCmd()().(workspaceEditorReadyMsg)
	if !ok || !errors.Is(ready.err, workspace.ErrStaleLocation) {
		t.Fatalf("editor readiness = %#v, want stale-location refusal", ready)
	}
}

func TestWorkspaceEditorReturnInvalidatesEvenAfterError(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := workspaceModel(t, root)
	drainWorkspace(t, m, cmdFiles(m, "main.go"))
	v := activeWorkspaceView(t, m)
	oldGeneration := v.generation
	oldPreviewRequest := v.previewRequest
	beforeEpoch := m.workspaceRuntime.epoch.Load()

	cmd := m.onWorkspaceEditorDone(workspaceEditorDoneMsg{
		view: v, sessionID: v.sessionID, generation: oldGeneration,
		path: "main.go", err: errors.New("editor exited after saving"),
	})
	if !v.stale || !v.previewStale {
		t.Fatalf("failed editor return stale=%v previewStale=%v, want both", v.stale, v.previewStale)
	}
	if v.generation == oldGeneration || v.previewRequest == oldPreviewRequest {
		t.Fatalf("failed editor return kept old async binding: generation=%d request=%d", v.generation, v.previewRequest)
	}
	if got := m.workspaceRuntime.epoch.Load(); got != beforeEpoch+1 {
		t.Fatalf("workspace epoch = %d, want %d", got, beforeEpoch+1)
	}
	if msg, ok := cmd().(noticeMsg); !ok || msg.level != "error" {
		t.Fatalf("failed editor return notice = %#v", msg)
	}

	// A preview already in flight when the editor opened cannot repaint the
	// stale panel with the pre-editor document after the process returns.
	beforeContent := string(v.preview.Content)
	_, _ = m.Update(workspacePreviewMsg{
		view: v, sessionID: v.sessionID, generation: oldGeneration,
		request: oldPreviewRequest, expected: workspace.Location{Path: "main.go"},
		document: workspace.Document{Location: workspace.Location{Path: "main.go"}, Content: []byte("old\n")},
	})
	if string(v.preview.Content) != beforeContent {
		t.Fatal("a pre-editor preview repainted the panel after invalidation")
	}
}

func TestWorkspaceInvalidationMessageMarksIndexDirtyAfterBatchBoundary(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := workspaceModel(t, root)
	drainWorkspace(t, m, cmdFiles(m, "main.go"))
	v := activeWorkspaceView(t, m)
	if close, cmd := v.key(tea.KeyMsg{Type: tea.KeyEnter}); close || cmd != nil || !v.source {
		t.Fatalf("enter source = close %v cmd %v source %v", close, cmd != nil, v.source)
	}
	before := m.workspaceRuntime.epoch.Load()
	_, _ = m.Update(workspaceInvalidatedMsg{})
	if after := m.workspaceRuntime.epoch.Load(); after != before+1 {
		t.Fatalf("invalidation epoch = %d, want %d", after, before+1)
	}
	if !v.stale || !v.previewStale {
		t.Fatalf("active source was not visibly invalidated: stale=%v previewStale=%v", v.stale, v.previewStale)
	}
	if view := stripANSI(v.view(90, 12, darkTheme())); !strings.Contains(view, "stale: reopen before editing") {
		t.Fatalf("stale source did not explain how to refresh it:\n%s", view)
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if cmd == nil {
		t.Fatal("stale source silently accepted navigation")
	}
	result := cmd()
	if notice, ok := result.(noticeMsg); !ok || notice.level != "warn" || !strings.Contains(notice.text, "workspace changed") {
		t.Fatalf("stale source action = %#v, want refresh warning", result)
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if m.full != nil {
		t.Fatal("q did not close the stale source panel")
	}
}

func TestWorkspaceInvalidationRejectsPendingPanelLoad(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "late.go"), []byte("package late\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := workspaceModel(t, root)
	cmd := cmdFiles(m, "late")
	v := activeWorkspaceView(t, m)
	_, _ = m.Update(workspaceInvalidatedMsg{})
	if !v.stale {
		t.Fatal("pending workspace panel was not marked stale")
	}
	_, follow := m.Update(cmd())
	if follow != nil || len(v.rows) != 0 {
		t.Fatalf("pre-invalidation load landed after the boundary: follow=%v rows=%+v", follow != nil, v.rows)
	}
}
