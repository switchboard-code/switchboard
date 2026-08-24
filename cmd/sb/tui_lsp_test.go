package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/switchboard-code/switchboard/internal/lsp"
)

type fakeLSPRuntime struct {
	status       lsp.ServerStatus
	store        *lsp.ProblemStore
	document     func(context.Context, string, int) ([]lsp.Symbol, bool, error)
	workspace    func(context.Context, string, int) ([]lsp.Symbol, bool, error)
	definition   func(context.Context, string, int, string) ([]lsp.Location, error)
	references   func(context.Context, string, int, string) ([]lsp.Location, error)
	statusCalls  atomic.Int32
	queryCalls   atomic.Int32
	problemCalls atomic.Int32
}

func (f *fakeLSPRuntime) Status() lsp.ServerStatus {
	f.statusCalls.Add(1)
	return f.status
}

func (f *fakeLSPRuntime) Problems() *lsp.ProblemStore {
	f.problemCalls.Add(1)
	return f.store
}

func (f *fakeLSPRuntime) DocumentSymbols(ctx context.Context, path string, limit int) ([]lsp.Symbol, bool, error) {
	f.queryCalls.Add(1)
	if f.document == nil {
		return nil, false, nil
	}
	return f.document(ctx, path, limit)
}

func (f *fakeLSPRuntime) WorkspaceSymbols(ctx context.Context, query string, limit int) ([]lsp.Symbol, bool, error) {
	f.queryCalls.Add(1)
	if f.workspace == nil {
		return nil, false, nil
	}
	return f.workspace(ctx, query, limit)
}

func (f *fakeLSPRuntime) DefinitionAtSymbol(ctx context.Context, path string, line int, symbol string) ([]lsp.Location, error) {
	f.queryCalls.Add(1)
	if f.definition == nil {
		return nil, nil
	}
	return f.definition(ctx, path, line, symbol)
}

func (f *fakeLSPRuntime) ReferencesAtSymbol(ctx context.Context, path string, line int, symbol string) ([]lsp.Location, error) {
	f.queryCalls.Add(1)
	if f.references == nil {
		return nil, nil
	}
	return f.references(ctx, path, line, symbol)
}

func (f *fakeLSPRuntime) Close() {}

func lspTestModel(t *testing.T) (*tuiModel, *fakeLSPRuntime, string) {
	t.Helper()
	m := testModel(t)
	root := m.app.loop.Tools.Root()
	m.app.workspace = root
	m.workspaceRuntime = newWorkspaceRuntime(root)
	fake := &fakeLSPRuntime{
		status: lsp.ServerStatus{State: lsp.ServerConfigured, Executable: "fixture-ls"},
		store:  lsp.NewProblemStore(root),
	}
	m.app.lsp = fake
	return m, fake, root
}

func TestOptionalLSPRuntimeDoesNotBoxATypedNil(t *testing.T) {
	var server *lsp.Server
	if runtime := optionalLSPRuntime(server); runtime != nil {
		t.Fatalf("optional runtime = %#v, want a nil interface", runtime)
	}
	server = &lsp.Server{Root: t.TempDir()}
	if runtime := optionalLSPRuntime(server); runtime == nil {
		t.Fatal("non-nil server disappeared at the TUI boundary")
	}
}

func activeLSPView(t *testing.T, m *tuiModel) *lspView {
	t.Helper()
	view, ok := m.full.(*lspView)
	if !ok {
		t.Fatalf("fullscreen = %T, want lspView", m.full)
	}
	return view
}

func TestLSPStatusIsNonStartingAndMissingGuidanceIsSpecific(t *testing.T) {
	m, fake, _ := lspTestModel(t)
	if cmd := cmdLSP(m, ""); cmd != nil {
		t.Fatal("/lsp unexpectedly returned asynchronous work")
	}
	if fake.statusCalls.Load() != 1 || fake.queryCalls.Load() != 0 || fake.problemCalls.Load() != 0 {
		t.Fatalf("status=%d queries=%d problems=%d; /lsp must inspect only Status", fake.statusCalls.Load(), fake.queryCalls.Load(), fake.problemCalls.Load())
	}
	transcript := strings.Join(m.tr.flat, "\n")
	if !strings.Contains(transcript, "lazy; no process is started by /lsp") || !strings.Contains(transcript, "does not prove") {
		t.Fatalf("/lsp status omitted lazy/coverage truth:\n%s", transcript)
	}

	m.app.lsp = nil
	m.app.lspNote = "fixture-ls is available; /trust grant enables semantic navigation"
	cmdLSP(m, "")
	if transcript = strings.Join(m.tr.flat, "\n"); !strings.Contains(transcript, "/trust grant") {
		t.Fatalf("missing server guidance omitted trust gate:\n%s", transcript)
	}
	cmd := cmdOutline(m, "main.go")
	if cmd == nil {
		t.Fatal("missing-server outline did not return guidance")
	}
	if msg, ok := cmd().(noticeMsg); !ok || !strings.Contains(msg.text, "/trust grant") {
		t.Fatalf("missing-server outline = %#v", msg)
	}
}

