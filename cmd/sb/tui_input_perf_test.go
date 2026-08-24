package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/switchboard-code/switchboard/internal/config"
)

func TestStatusLineIsCellBoundedAndEscapesMetadata(t *testing.T) {
	m := testModel(t)
	m.app.tier.ID = "界界界界]0;forged\a"
	m.app.tier.Label = "e\u0301 🧑🏽‍💻\u202e"
	m.tierLine = "tier  openai/responses/模型-🚀[2J"
	m.updateAvail = "v2\a\u202e"
	m.costLine = "unpriced\rspoof"
	m.ctxWindow = 100
	m.callTokens = 75

	for _, width := range []int{20, 31, 80} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m.width = width
			line := m.statusLine()
			if got := ansi.StringWidth(line); got > width {
				t.Fatalf("status width = %d, want at most %d:\n%q", got, width, line)
			}
			plain := stripANSI(line)
			for _, unsafe := range []string{"\a", "\r", "\n", "\u202e"} {
				if strings.Contains(plain, unsafe) {
					t.Fatalf("%d-cell status retained terminal control %q: %q", width, unsafe, plain)
				}
			}
		})
	}
}

func TestPromptEditorArgvPreservesQuotesWithoutShellExpansion(t *testing.T) {
	t.Setenv("VISUAL", `"/Applications/My Editor.app/editor" --wait "profile one" '$HOME'`)
	t.Setenv("EDITOR", "ignored")

	got, err := promptEditorArgv()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/Applications/My Editor.app/editor", "--wait", "profile one", "$HOME"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("editor argv = %q, want %q", got, want)
	}

	t.Setenv("VISUAL", `"unterminated`)
	if _, err := promptEditorArgv(); err == nil || !strings.Contains(err.Error(), "unmatched quote") {
		t.Fatalf("unmatched editor quote error = %v", err)
	}
}

func TestPromptEditorRefusesToSuspendDuringActiveWork(t *testing.T) {
	m := testModel(t)
	m.busy = true
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")

	cmd := m.openEditor()
	if cmd == nil {
		t.Fatal("busy editor request returned no explanation")
	}
	msg, ok := cmd().(noticeMsg)
	if !ok || msg.level != "warn" || !strings.Contains(msg.text, "while work is running") {
		t.Fatalf("busy editor request = %#v", msg)
	}
}

func TestCustomCommandsAppearInPaletteWithHonestCollisionSelectors(t *testing.T) {
	m := testModel(t)
	m.custom = []customCommand{
		{name: "deploy", desc: "ship it", body: "deploy $ARGUMENTS", fromHome: true},
		{name: "help", desc: "team help", body: "custom help $ARGUMENTS"},
		{name: "quit", desc: "not the builtin alias", body: "custom quit"},
		{name: "t1", desc: "not the tier", body: "custom tier"},
	}
	m.openPalette()
	dialog, ok := m.dlg.(*pickerDialog)
	if !ok {
		t.Fatalf("palette = %T, want picker", m.dlg)
	}
	items := map[string]pickerItem{}
	for _, item := range dialog.items {
		items[item.id] = item
	}
	for selector, label := range map[string]string{
		"deploy":      "/deploy",
		"custom:help": "/custom:help",
		"custom:quit": "/custom:quit",
		"custom:t1":   "/custom:t1",
	} {
		item, exists := items[selector]
		if !exists || item.label != label || !strings.Contains(item.desc, "command") {
			t.Errorf("palette item %q = %#v", selector, item)
		}
	}

	// The ordinary spelling keeps the built-in meaning; the palette's
	// namespaced spelling reaches the colliding custom definition and records
	// the exact spelling that did so.
	m.completeDialog()
	before := len(m.tr.entries)
	if cmd := m.runSlash("/help"); cmd != nil {
		t.Fatal("builtin /help unexpectedly became asynchronous custom expansion")
	}
	if len(m.tr.entries) <= before {
		t.Fatal("builtin /help did not render its help surface")
	}
	cmd := m.runSlash("/custom:help details")
	msg, ok := cmd().(expandedCustomMsg)
	if !ok || msg.prompt != "custom help details" || msg.authored != "/custom:help details" {
		t.Fatalf("explicit custom dispatch = %#v", msg)
	}
}

