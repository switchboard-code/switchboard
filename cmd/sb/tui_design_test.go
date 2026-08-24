package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/trust"
)

// The redesign's invariants, asserted at the SGR level for the same reason
// the markdown tests are: a style field can be dead under a changed
// formatter, and the emitted sequence is what the terminal actually shows.

func TestUserTurnRendersAsCard(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	tr := testTranscript(t, 80)
	tr.add(&entry{kind: kindUser, text: "fix the failing test and explain what broke so it stays fixed"})

	var lines []string
	for _, l := range tr.flat {
		if strings.Contains(stripANSI(l), "fix the failing test") || strings.Contains(l, "48;5;235") {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		t.Fatalf("no user card rendered:\n%s", strings.Join(tr.flat, "\n"))
	}
	for i, l := range lines {
		plain := stripANSI(l)
		if !strings.Contains(l, "48;5;235") {
			t.Fatalf("card line %d left the surface ground: %q", i, l)
		}
		if got := lipgloss.Width(plain); got != 80 {
			t.Fatalf("card line %d is %d cells, want the full 80: %q", i, got, plain)
		}
		if i == 0 && !strings.HasPrefix(strings.TrimPrefix(plain, " "), "▌") {
			t.Fatalf("the card's first line lost its patch bar: %q", plain)
		}
		if i > 0 && strings.Contains(plain, "▌") {
			t.Fatalf("continuation line repeats the bar; the card is one object: %q", plain)
		}
	}
}

func TestToolCompletionCarriesAVerdict(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	tr := testTranscript(t, 80)
	tr.add(&entry{kind: kindTool, tool: toolEntry{name: "exec", desc: "go test ./...", done: true, took: time.Second}})
	tr.add(&entry{kind: kindTool, tool: toolEntry{name: "exec", desc: "go vet ./...", done: true, failed: true, took: 2 * time.Second}})

	joined := strings.Join(tr.flat, "\n")
	if !strings.Contains(joined, "✓") {
		t.Fatalf("a completed tool drew no ✓:\n%s", joined)
	}
	if !strings.Contains(joined, "✗") {
		t.Fatalf("a failed tool drew no ✗:\n%s", joined)
	}
	if strings.Contains(joined, "ok ") || strings.Contains(joined, "failed ") {
		t.Fatalf("verdict words crept back in; the glyphs carry the verdict:\n%s", joined)
	}
}

func TestTUIToolResultCannotWriteTerminalControls(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	tr := testTranscript(t, 80)
	tr.add(&entry{kind: kindTool, expanded: true, tool: toolEntry{
		name: "exec", done: true, took: time.Millisecond,
		detail: "ok\x1b[2J\x1b]52;c;Y2xpcGJvYXJk\x07\rSPOOF\nnext\tcolumn",
	}})
	got := strings.Join(tr.flat, "\n")
	if strings.ContainsAny(got, "\x07\r") || strings.Contains(got, "\x1b[2J") || strings.Contains(got, "\x1b]52;") {
		t.Fatalf("TUI rendered unsafe tool output: %q", got)
	}
	plain := stripANSI(got)
	if !strings.Contains(plain, `\x1b`) || !strings.Contains(plain, "next") || !strings.Contains(plain, "column") {
		t.Fatalf("TUI lost visible escaping or layout: %q", plain)
	}
}

func TestTUIModelAndNoticeTextCannotWriteTerminalControls(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	tr := testTranscript(t, 80)
	tr.add(&entry{kind: kindAssistant, text: "answer\x1b[2J\x1b]52;c;Y2xpcGJvYXJk\x07"})
	tr.add(&entry{kind: kindThinking, text: "thought\rSPOOF"})
	tr.add(&entry{kind: kindNotice, level: "error", text: "provider\x1b[8mhidden"})
	got := strings.Join(tr.flat, "\n")
	if strings.ContainsAny(got, "\x07\r") || strings.Contains(got, "\x1b[2J") || strings.Contains(got, "\x1b]52;") || strings.Contains(got, "\x1b[8m") {
		t.Fatalf("TUI rendered unsafe model/notice text: %q", got)
	}
	plain := stripANSI(got)
	if !strings.Contains(plain, `\x1b`) || !strings.Contains(plain, `\x0d`) {
		t.Fatalf("TUI omitted visible control markers: %q", plain)
	}
}

func TestWorkingLineSpeaksOperator(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	m := testModel(t)
	line := stripANSI(m.workingLine())
	found := false
	for _, v := range workVerbs {
		if strings.Contains(line, v) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("working line lost the operator's verbs: %q", line)
	}
	if !strings.Contains(line, m.app.tier.ID) {
		t.Fatalf("working line lost who is working: %q", line)
	}
}

// The transcript anchors at the top: a session shorter than the viewport
// starts where the eye starts, and the empty rows fall below the content.
func TestShortTranscriptAnchorsTop(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	tr := testTranscript(t, 80)
	tr.add(&entry{kind: kindUser, text: "hello"})
	view := strings.Split(tr.view(10), "\n")
	if len(view) != 10 {
		t.Fatalf("view is %d lines, want 10", len(view))
	}
	if stripANSI(view[0]) == "" {
		t.Fatalf("content sank to the bottom; the first row is blank:\n%s", strings.Join(view, "\n"))
	}
	if stripANSI(view[len(view)-1]) != "" {
		t.Fatalf("padding went above the content, not below it")
	}
}

// Scrolling stops where the content does: a transcript that fits its
// viewport has nothing to scroll past.
func TestScrollClampsToContent(t *testing.T) {
	tr := testTranscript(t, 80)
	tr.add(&entry{kind: kindUser, text: "hello"})
	tr.view(10)
	tr.scrollBy(50)
	if tr.offset != 0 {
		t.Fatalf("scrolled %d lines past a transcript that fits the viewport", tr.offset)
	}
}

// The composer must never paint the bubbles default cursor-line slab: a
// filled input row reads as a broken artifact on any tinted terminal.
func TestComposerHasNoCursorLineSlab(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	m := testModel(t)
	zone := m.inputZoneView()
	for _, slab := range []string{"48;5;0m", "48;5;255m", "48;2;"} {
		if strings.Contains(zone, slab) {
			t.Fatalf("the composer painted a cursor-line background (%s):\n%q", slab, zone)
		}
	}
	if !strings.Contains(zone, "╭") || !strings.Contains(zone, "╰") {
		t.Fatalf("the composer lost its frame:\n%s", stripANSI(zone))
	}
}

// The turn's closing verdict closes a tool rail with the rail's own corner
// only when a rail is directly above; after prose the corner would hang
// from nothing and reads as a broken rail.
func TestDoneVerdictClosesOnlyAnOpenRail(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	tr := testTranscript(t, 80)
	tr.add(&entry{kind: kindNotice, level: "done", text: "t1 · 3s", rank: 0, rail: true})
	tr.add(&entry{kind: kindNotice, level: "done", text: "t1 · 3s", rank: 0})
	railed := stripANSI(strings.Join(tr.render(tr.entries[0]), "\n"))
	bare := stripANSI(strings.Join(tr.render(tr.entries[1]), "\n"))
	if !strings.Contains(railed, "└ ✓") {
		t.Fatalf("a rail-closing verdict lost its corner: %q", railed)
	}
	if strings.Contains(bare, "└") {
		t.Fatalf("a verdict after prose grew a corner with nothing above it: %q", bare)
	}
}

// A turn boundary breathes: a user card after content opens with a blank
// line, and the first entry after the banner does not double it.
func TestTurnBoundaryBreathes(t *testing.T) {
	tr := testTranscript(t, 80)
	tr.add(&entry{kind: kindTool, tool: toolEntry{name: "read", desc: "a.go", done: true}})
	e := tr.add(&entry{kind: kindUser, text: "next task"})
	lines := tr.render(e)
	if len(lines) == 0 || stripANSI(lines[0]) != "" {
		t.Fatalf("a user card after a rail did not open with air:\n%q", lines)
	}
}

// When the terminal narrows, the status bar sheds luxuries before facts:
// the sparkline leaves, the mode and context stay.
func TestStatusBarShedsLuxuriesFirst(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	m := testModel(t)
	m.busy = true
	m.samples = []float64{10, 20, 30}
	m.ctxWindow = 100000
	m.callTokens = 34000
	m.moves = []int{0}
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 30})
	line := stripANSI(m.statusLine())
	if strings.Contains(line, "tok/s") {
		t.Fatalf("a 60-cell bar kept the sparkline: %q", line)
	}
	for _, want := range []string{"default", "ctx 34%"} {
		if !strings.Contains(line, want) {
			t.Fatalf("a 60-cell bar dropped %q: %q", want, line)
		}
	}
}