func TestLSPOutlineIsAsyncBoundedNavigableAndSuppressesUTF16Columns(t *testing.T) {
	m, fake, root := lspTestModel(t)
	inside := filepath.Join(root, "main.go")
	if err := os.WriteFile(inside, []byte("package main\n😀func Target() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external.go")
	if err := os.WriteFile(external, []byte("package external\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake.document = func(context.Context, string, int) ([]lsp.Symbol, bool, error) {
		return []lsp.Symbol{
			{Name: "Target", Kind: lsp.SymbolFunction, Path: inside, SelectionRange: lsp.Range{Start: lsp.Position{Line: 1, Character: 2}}},
			{Name: "External", Kind: lsp.SymbolFunction, Path: external, SelectionRange: lsp.Range{Start: lsp.Position{Line: 3, Character: 9}}},
		}, false, nil
	}

	cmd := cmdOutline(m, "main.go")
	view := activeLSPView(t, m)
	if cmd == nil || view.loading == "" || fake.queryCalls.Load() != 0 {
		t.Fatal("outline did work synchronously or failed to install its loading panel")
	}
	msg, ok := cmd().(lspLoadedMsg)
	if !ok || fake.queryCalls.Load() != 1 {
		t.Fatalf("outline command = %#v, calls=%d", msg, fake.queryCalls.Load())
	}
	_, next := m.Update(msg)
	if next != nil || len(view.rows) != 2 || !view.rows[0].navigable || view.rows[1].navigable {
		t.Fatalf("outline rows = %+v", view.rows)
	}
	plain := stripANSI(view.view(80, 24, darkTheme()))
	if lines := strings.Count(plain, "\n") + 1; lines != 24 {
		t.Fatalf("80x24 LSP view rendered %d lines", lines)
	}
	if strings.Contains(plain, "main.go:2:3") || !strings.Contains(plain, "main.go:2") {
		t.Fatalf("view exposed a UTF-16 column or lost the honest line:\n%s", plain)
	}

	oldGetenv := workspaceGetenv
	workspaceGetenv = func(key string) string {
		if key == "VISUAL" {
			return "code"
		}
		return ""
	}
	t.Cleanup(func() { workspaceGetenv = oldGetenv })
	ready, ok := view.editorCmd()().(lspEditorReadyMsg)
	if !ok || ready.err != nil || len(ready.argv) != 3 || ready.argv[1] != "--goto" || !strings.HasSuffix(ready.argv[2], "main.go:2:1") {
		t.Fatalf("editor ready = %#v", ready)
	}
	_, _ = m.Update(workspaceInvalidatedMsg{})
	_, launch := m.Update(ready)
	if launch == nil {
		t.Fatal("Enter followed by invalidation still reached the editor launch boundary")
	}
	if msg, ok := launch().(noticeMsg); !ok || !strings.Contains(msg.text, "workspace changed before the editor") {
		t.Fatalf("Enter→invalidate→ready = %#v", msg)
	}

	view.selected = 1
	cmd = view.editorCmd()
	if msg, ok := cmd().(noticeMsg); !ok || !strings.Contains(msg.text, "outside the workspace") {
		t.Fatalf("external enter = %#v", msg)
	}
	view.selected = 0
	if msg, ok := view.editorCmd()().(noticeMsg); !ok || !strings.Contains(msg.text, "rerun the semantic command") {
		t.Fatalf("stale result enter = %#v", msg)
	}
}

func TestLSPEditorReturnInvalidatesWorkspaceAndSemanticResults(t *testing.T) {
	m, _, root := lspTestModel(t)
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = cmdOutline(m, "main.go")
	view := activeLSPView(t, m)
	before := m.workspaceRuntime.epoch.Load()
	oldGeneration, oldRequest := view.generation, view.request

	cmd := m.onLSPEditorDone(lspEditorDoneMsg{
		view: view, sessionID: view.sessionID, generation: oldGeneration,
		path: "main.go", err: errors.New("editor exited after saving"),
	})
	if !view.stale || m.workspaceRuntime.epoch.Load() != before+1 {
		t.Fatalf("editor return stale=%v epoch=%d, want stale and %d", view.stale, m.workspaceRuntime.epoch.Load(), before+1)
	}
	if view.generation == oldGeneration || view.request == oldRequest {
		t.Fatalf("editor return kept old semantic binding: generation=%d request=%d", view.generation, view.request)
	}
	if msg, ok := cmd().(noticeMsg); !ok || msg.level != "error" {
		t.Fatalf("failed editor return notice = %#v", msg)
	}
	_, _ = m.Update(lspLoadedMsg{
		view: view, sessionID: view.sessionID, generation: oldGeneration, request: oldRequest,
		rows: []lspRow{{id: "pre-editor", label: "pre-editor"}},
	})
	for _, row := range view.rows {
		if row.id == "pre-editor" {
			t.Fatal("a pre-editor semantic result repainted the panel after invalidation")
		}
	}
}

func TestLSPUnsupportedCapabilityIsVisible(t *testing.T) {
	m, fake, root := lspTestModel(t)
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake.document = func(context.Context, string, int) ([]lsp.Symbol, bool, error) {
		return nil, false, errors.New("fixture-ls does not advertise document symbols support")
	}
	cmd := cmdOutline(m, "main.go")
	_, _ = m.Update(cmd())
	view := activeLSPView(t, m)
	if view.err == nil || !strings.Contains(view.err.Error(), "unsupported by this language server") {
		t.Fatalf("unsupported error = %v", view.err)
	}
	if rendered := stripANSI(view.view(80, 8, darkTheme())); !strings.Contains(rendered, "does not advertise") {
		t.Fatalf("unsupported capability is not visible:\n%s", rendered)
	}
}

func TestLSPLateResultCannotCrossCloseReopenOrSession(t *testing.T) {
	m, fake, root := lspTestModel(t)
	path := filepath.Join(root, "main.go")
	fake.workspace = func(context.Context, string, int) ([]lsp.Symbol, bool, error) {
		return []lsp.Symbol{{Name: "Late", Kind: lsp.SymbolFunction, Path: path}}, false, nil
	}

	oldCmd := cmdSymbols(m, "Late")
	oldView := activeLSPView(t, m)
	oldMsg := oldCmd().(lspLoadedMsg)
	m.closeFullscreen()
	_, _ = m.Update(oldMsg)
	if len(oldView.rows) != 0 || m.full != nil {
		t.Fatal("closed LSP view accepted a late result")
	}

	_ = cmdSymbols(m, "Current")
	current := activeLSPView(t, m)
	_, _ = m.Update(oldMsg)
	if len(current.rows) != 0 {
		t.Fatal("prior panel generation installed into its replacement")
	}

	currentCmd := current.loadCmd()
	currentMsg := currentCmd().(lspLoadedMsg)
	replacement, err := m.app.store.Create(root, m.app.loop.Binding().Target.ID(), "replacement")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { replacement.Close() })
	m.app.loop.Session = replacement
	_, _ = m.Update(currentMsg)
	if len(current.rows) != 0 {
		t.Fatal("old-session LSP result installed after the session changed")
	}
}

func TestLSPClosingPanelCancelsRequest(t *testing.T) {
	m, fake, root := lspTestModel(t)
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake.document = func(ctx context.Context, _ string, _ int) ([]lsp.Symbol, bool, error) {
		<-ctx.Done()
		return nil, false, ctx.Err()
	}
	cmd := cmdOutline(m, "main.go")
	view := activeLSPView(t, m)
	m.closeFullscreen()
	msg := cmd().(lspLoadedMsg)
	if !errors.Is(msg.err, context.Canceled) || view.cancel != nil {
		t.Fatalf("closed request = %#v, cancel retained=%v", msg, view.cancel != nil)
	}
	_, _ = m.Update(msg)
	if m.full != nil || len(view.rows) != 0 {
		t.Fatal("cancelled request mutated the closed panel")
	}
}

func TestLSPProblemsPreserveSelectionAndNeverClaimSilenceMeansClean(t *testing.T) {
	m, _, root := lspTestModel(t)
	path := filepath.Join(root, "main.go")
	cmd := cmdProblems(m, "")
	if cmd == nil {
		t.Fatal("problems did not load asynchronously")
	}
	view := activeLSPView(t, m)
	uri := "file://" + filepath.ToSlash(path)
	problem := func(line int, message string) lsp.Problem {
		return lsp.Problem{URI: uri, Path: path, Navigable: true, Line: line, Column: 3, EndLine: line,
			EndColumn: 5, Severity: lsp.SeverityWarning, Source: "fixture", Message: message}
	}
	snapshot := func(problems ...lsp.Problem) lsp.ProblemSnapshot {
		return lsp.ProblemSnapshot{Available: true, Total: len(problems), Documents: []lsp.DocumentProblems{{
			URI: uri, Path: path, Navigable: true, Freshness: lsp.Stale, Problems: problems,
		}}}
	}
	longMessage := strings.Repeat("long diagnostic text ", 20)
	first := snapshot(problem(4, longMessage), problem(9, "keep selected"))
	_, _ = m.Update(lspLoadedMsg{
		view: view, sessionID: view.sessionID, generation: view.generation, request: view.request,
		problems: first, rows: lspProblemRows(view.resolver, first),
	})
	view.selected = 1
	selectedID := view.rows[1].id
	second := snapshot(problem(2, "inserted"), problem(4, longMessage), problem(9, "keep selected"))
	view.request++
	_, _ = m.Update(lspLoadedMsg{
		view: view, sessionID: view.sessionID, generation: view.generation, request: view.request,
		problems: second, rows: lspProblemRows(view.resolver, second),
	})
	if view.rows[view.selected].id != selectedID || !strings.Contains(view.rows[view.selected].label, "keep selected") {
		t.Fatalf("diagnostic selection moved: selected=%d rows=%+v", view.selected, view.rows)
	}
	rendered := stripANSI(view.view(80, 24, darkTheme()))
	for _, want := range []string{"partial coverage", "silence ≠ clean", "[stale]"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("problems view missing %q:\n%s", want, rendered)
		}
	}
	_, _ = m.Update(workspaceInvalidatedMsg{})
	if changed := stripANSI(view.view(80, 24, darkTheme())); !strings.Contains(changed, "workspace changed") {
		t.Fatalf("tool edit did not mark semantic results stale:\n%s", changed)
	}

	view.replaceRows(nil)
	view.problems = lsp.ProblemSnapshot{Available: true}
	empty := stripANSI(view.view(80, 8, darkTheme()))
	if !strings.Contains(empty, "not proof") || !strings.Contains(empty, "partial") {
		t.Fatalf("empty diagnostics implied a clean workspace:\n%s", empty)
	}
}

