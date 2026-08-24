package main

import (
	"strings"
	"testing"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func typeSearch(m *tuiModel, text string) {
	for _, r := range text {
		m.transcriptSearchKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func TestTranscriptSearchMarkerAtWidthOneIsValidAndBounded(t *testing.T) {
	m := testModel(t)
	m.tr = newTranscript(1, m.th, newMarkdown(1, true))
	m.tr.add(&entry{kind: kindInfo, text: "✗ needle"})
	m.tr.view(1)
	m.startTranscriptSearch()
	typeSearch(m, "needle")

	view := m.tr.view(1)
	if !utf8.ValidString(view) {
		t.Fatalf("width-one marker corrupted UTF-8: %q", view)
	}
	if cells := ansi.StringWidth(view); cells > 1 {
		t.Fatalf("width-one marker occupies %d cells: %q", cells, view)
	}
}

func TestTranscriptSearchScrollToShowsMatchAtHeightOne(t *testing.T) {
	m := testModel(t)
	m.tr.reset()
	m.tr.add(&entry{kind: kindInfo, text: "zero"})
	m.tr.add(&entry{kind: kindInfo, text: "unique needle"})
	m.tr.add(&entry{kind: kindInfo, text: "tail"})
	m.tr.view(1)
	m.startTranscriptSearch()
	typeSearch(m, "unique needle")

	if got := ansi.Strip(m.tr.view(1)); !strings.Contains(got, "unique") {
		t.Fatalf("one-row search landed above its match: %q", got)
	}
}

func TestTranscriptSearchRescansFlatCoordinatesAfterResize(t *testing.T) {
	m := testModel(t)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 12})
	m.tr.reset()
	m.tr.add(&entry{kind: kindInfo, text: strings.Repeat("long prelude ", 15)})
	m.tr.add(&entry{kind: kindInfo, text: "UNIQUE-NEEDLE at the target"})
	m.startTranscriptSearch()
	typeSearch(m, "unique-needle")

	m.Update(tea.WindowSizeMsg{Width: 20, Height: 12})
	if len(m.trMatches) == 0 {
		t.Fatal("resize lost the active transcript search")
	}
	for _, line := range m.trMatches {
		if line < 0 || line >= len(m.tr.searchable) || !strings.Contains(m.tr.searchable[line], "unique-needle") {
			t.Fatalf("resize left stale match %d in %d rows: matches=%v", line, len(m.tr.searchable), m.trMatches)
		}
	}
}

// ctrl+f searches what this session is saying. The alternate screen hides
// the transcript from the terminal's own search, so the TUI carries its
// own: newest match first, up walks older, the margin carries the marks.
func TestTranscriptSearchFindsNewestFirstAndMarksTheMargin(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	m := testModel(t)
	m.tr.add(&entry{kind: kindInfo, text: "the runner race begins here"})
	m.tr.add(&entry{kind: kindInfo, text: "nothing relevant"})
	m.tr.add(&entry{kind: kindInfo, text: "the Runner race ends here"})
	m.tr.view(10)

	m.startTranscriptSearch()
	typeSearch(m, "runner race")

	if len(m.trMatches) != 2 {
		t.Fatalf("found %d matches, want 2 (case-insensitive)", len(m.trMatches))
	}
	if m.trMatch != 0 {
		t.Fatalf("search landed on match %d, want the newest", m.trMatch)
	}
	if m.trMatches[0] < m.trMatches[1] {
		t.Fatalf("matches are not newest-first: %v", m.trMatches)
	}
	if len(m.tr.marks) != 2 {
		t.Fatalf("the margin carries %d marks, want 2", len(m.tr.marks))
	}

	// The view paints the marker into the margin cell without touching the
	// flat buffer.
	view := m.tr.view(10)
	if !strings.Contains(view, "▌") {
		t.Fatalf("no margin marker in the view:\n%s", view)
	}
	for _, l := range m.tr.flat {
		if strings.HasPrefix(l, "\x1b") && strings.Contains(l, "▌") && strings.Contains(plainLine(l), "runner race begins") {
			t.Fatalf("search state leaked into the flat buffer: %q", l)
		}
	}

	// Up walks older and stops at the oldest; down walks back and stops at
	// the newest.
	m.transcriptSearchKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.trMatch != 1 {
		t.Fatalf("up did not walk older: at %d", m.trMatch)
	}
	m.transcriptSearchKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.trMatch != 1 {
		t.Fatalf("up walked past the oldest match: at %d", m.trMatch)
	}
	m.transcriptSearchKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.trMatch != 0 {
		t.Fatalf("down did not walk newer: at %d", m.trMatch)
	}

	// Esc closes the search, clears the marks, and stays where the search
	// led: the point of searching was to get there.
	before := m.tr.offset
	m.transcriptSearchKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.trSearch || m.tr.marks != nil {
		t.Fatal("esc did not close the search and clear the marks")
	}
	if m.tr.offset != before {
		t.Fatal("closing the search moved the scroll away from the match")
	}
}

func TestTranscriptSearchWithNoMatchSaysSo(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	m := testModel(t)
	m.tr.add(&entry{kind: kindInfo, text: "hello"})
	m.startTranscriptSearch()
	typeSearch(m, "zeppelin")
	if len(m.trMatches) != 0 || m.tr.marks != nil {
		t.Fatal("a query with no match kept matches or marks")
	}
	if view := plainLine(m.transcriptSearchView()); !strings.Contains(view, "no match") {
		t.Fatalf("the bar does not say no match: %q", view)
	}
}

// The search rescans per keystroke, so its cost on a long session is a
// latency the fingers feel; it gets the same benchmark discipline as the
// view. Compared runs answer whether a rescan stays under the input
// budget or the plain lines need caching.
func benchModelForSearch(turns int) *tuiModel {
	th := darkTheme()
	m := &tuiModel{th: th}
	m.tr = newTranscript(100, th, newMarkdown(100, true))
	for i := 0; i < turns; i++ {
		m.tr.add(&entry{kind: kindUser, text: "a question that fills a line or two of the terminal"})
		m.tr.add(&entry{kind: kindAssistant,
			text: "an answer with a needle in it and some **markdown**\n\n```go\ncode := true\n```\n"})
	}
	m.tr.view(40)
	return m
}

func benchSearch(b *testing.B, turns int) {
	m := benchModelForSearch(turns)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.trQuery = "needle"
		m.rescanTranscript()
	}
}

func BenchmarkTranscriptSearch50Turns(b *testing.B)  { benchSearch(b, 50) }
func BenchmarkTranscriptSearch500Turns(b *testing.B) { benchSearch(b, 500) }
