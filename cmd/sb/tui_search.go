package main

// Transcript search, ctrl+f. The TUI runs on the alternate screen, so the
// terminal's own scrollback search cannot see the conversation — the one
// place in a terminal where /find's answer lives out of reach. This is the
// missing half: incremental search over the rendered transcript, newest
// match first, the page margin carrying the markers. /find searches what
// other sessions said; ctrl+f searches what this one is saying.

import (
	"fmt"
	"regexp"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/terminaltext"
)

// sgr strips ANSI style sequences, so matching runs over what the reader
// reads rather than the escape codes around it.
var sgr = regexp.MustCompile("\x1b\\[[0-9;]*m")

func plainLine(s string) string { return sgr.ReplaceAllString(s, "") }

func (m *tuiModel) startTranscriptSearch() {
	m.trSearch = true
	m.trQuery = ""
	m.trMatches = nil
	m.trMatch = -1
}

// transcriptSearchKey handles one keypress while the search owns the
// keyboard. Enter keeps the scroll where the search left it; esc does too,
// because the point of searching was to get there.
func (m *tuiModel) transcriptSearchKey(msg tea.KeyMsg) {
	switch msg.String() {
	case "esc", "ctrl+c", "enter":
		m.trSearch = false
		m.tr.marks = nil
		return
	case "up", "ctrl+f":
		if m.trMatch+1 < len(m.trMatches) {
			m.trMatch++
			m.jumpToMatch()
		}
		return
	case "down":
		if m.trMatch > 0 {
			m.trMatch--
			m.jumpToMatch()
		}
		return
	case "backspace":
		if m.trQuery != "" {
			runes := []rune(m.trQuery)
			m.trQuery = string(runes[:len(runes)-1])
		}
		m.rescanTranscript()
		return
	}
	if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
		if msg.Type == tea.KeySpace {
			m.trQuery += " "
		} else {
			m.trQuery += string(msg.Runes)
		}
		m.rescanTranscript()
	}
	// Everything else is swallowed; the search owns the keyboard.
}

// rescanTranscript recomputes the match list, newest first, and lands on
// the newest match: a search usually wants the latest mention, and up
// walks toward the beginning from there.
func (m *tuiModel) rescanTranscript() {
	m.trMatches = nil
	m.trMatch = -1
	query := strings.ToLower(m.trQuery)
	if query == "" {
		m.tr.marks = nil
		return
	}
	for i := len(m.tr.searchable) - 1; i >= 0; i-- {
		if strings.Contains(m.tr.searchable[i], query) {
			m.trMatches = append(m.trMatches, i)
		}
	}
	if len(m.trMatches) > 0 {
		m.trMatch = 0
		m.jumpToMatch()
	} else {
		m.tr.marks = nil
	}
}

// jumpToMatch scrolls the current match into view and repaints the margin
// markers: every match wears a faint bar, the current one the accent.
func (m *tuiModel) jumpToMatch() {
	if m.trMatch < 0 || m.trMatch >= len(m.trMatches) {
		return
	}
	current := m.trMatches[m.trMatch]
	m.tr.scrollTo(current)
	marks := make(map[int]string, len(m.trMatches))
	for _, i := range m.trMatches {
		marks[i] = m.th.faint.Render("▌")
	}
	marks[current] = m.th.accent.Render("▌")
	m.tr.marks = marks
}

func (m *tuiModel) transcriptSearchView() string {
	line := "(search) " + terminaltext.Escape(m.trQuery)
	switch {
	case len(m.trMatches) > 0:
		line += m.th.dim.Render(fmt.Sprintf("  %d/%d", m.trMatch+1, len(m.trMatches)))
		line += m.th.faint.Render("  ↑ older · ↓ newer · esc closes here")
	case m.trQuery != "":
		line += m.th.dim.Render("  no match")
	default:
		line += m.th.faint.Render("  type to search this session's transcript")
	}
	return m.th.accent.Render(line)
}
