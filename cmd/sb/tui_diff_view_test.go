package main

import (
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func TestDiffVisualLinesWrapByTerminalCells(t *testing.T) {
	styled := "\x1b[31m+界界e\u0301🙂ab界\x1b[0m"
	d := &diffView{lines: []string{styled}}

	rows := d.visualLines(5)
	wants := []int{5, 5, 2}
	if len(rows) != len(wants) {
		t.Fatalf("visual rows = %q, want %d rows", rows, len(wants))
	}
	for i, row := range rows {
		if got := ansi.StringWidth(row); got != wants[i] {
			t.Errorf("row %d width = %d, want %d: %q", i, got, wants[i], row)
		}
		if !strings.HasSuffix(row, ansi.ResetStyle) {
			t.Errorf("row %d does not close its style: %q", i, row)
		}
	}
	if !strings.Contains(rows[1], "\x1b[31m") {
		t.Fatalf("continuation row did not resume its style: %q", rows[1])
	}
	if got, want := ansi.Strip(strings.Join(rows, "")), ansi.Strip(styled); got != want {
		t.Fatalf("wrapped content = %q, want %q", got, want)
	}
}

func TestDiffViewNeverExceedsViewportCells(t *testing.T) {
	d := &diffView{lines: []string{
		"\x1b[32m+" + strings.Repeat("界", 20) + "\x1b[0m",
		"\t" + strings.Repeat("long", 20),
	}}

	for _, width := range []int{1, 7, 19} {
		got := d.view(width, 8, darkTheme())
		rows := strings.Split(got, "\n")
		if len(rows) != 8 {
			t.Errorf("width %d: frame rows = %d, want 8\n%q", width, len(rows), got)
		}
		for row, text := range rows {
			if cells := ansi.StringWidth(text); cells > width {
				t.Errorf("width %d: row %d occupies %d cells: %q", width, row, cells, text)
			}
		}
	}
}

func TestDiffVisualLinesResumeCompositeTrueColorStyle(t *testing.T) {
	styled := "\x1b[1m\x1b[38;2;0;255;0mabcdef\x1b[0m"
	rows := (&diffView{lines: []string{styled}}).visualLines(3)
	if len(rows) != 2 {
		t.Fatalf("visual rows = %q, want 2", rows)
	}
	for _, sequence := range []string{"\x1b[1m", "\x1b[38;2;0;255;0m"} {
		if !strings.Contains(rows[1], sequence) {
			t.Errorf("continuation row lost %q: %q", sequence, rows[1])
		}
	}
}

func TestDiffViewBottomKeyUsesWrappedRows(t *testing.T) {
	d := &diffView{lines: []string{strings.Repeat("x", 20)}}
	_ = d.view(5, 3, darkTheme()) // one body row; four wrapped rows total

	close, cmd := d.key(runeKey('G'))
	if close || cmd != nil {
		t.Fatalf("bottom key = (close %v, cmd %v), want (false, nil)", close, cmd)
	}
	_ = d.view(5, 3, darkTheme())
	if d.offset != 3 {
		t.Fatalf("bottom offset = %d, want 3 visual rows", d.offset)
	}
}

func TestHighlightDiffAsciiProfileIsPlain(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(previous) })

	text := "diff --git a/a.go b/a.go\n-old\n+new\n"
	got := highlightDiff(text, true)
	want := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ASCII diff = %q, want plain %q", got, want)
	}
	if strings.Contains(strings.Join(got, "\n"), "\x1b") {
		t.Fatalf("ASCII diff contains an escape sequence: %q", got)
	}
}

func TestWorkspaceInvalidationVisiblyStalesOpenDiff(t *testing.T) {
	m := testModel(t)
	d := &diffView{lines: []string{"diff --git a/main.go b/main.go", "+changed"}}
	m.full = d

	_, _ = m.Update(workspaceInvalidatedMsg{})
	if !d.stale {
		t.Fatal("active diff remained current after the workspace changed")
	}
	view := stripANSI(d.view(80, 8, darkTheme()))
	for _, want := range []string{"STALE", "workspace changed", "reopen /diff"} {
		if !strings.Contains(view, want) {
			t.Fatalf("stale diff omitted %q:\n%s", want, view)
		}
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if cmd == nil {
		t.Fatal("stale diff silently accepted scrolling")
	}
	result := cmd()
	if notice, ok := result.(noticeMsg); !ok || notice.level != "warn" || !strings.Contains(notice.text, "reopen /diff") {
		t.Fatalf("stale diff action = %#v, want refresh warning", result)
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if m.full != nil {
		t.Fatal("q did not close the stale diff")
	}
}

func TestWorkspaceInvalidationRejectsPendingDiffLoad(t *testing.T) {
	m := testModel(t)
	cmd := cmdDiff(m, "")
	generation := m.workspaceGeneration
	sessionID := currentSessionID(m)
	_, _ = m.Update(workspaceInvalidatedMsg{})

	_, follow := m.Update(diffLoadedMsg{
		generation: generation,
		sessionID:  sessionID,
		lines:      []string{"SHOULD-NOT-OPEN"},
	})
	if follow != nil || m.full != nil {
		t.Fatalf("pre-invalidation diff opened after the boundary: follow=%v full=%T", follow != nil, m.full)
	}
	_ = cmd // The real command may still be running; its immutable ticket is what this result mirrors.
}