func TestCollidingCustomSuggestionShowsRunnableSelector(t *testing.T) {
	m := testModel(t)
	m.custom = []customCommand{{name: "review", desc: "project review", body: "review it"}}
	m.ta.SetValue("/rev")

	seenBuiltin, seenCustom := false, false
	for _, item := range m.suggestions() {
		switch item.name {
		case "review":
			seenBuiltin = true
		case "custom:review":
			seenCustom = true
		}
	}
	if !seenBuiltin || !seenCustom {
		t.Fatalf("/rev suggestions builtin=%v custom=%v: %#v", seenBuiltin, seenCustom, m.suggestions())
	}

	m.ta.SetValue("/custom:rev")
	m.acceptSuggestion()
	if got := m.ta.Value(); got != "/custom:review " {
		t.Fatalf("accepted custom selector = %q", got)
	}
}

func TestCustomNamespaceAndWhitespaceNamesRemainRunnable(t *testing.T) {
	m := testModel(t)
	m.custom = []customCommand{
		{name: "help", body: "ordinary collision"},
		{name: "custom:help", body: "nested namespace $ARGUMENTS"},
		{name: "team review", body: "spaced name $ARGUMENTS"},
	}
	// Even a tier that claims the first explicit spelling cannot make the
	// custom namespace dispatch to the tier instead.
	m.app.config.Tiers = append(m.app.config.Tiers, config.Tier{ID: "custom:help"})

	for _, test := range []struct {
		custom customCommand
		want   string
	}{
		{custom: m.custom[0], want: "ordinary collision"},
		{custom: m.custom[1], want: "nested namespace details"},
		{custom: m.custom[2], want: "spaced name details"},
	} {
		selector := customSelector(m, test.custom)
		args := ""
		if strings.Contains(test.custom.body, "$ARGUMENTS") {
			args = " details"
		}
		cmd := m.runSlash("/" + selector + args)
		if cmd == nil {
			t.Fatalf("/%s returned no expansion command", selector)
		}
		msg, ok := cmd().(expandedCustomMsg)
		if !ok || msg.prompt != test.want {
			t.Fatalf("/%s expanded to %#v, want %q", selector, msg, test.want)
		}
	}
}

func TestComposerEditingKeysAreNotStolenByTranscriptNavigation(t *testing.T) {
	tests := []struct {
		name string
		key  tea.KeyType
		prep func(*tuiModel)
		want string
		col  int
	}{
		{
			name: "ctrl-u deletes before cursor", key: tea.KeyCtrlU,
			prep: func(m *tuiModel) { m.ta.SetValue("alpha\nbeta") },
			want: "alpha\n", col: 0,
		},
		{
			name: "ctrl-d deletes forward", key: tea.KeyCtrlD,
			prep: func(m *tuiModel) { m.ta.SetValue("alpha\nbeta"); m.ta.CursorStart() },
			want: "alpha\neta", col: 0,
		},
		{
			name: "ctrl-f moves forward", key: tea.KeyCtrlF,
			prep: func(m *tuiModel) { m.ta.SetValue("alpha\nbeta"); m.ta.CursorStart() },
			want: "alpha\nbeta", col: 1,
		},
		{
			name: "home moves to line start", key: tea.KeyHome,
			prep: func(m *tuiModel) { m.ta.SetValue("alpha\nbeta") },
			want: "alpha\nbeta", col: 0,
		},
		{
			name: "end moves to line end", key: tea.KeyEnd,
			prep: func(m *tuiModel) { m.ta.SetValue("alpha\nbeta"); m.ta.CursorStart() },
			want: "alpha\nbeta", col: 4,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := testModel(t)
			test.prep(m)
			m.tr.offset = 7
			m.key(tea.KeyMsg{Type: test.key})
			if got := m.ta.Value(); got != test.want {
				t.Fatalf("value = %q, want %q", got, test.want)
			}
			info := m.ta.LineInfo()
			if got := info.StartColumn + info.ColumnOffset; got != test.col {
				t.Fatalf("cursor column = %d, want %d", got, test.col)
			}
			if m.tr.offset != 7 {
				t.Fatalf("transcript offset changed to %d while editing", m.tr.offset)
			}
			if m.trSearch {
				t.Fatal("ctrl+f opened transcript search while editing")
			}
		})
	}
}

func TestMultilineAndSoftWrappedArrowsStayInComposer(t *testing.T) {
	m := testModel(t)
	m.history = []string{"old prompt"}
	m.histIdx = len(m.history)
	m.ta.SetValue("top\nbottom")
	if m.ta.Line() != 1 {
		t.Fatalf("fixture cursor line = %d", m.ta.Line())
	}
	m.key(tea.KeyMsg{Type: tea.KeyUp})
	if m.ta.Line() != 0 || m.ta.Value() != "top\nbottom" {
		t.Fatalf("up escaped multiline composer: line=%d value=%q", m.ta.Line(), m.ta.Value())
	}
	m.key(tea.KeyMsg{Type: tea.KeyDown})
	if m.ta.Line() != 1 {
		t.Fatalf("down did not return to second line: %d", m.ta.Line())
	}

	m.ta.SetWidth(8)
	m.ta.SetValue("a long soft wrapped paragraph")
	if !m.composerMultiline() {
		t.Fatal("soft-wrapped paragraph was treated as one-line history input")
	}
	m.key(tea.KeyMsg{Type: tea.KeyUp})
	if got := m.ta.Value(); got != "a long soft wrapped paragraph" {
		t.Fatalf("soft-wrapped up recalled history: %q", got)
	}
}