// The tab's title answers "which terminal was that": workspace and tier,
// marked while a turn runs. Startup and every later update share one
// formatter, so the two can never disagree about what the title holds.
func TestTitleNamesWorkspaceAndTierAndMarksWork(t *testing.T) {
	m := testModel(t)
	idle := m.titleText()
	if !strings.Contains(idle, "ws") || !strings.Contains(idle, "t1") {
		t.Fatalf("idle title lost the workspace or the tier: %q", idle)
	}
	m.busy = true
	if busy := m.titleText(); !strings.HasPrefix(busy, "● ") {
		t.Fatalf("a running turn is not marked in the title: %q", busy)
	}
	m.busy = false
	m.syncTitle()
	if cmd := m.syncTitle(); cmd != nil {
		t.Fatal("an unchanged title was rewritten; the memo should keep quiet")
	}
}

func TestTitleEscapesTerminalControlsAndStaysBounded(t *testing.T) {
	m := testModel(t)
	m.app.workspace = filepath.Join(t.TempDir(), "project\x1b]2;forged\a\nname")
	m.app.tier.ID = "t1\x1b]0;spoof\a"
	title := m.titleText()
	for _, unsafe := range []string{"\x1b", "\a", "\n", "\r"} {
		if strings.Contains(title, unsafe) {
			t.Fatalf("title retained terminal control %q: %q", unsafe, title)
		}
	}
	if lipgloss.Width(title) > 120 {
		t.Fatalf("title is %d cells, want at most 120", lipgloss.Width(title))
	}
}