func TestLSPProblemsSnapshotAndSubscriptionDoNotStartServer(t *testing.T) {
	m, fake, _ := lspTestModel(t)
	cmd := cmdProblems(m, "")
	view := activeLSPView(t, m)
	_, _ = m.Update(cmd())
	if fake.queryCalls.Load() != 0 || fake.statusCalls.Load() != 1 || fake.problemCalls.Load() != 1 {
		t.Fatalf("problems status=%d snapshot=%d semantic=%d", fake.statusCalls.Load(), fake.problemCalls.Load(), fake.queryCalls.Load())
	}
	if rendered := stripANSI(view.view(100, 8, darkTheme())); !strings.Contains(rendered, "server not started") || !strings.Contains(rendered, "partial coverage") {
		t.Fatalf("configured diagnostics coverage is misleading:\n%s", rendered)
	}

	source := make(chan uint64)
	m.app.lspProblems = source
	before := view.request
	if rearm := m.onLSPProblemsChanged(lspProblemsChangedMsg{server: fake, source: source, generation: 3, open: true}); rearm == nil {
		t.Fatal("live diagnostics notification did not re-arm the coalesced subscription")
	}
	if view.request != before+1 || view.loading == "" {
		t.Fatal("live diagnostics notification did not schedule an async snapshot refresh")
	}
	if cmd := m.onLSPProblemsChanged(lspProblemsChangedMsg{server: fake, source: source, open: false}); cmd != nil {
		t.Fatal("closed diagnostics channel was re-armed")
	}
	closed := make(chan uint64)
	close(closed)
	closedMsg, ok := waitLSPProblems(fake, closed)().(lspProblemsChangedMsg)
	if !ok || closedMsg.open {
		t.Fatalf("closed subscription wait = %#v", closedMsg)
	}
}

func TestParseLSPLocationKeepsSpacesAndWindowsDrive(t *testing.T) {
	for _, test := range []struct {
		input      string
		path, name string
		line       int
	}{
		{"dir/a file.go:12 Thing", "dir/a file.go", "Thing", 12},
		{`C:\\work\\main.go:7 Target`, `C:\\work\\main.go`, "Target", 7},
	} {
		path, line, name, err := parseLSPLocation(test.input)
		if err != nil || path != test.path || line != test.line || name != test.name {
			t.Errorf("parseLSPLocation(%q) = %q, %d, %q, %v", test.input, path, line, name, err)
		}
	}
}