func TestComposerDeletionIsAtomicAcrossExtendedGraphemeClusters(t *testing.T) {
	clusters := []string{"e\u0301", "👩‍💻", "👍🏽", "🇺🇸", "🏳️‍🌈"}
	for _, cluster := range clusters {
		t.Run(fmt.Sprintf("backspace_%x", []byte(cluster)), func(t *testing.T) {
			m := testModel(t)
			m.ta.SetValue("A" + cluster)
			m.key(tea.KeyMsg{Type: tea.KeyBackspace})
			if got := m.ta.Value(); got != "A" {
				t.Fatalf("backspace left a partial grapheme: %q", got)
			}
		})
		t.Run(fmt.Sprintf("delete_%x", []byte(cluster)), func(t *testing.T) {
			m := testModel(t)
			m.ta.SetValue("A" + cluster + "B")
			m.ta.SetCursor(1)
			m.key(tea.KeyMsg{Type: tea.KeyDelete})
			if got := m.ta.Value(); got != "AB" {
				t.Fatalf("delete left a partial grapheme: %q", got)
			}
		})
	}
}

func TestComposerDeletionRemovesClusterWhenRuneCursorIsInsideIt(t *testing.T) {
	for _, key := range []tea.KeyType{tea.KeyBackspace, tea.KeyDelete} {
		m := testModel(t)
		m.ta.SetValue("A👩‍💻B")
		m.ta.SetCursor(2) // after the pictograph, before the ZWJ and laptop
		m.key(tea.KeyMsg{Type: key})
		if got := m.ta.Value(); got != "AB" {
			t.Fatalf("%s at an interior rune boundary left %q", key, got)
		}
		if info := m.ta.LineInfo(); info.StartColumn+info.ColumnOffset != 1 {
			t.Fatalf("%s cursor did not land at the deleted cluster boundary: %+v", key, info)
		}
	}
}

func TestComposerCursorMovesAcrossExtendedGraphemeClusters(t *testing.T) {
	clusters := []string{"e\u0301", "👩‍💻", "👍🏽", "🇺🇸", "🏳️‍🌈"}
	for _, cluster := range clusters {
		t.Run(fmt.Sprintf("cluster_%x", []byte(cluster)), func(t *testing.T) {
			m := testModel(t)
			m.ta.SetValue(cluster + "X")
			m.ta.CursorStart()

			m.key(tea.KeyMsg{Type: tea.KeyRight})
			want := len([]rune(cluster))
			if info := m.ta.LineInfo(); info.StartColumn+info.ColumnOffset != want {
				t.Fatalf("right cursor column = %d, want grapheme end %d", info.StartColumn+info.ColumnOffset, want)
			}
			m.key(tea.KeyMsg{Type: tea.KeyLeft})
			if info := m.ta.LineInfo(); info.StartColumn+info.ColumnOffset != 0 {
				t.Fatalf("left cursor column = %d, want grapheme start", info.StartColumn+info.ColumnOffset)
			}
		})
	}
}

func TestComposerEmacsCharacterKeysAreGraphemeAtomic(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyType
		want string
		col  int
	}{
		{name: "ctrl-b", key: tea.KeyCtrlB, want: "A👩‍💻B", col: 1},
		{name: "ctrl-f", key: tea.KeyCtrlF, want: "A👩‍💻B", col: 4},
		{name: "ctrl-h", key: tea.KeyCtrlH, want: "AB", col: 1},
		{name: "ctrl-d", key: tea.KeyCtrlD, want: "AB", col: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testModel(t)
			m.ta.SetValue("A👩‍💻B")
			switch tc.key {
			case tea.KeyCtrlB, tea.KeyCtrlH:
				m.ta.SetCursor(4)
			default:
				m.ta.SetCursor(1)
			}
			m.key(tea.KeyMsg{Type: tc.key})
			if got := m.ta.Value(); got != tc.want {
				t.Fatalf("value = %q, want %q", got, tc.want)
			}
			if info := m.ta.LineInfo(); info.StartColumn+info.ColumnOffset != tc.col {
				t.Fatalf("cursor column = %d, want %d", info.StartColumn+info.ColumnOffset, tc.col)
			}
		})
	}
}