func TestBannerAndSuggestionsEscapeTerminalControls(t *testing.T) {
	m := testModel(t)
	m.app.workspace = filepath.Join(t.TempDir(), "repo\x1b]2;forged\a")
	m.app.config.Tiers[0].ID = "t1\x1b[2J"
	m.app.config.Tiers[0].Target.ModelID = "model\a\u202e"
	m.app.tier = m.app.config.Tiers[0]
	m.tr.reset()
	m.addBanner(m.app.loop.Session, false)
	plain := stripANSI(strings.Join(m.tr.flat, "\n"))
	for _, unsafe := range []string{"\x1b", "\a", "\u202e"} {
		if strings.Contains(plain, unsafe) {
			t.Fatalf("banner retained terminal control %q: %q", unsafe, plain)
		}
	}

	m.custom = []customCommand{{name: "inspect\x1b]0;spoof\a", desc: "custom\u202e command"}}
	m.ta.SetValue("/inspect")
	plain = stripANSI(m.suggestionsView())
	for _, unsafe := range []string{"\x1b", "\a", "\u202e"} {
		if strings.Contains(plain, unsafe) {
			t.Fatalf("suggestions retained terminal control %q: %q", unsafe, plain)
		}
	}
}

func TestPlanningAndOperationsStartTheirWorkingClock(t *testing.T) {
	m := testModel(t)

	before := time.Now()
	_, _ = m.startPlanning()
	if m.started.Before(before) || m.started.IsZero() {
		t.Fatalf("planning clock = %v, want a fresh timestamp", m.started)
	}
	m.finishPlanning()

	before = time.Now()
	_, generation, _, err := m.startOperation("resume")
	if err != nil {
		t.Fatal(err)
	}
	if m.started.Before(before) || m.started.IsZero() {
		t.Fatalf("operation clock = %v, want a fresh timestamp", m.started)
	}
	m.finishOperation(generation, false)
}

// A click lands where the view says it does: entryAt mirrors the viewport
// math, so clicking a tool rail toggles that rail and a click below the
// content toggles nothing.
func TestClickMapsToTheEntryOnThatRow(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	m := testModel(t)
	m.tr.reset()
	m.tr.add(&entry{kind: kindInfo, text: "one line"})
	tool := m.tr.add(&entry{kind: kindTool, tool: toolEntry{name: "exec", desc: "go test", done: true, took: time.Second, detail: "ok\nmore"}})
	m.tr.view(10)

	toolStart := m.tr.starts[m.tr.indexOf(tool)]
	if got := m.tr.entryAt(toolStart); got != m.tr.indexOf(tool) {
		t.Fatalf("the tool's first row maps to entry %d, want %d", got, m.tr.indexOf(tool))
	}
	if got := m.tr.entryAt(9); got != -1 {
		t.Fatalf("a click on bottom padding mapped to entry %d, want none", got)
	}
	if got := m.tr.entryAt(-1); got != -1 {
		t.Fatal("a row outside the viewport mapped to an entry")
	}
}

// Queued prompts are visible and droppable: a prompt that silently queued
// is a prompt the user may believe was lost.
func TestQueueShowsAndClears(t *testing.T) {
	m := testModel(t)
	m.queue = []string{"first waiting prompt", "second waiting prompt"}
	cmdQueue(m, "")
	joined := strings.Join(m.tr.flat, "\n")
	if !strings.Contains(joined, "2 queued") || !strings.Contains(joined, "second waiting") {
		t.Fatalf("/queue did not list the queue:\n%s", joined)
	}
	cmdQueue(m, "clear")
	if len(m.queue) != 0 {
		t.Fatal("/queue clear left prompts queued")
	}
}

func TestTruncatedLocalListingsRedactCredentialsBeforeTheirCaps(t *testing.T) {
	m := testModel(t)
	m.queue = []string{strings.Repeat("x", 68) + testGitHubToken}
	cmdQueue(m, "")
	joined := strings.Join(m.tr.flat, "\n")
	if strings.Contains(joined, "ghp_") || strings.Contains(joined, testGitHubToken) {
		t.Fatalf("queued prompt listing exposed a credential fragment: %q", joined)
	}

	for _, message := range []provider.Message{
		provider.UserText(strings.Repeat("x", 158) + testGitHubToken),
		provider.UserText(compactSeedHead + "\n\n" + strings.Repeat("x", 158) + testGitHubToken),
	} {
		notice := syntheticReplayNotice(message)
		if strings.Contains(notice, "ghp_") || strings.Contains(notice, testGitHubToken) {
			t.Fatalf("synthetic replay notice exposed a credential fragment: %q", notice)
		}
	}
}

// /changes maps files to the turns that touched them, states its scope -
// the recorder's, not the workspace's - and says the way to act on what
// it shows.
func TestChangesMapsFilesToTurns(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	dir := t.TempDir()
	path := dir + "/main.go"
	os.WriteFile(path, []byte("x"), 0o644)
	m.app.undo.Begin("fix the flaky test")
	m.app.undo.Record(path)

	cmdChanges(m, "")
	joined := strings.Join(m.tr.flat, "\n")
	for _, want := range []string{"fix the flaky test", "main.go", "shell command's side effects are not captured", "/undo"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("/changes is missing %q:\n%s", want, joined)
		}
	}
}

// /context names the window's composition in the estimator's own terms and
// keeps the two measurements apart: the split is estimated, the meter is
// what the provider reported.
func TestContextSplitsTheZones(t *testing.T) {
	m := testModel(t)
	m.app.config.CompactAuto = true
	m.app.loop.System = []provider.Block{provider.Text{Text: strings.Repeat("s", 400)}}
	m.app.loop.Session.AppendMessage(provider.UserText(strings.Repeat("c", 800)))
	cmdContext(m, "")
	joined := strings.Join(m.tr.flat, "\n")
	for _, want := range []string{"system", "tools", "conversation", "estimated", "auto-compact fires at 85%"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("/context is missing %q:\n%s", want, joined)
		}
	}

	// Disarmed means unsaid: a tripwire that will not fire is not a fact
	// about this session's window.
	m.app.config.CompactAuto = false
	m.tr.reset()
	cmdContext(m, "")
	if strings.Contains(strings.Join(m.tr.flat, "\n"), "auto-compact fires") {
		t.Fatal("/context announced a disarmed tripwire")
	}
}