func TestComposerCursorEscapesInteriorGraphemeRunePositions(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyType
		want int
	}{
		{name: "left", key: tea.KeyLeft, want: 1},
		{name: "right", key: tea.KeyRight, want: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testModel(t)
			m.ta.SetValue("A👩‍💻B")
			m.ta.SetCursor(2) // after the pictograph, inside the ZWJ cluster
			m.key(tea.KeyMsg{Type: tc.key})
			if info := m.ta.LineInfo(); info.StartColumn+info.ColumnOffset != tc.want {
				t.Fatalf("cursor column = %d, want cluster boundary %d", info.StartColumn+info.ColumnOffset, tc.want)
			}
		})
	}
}

func TestComposerGraphemeCursorPreservesTextareaNewlineCrossing(t *testing.T) {
	m := testModel(t)
	m.ta.SetValue("A\nB")
	m.ta.SetCursor(0)
	m.key(tea.KeyMsg{Type: tea.KeyLeft})
	if m.ta.Line() != 0 {
		t.Fatalf("left at logical line start did not cross newline: line=%d", m.ta.Line())
	}
	m.key(tea.KeyMsg{Type: tea.KeyRight})
	if m.ta.Line() != 1 {
		t.Fatalf("right at logical line end did not cross newline: line=%d", m.ta.Line())
	}
}

func TestComposerGraphemeDeletionPreservesTextareaNewlineMerging(t *testing.T) {
	m := testModel(t)
	m.ta.SetValue("A\nB")
	m.ta.SetCursor(0)
	m.key(tea.KeyMsg{Type: tea.KeyBackspace})
	if got := m.ta.Value(); got != "AB" {
		t.Fatalf("backspace at logical line start no longer merges lines: %q", got)
	}

	m.ta.SetValue("A\nB")
	m.ta.CursorUp()
	m.ta.CursorEnd()
	m.key(tea.KeyMsg{Type: tea.KeyDelete})
	if got := m.ta.Value(); got != "AB" {
		t.Fatalf("delete at logical line end no longer merges lines: %q", got)
	}
}

func TestMentionCompletionReplacesTokenAtCursor(t *testing.T) {
	m := testModel(t)
	m.mentionList = []string{"design.go"}
	m.mentionListAt = time.Now()
	m.ta.SetValue("read @desIGN after")
	for range []rune("IGN after") {
		m.ta, _ = m.ta.Update(tea.KeyMsg{Type: tea.KeyLeft})
	}
	if fragment, _, ok := m.currentMention(); !ok || fragment != "des" {
		t.Fatalf("mention under cursor = %q, %v", fragment, ok)
	}
	m.acceptMention()
	if got := m.ta.Value(); got != "read @design.go after" {
		t.Fatalf("cursor completion = %q", got)
	}
	m.ta, _ = m.ta.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'X'}})
	if got := m.ta.Value(); got != "read @design.go Xafter" {
		t.Fatalf("completion cursor landed at the wrong position: %q", got)
	}
}

func TestMentionCompletionUsesSharedIndexOffEventLoop(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "needed.go"), []byte("package nested\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := testModel(t)
	m.app.workspace = root
	m.workspaceRuntime = newWorkspaceRuntime(root)
	m.mentionList = nil
	m.ta.SetValue("inspect @nee")

	cmd := m.refreshMentionMatches()
	if cmd == nil {
		t.Fatal("first mention did not schedule an index query")
	}
	if got := m.mentionMatches(); len(got) != 0 {
		t.Fatalf("matches appeared synchronously before index result: %v", got)
	}
	msg, ok := cmd().(mentionMatchesMsg)
	if !ok || msg.err != nil {
		t.Fatalf("index completion = %#v", msg)
	}
	_, followup := m.Update(msg)
	if followup != nil {
		t.Fatal("current index result unexpectedly scheduled another query")
	}
	if got := m.mentionMatches(); len(got) != 1 || got[0] != "nested/needed.go" {
		t.Fatalf("indexed mention matches = %v", got)
	}

	// A durable workspace invalidation makes the cached result ineligible even
	// before a replacement result arrives.
	m.workspaceRuntime.invalidate()
	if got := m.mentionMatches(); len(got) != 0 {
		t.Fatalf("stale indexed matches remained visible after invalidation: %v", got)
	}
	if got := m.refreshMentionMatches(); got == nil {
		t.Fatal("workspace invalidation did not schedule refreshed completion")
	}
}