// /undo <path> is the surgical form: one file back to before the newest
// turn that captured it, matched the way /changes displays it, the turn's
// other files standing.
func TestUndoPathRestoresOneFile(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	m.app.workspace = m.app.loop.Tools.Root()
	path := m.app.workspace + "/main.go"
	os.WriteFile(path, []byte("before"), 0o644)
	if result := runSurfaceTool(t, m.app.loop.Tools, "read", map[string]any{"path": "main.go"}); result.IsError {
		t.Fatalf("fixture read failed: %s", result.Content)
	}
	m.app.undo.Begin("the turn")
	m.app.undo.Record(path)
	os.WriteFile(path, []byte("after"), 0o644)
	semantic := &lspView{}
	m.full = semantic
	workspaceEpoch := m.workspaceRuntime.epoch.Load()

	cmdUndo(m, "main.go")
	if got, _ := os.ReadFile(path); string(got) != "before" {
		t.Fatalf("the file holds %q, want its pre-turn content", got)
	}
	joined := strings.Join(m.tr.flat, "\n")
	if !strings.Contains(joined, "restored main.go") {
		t.Fatalf("the restore was not reported:\n%s", joined)
	}
	if got := m.workspaceRuntime.epoch.Load(); got != workspaceEpoch+1 || !semantic.stale {
		t.Fatalf("undo left final-code caches live: epoch=%d want=%d lsp-stale=%v", got, workspaceEpoch+1, semantic.stale)
	}
	if result := runSurfaceTool(t, m.app.loop.Tools, "edit", map[string]any{
		"path": "main.go", "old_string": "before", "new_string": "again",
	}); !result.IsError {
		t.Fatal("undo left the model's pre-undo read authority live")
	}

	m.tr.reset()
	if cmd := cmdUndo(m, "absent.go"); cmd != nil {
		if msg := cmd(); msg != nil {
			m.Update(msg)
		}
	}
	if joined := strings.Join(m.tr.flat, "\n"); !strings.Contains(joined, "no turn captured") {
		t.Fatalf("an uncaptured path did not say so:\n%s", joined)
	}
}

func TestUndoTurnInvalidatesWorkspaceAndSemanticCaches(t *testing.T) {
	m := testModel(t)
	m.app.undo = checkpoint.NewRecorder()
	m.app.workspace = m.app.loop.Tools.Root()
	path := m.app.workspace + "/main.go"
	os.WriteFile(path, []byte("before"), 0o644)
	m.app.undo.Begin("the turn")
	m.app.undo.Record(path)
	os.WriteFile(path, []byte("after"), 0o644)
	semantic := &lspView{}
	m.full = semantic
	workspaceEpoch := m.workspaceRuntime.epoch.Load()

	if cmd := cmdUndo(m, ""); cmd != nil {
		if msg := cmd(); msg != nil {
			m.Update(msg)
		}
	}
	if got := m.workspaceRuntime.epoch.Load(); got != workspaceEpoch+1 || !semantic.stale {
		t.Fatalf("whole-turn undo left final-code caches live: epoch=%d want=%d lsp-stale=%v", got, workspaceEpoch+1, semantic.stale)
	}
}

// /copy code takes a block a mouse selection across wrapped styled lines
// would mangle. Blocks count newest-first across responses, both fence
// styles read, and a fence a stream left unclosed still yields its code.
func TestCodeBlocksExtractFences(t *testing.T) {
	text := "intro\n```go\nfunc a() {}\n```\nmiddle\n~~~\nplain block\n~~~\ntail\n```py\nunclosed"
	blocks := codeBlocks(text)
	if len(blocks) != 3 {
		t.Fatalf("got %d blocks, want 3: %q", len(blocks), blocks)
	}
	if blocks[0] != "func a() {}" || blocks[1] != "plain block" || blocks[2] != "unclosed" {
		t.Fatalf("blocks = %q", blocks)
	}
	if len(codeBlocks("no fences here")) != 0 {
		t.Fatal("prose grew a code block")
	}
}

func TestCopyCodeCountsNewestFirst(t *testing.T) {
	m := testModel(t)
	m.tr.add(&entry{kind: kindAssistant, text: "```\nold block\n```"})
	m.tr.add(&entry{kind: kindAssistant, text: "```\nnew block\n```"})

	if cmd := cmdCopy(m, "code 5"); cmd != nil {
		if msg := cmd(); msg != nil {
			m.Update(msg)
		}
	}
	if joined := strings.Join(m.tr.flat, "\n"); !strings.Contains(joined, "only 2 code blocks") {
		t.Fatalf("an out-of-range block did not say the count:\n%s", joined)
	}
}

// The moment of granting is the moment that has to be plain: /trust names
// what this checkout's declarations would actually enable - which servers,
// which hooks - before and at the grant, and reads them without running
// anything.
func TestTrustNamesWhatAGrantCovers(t *testing.T) {
	m := testModel(t)
	m.app.workspace = t.TempDir()
	dir := m.app.workspace + "/.switchboard"
	os.MkdirAll(dir, 0o755)
	os.WriteFile(dir+"/mcp.toml", []byte("[mcp.docs]\ncommand = \"npx some-docs-server\"\n"), 0o644)
	os.WriteFile(dir+"/hooks.toml", []byte("[[hooks.pre_tool]]\ntools = [\"exec\"]\nrun = \"./guard.sh\"\n"), 0o644)

	decls := trustDeclarations(m)
	joined := strings.Join(decls, "\n")
	for _, want := range []string{"/diff", "Git status", "filters and hooks", "docs", "npx some-docs-server", "exec", "guard.sh"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("declarations missing %q:\n%s", want, joined)
		}
	}
}

func TestTrustGrantAlwaysDisclosesGitExecution(t *testing.T) {
	m := testModel(t)
	m.app.workspace = t.TempDir()
	store, err := trust.OpenFile(filepath.Join(t.TempDir(), "trust.toml"))
	if err != nil {
		t.Fatal(err)
	}
	m.app.trust = store
	if cmd := cmdTrust(m, "grant"); cmd != nil {
		m.Update(cmd())
	}
	rendered := strings.Join(m.tr.flat, "\n")
	for _, want := range []string{"/diff", "Git filters/hooks", "repository filters and hooks may execute"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("grant confirmation omitted %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "enables nothing") {
		t.Fatalf("grant falsely claimed it enables nothing:\n%s", rendered)
	}
}

func TestTrustDeclarationTruncationNeverExposesCredentialFragments(t *testing.T) {
	m := testModel(t)
	m.app.workspace = t.TempDir()
	dir := m.app.workspace + "/.switchboard"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[mcp.docs]\ncommand = \"" + strings.Repeat("x", 48) + testGitHubToken + "\"\n"
	if err := os.WriteFile(dir+"/mcp.toml", []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(trustDeclarations(m), "\n")
	if strings.Contains(joined, "ghp_") || strings.Contains(joined, testGitHubToken) {
		t.Fatalf("trust declaration exposed a credential fragment: %q", joined)
	}
}

// Every command appears in exactly one help group: a new command that
// misses the page would otherwise be invisible everywhere but the
// autocomplete, and a name in two groups would read as two commands.
func TestHelpGroupsCoverEveryCommandOnce(t *testing.T) {
	seen := map[string]int{}
	for _, g := range helpGroups {
		for _, name := range g.names {
			seen[name]++
		}
	}
	for _, c := range commands() {
		if seen[c.name] != 1 {
			t.Errorf("command %q appears %d times in help groups, want exactly once", c.name, seen[c.name])
		}
		delete(seen, c.name)
	}
	for name := range seen {
		t.Errorf("help groups name %q, which is not a command", name)
	}
}

func TestSandboxStatusIsAdvertisedOnBothInteractiveHelpSurfaces(t *testing.T) {
	var tuiUsage string
	for _, command := range commands() {
		if command.name == "sandbox" {
			tuiUsage = command.usage
			break
		}
	}
	if tuiUsage != "[off|on|auto|status]" {
		t.Fatalf("TUI sandbox usage = %q", tuiUsage)
	}

	output, err := os.CreateTemp(t.TempDir(), "repl-help")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = output.Close() })
	r := &repl{out: newRenderer(output), config: &config.Config{}}
	if exit := r.command(context.Background(), "/help"); exit {
		t.Fatal("REPL help exited the session")
	}
	r.out.flush()
	text, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), "/sandbox [off|on|auto|status]") {
		t.Fatalf("REPL help omitted /sandbox status:\n%s", text)
	}
}

// A governed session's spend readout warms as the ceiling nears, the same
// thresholds the context gauge uses: the warning comes before the refusal.
func TestBudgetReadoutWarmsBeforeTheCeiling(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	m := testModel(t)
	cat, priced := pricedTarget(t)
	m.app.catalog = cat
	m.app.loop.Target = priced
	m.app.budget = &budgetState{}
	m.app.budget.set(catalog.Money(1_000_000)) // a $1.00 ceiling

	state := m.app.loop.Session.State()
	state.CostMicroUSD = 700_000 // 70% spent
	m.refreshCost(state)
	if m.costPct != 70 {
		t.Fatalf("costPct = %d, want 70", m.costPct)
	}
	if line, want := m.statusLine(), m.th.onBar(m.th.warn).Render(m.costLine); !strings.Contains(line, want) {
		t.Fatalf("a 70%% spent ceiling did not warm the readout:\n%q", line)
	}
	state.RetryReserveMicroUSD = 100_000
	m.refreshCost(state)
	if m.costPct != 80 || !strings.Contains(m.costLine, catalog.Money(100_000).String()+" reserve") {
		t.Fatalf("failed-attempt reserve missing from readout: pct=%d line=%q", m.costPct, m.costLine)
	}

	state.CostMicroUSD = 900_000
	m.refreshCost(state)
	if line, want := m.statusLine(), m.th.onBar(m.th.err).Render(m.costLine); !strings.Contains(line, want) {
		t.Fatalf("a 90%% spent ceiling did not turn the readout red:\n%q", line)
	}

	// A switch to a local rung must not hide dollars this same session already
	// spent before the move. The accumulated ledger, not the active target,
	// owns the ratio.
	m.app.loop.Target = provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "q"}
	m.refreshCost(state)
	if m.costPct != 100 || !strings.Contains(m.costLine, "$0.9000") || !strings.Contains(m.costLine, "$0.1000 reserve") || strings.Contains(m.costLine, "local") {
		t.Fatalf("a paid-to-local move hid accumulated dollars: pct=%d line=%q", m.costPct, m.costLine)
	}
	state.CostMicroUSD = 0
	m.refreshCost(state)
	if m.costPct != 0 || m.costLine != "local" {
		t.Fatalf("a genuinely zero-dollar local session was misclassified: pct=%d line=%q", m.costPct, m.costLine)
	}

	m.app.budget = nil
	m.app.loop.Target = priced
	m.refreshCost(session.State{})
	if m.costPct != 0 {
		t.Fatalf("an ungoverned session kept a stale ratio: %d", m.costPct)
	}
}

func TestCostReadoutKeepsObservedDollarsAcrossRoutingDirections(t *testing.T) {
	cat, paid := pricedTarget(t)
	local := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "qwen3:4b"}
	for _, tc := range []struct {
		name        string
		startedOn   provider.RouteTarget
		currentlyOn provider.RouteTarget
	}{
		{name: "local-to-paid", startedOn: local, currentlyOn: paid},
		{name: "paid-to-local", startedOn: paid, currentlyOn: local},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testModel(t)
			m.app.catalog = cat
			m.app.loop.Target = tc.currentlyOn
			m.refreshCost(session.State{Target: string(tc.startedOn.ID()), CostMicroUSD: 420_000})
			if !strings.Contains(m.costLine, "$0.4200") || strings.Contains(m.costLine, "local") || strings.Contains(m.costLine, "plan") {
				t.Fatalf("routed cost readout = %q, want the full observed dollar amount", m.costLine)
			}
		})
	}
}

func TestCostReadoutShowsZeroDollarMeteringMix(t *testing.T) {
	m := testModel(t)
	cat, _ := pricedTarget(t)
	m.app.catalog = cat
	local := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "qwen3:4b"}
	planTarget := provider.RouteTarget{Provider: "openai", Surface: "subscription", ModelID: "gpt-5.6-sol"}
	m.app.loop.Target = planTarget
	m.refreshCost(session.State{UsageTargets: []string{string(local.ID()), string(planTarget.ID())}})
	if !strings.Contains(m.costLine, "mixed") || !strings.Contains(m.costLine, "local + plan") {
		t.Fatalf("zero-dollar routed calls were flattened to the active target: %q", m.costLine)
	}
}

// alt+N jumps to rung N; a digit past the ladder says how many rungs
// exist instead of guessing, and a plain digit still types.
func TestAltDigitJumpsToTheRung(t *testing.T) {
	m := testModel(t)
	if cmd := m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'5'}, Alt: true}); cmd != nil {
		if msg := cmd(); msg != nil {
			m.Update(msg)
		}
	}
	joined := strings.Join(m.tr.flat, "\n")
	if !strings.Contains(joined, "the ladder has 1 rungs") {
		t.Fatalf("an out-of-ladder alt+digit did not say the count:\n%s", joined)
	}
	before := m.ta.Value()
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'5'}})
	if m.ta.Value() == before {
		t.Fatal("a plain digit stopped reaching the composer")
	}
}
