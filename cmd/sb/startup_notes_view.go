package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// addStartupNoteReport is the bounded startup surface. Mandatory failures are
// always shown, while routine extension chatter shares a fixed highlight cap.
// The complete ordered record remains available in /doctor extensions.
func addStartupNoteReport(m *tuiModel, report startupNoteReport) {
	for _, note := range report.Highlights {
		note = startupHighlightForDisplay(note)
		note.text = fitStartupHighlight(note.text, startupHighlightMaxRunes)
		m.addNotice(note.level, note.text)
	}
	for _, line := range report.Summary {
		m.addInfo(line)
	}
}

// writeStartupNoteReport is the REPL counterpart. Keeping it here makes the
// two startup surfaces consume the same report instead of growing separate
// truncation and prioritization rules.
func writeStartupNoteReport(out *renderer, report startupNoteReport) {
	for _, note := range report.Highlights {
		note = startupHighlightForDisplay(note)
		note.text = fitStartupHighlight(note.text, startupHighlightMaxRunes)
		out.Notice(note.level, note.text)
	}
	for _, line := range report.Summary {
		out.line("  " + line)
	}
	out.flush()
}

// The general renderers have a small vocabulary: warn and error. Preserve
// mandatory startup severity inside that vocabulary and in the visible text,
// while leaving the exact original level untouched in Details.
func startupHighlightForDisplay(note mcpNote) mcpNote {
	level := strings.ToLower(strings.TrimSpace(note.level))
	switch level {
	case "warning":
		note.level = "warn"
	case "err":
		note.level = "error"
	case "fatal", "critical", "high", "required":
		note.level = "error"
		note.text = level + ": " + note.text
	default:
		if isHighSeverityStartupNote(note) && level != "error" {
			note.level = "error"
		}
	}
	return note
}

func fitStartupHighlight(text string, columns int) string {
	if columns < 2 || lipgloss.Width(text) <= columns {
		return text
	}
	runes := []rune(text)
	for len(runes) > 0 && lipgloss.Width(string(runes))+1 > columns {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

func writeStartupNoteDetails(out *renderer, report startupNoteReport) {
	for _, line := range startupNoteDetailLines(report) {
		out.line(line)
	}
	out.flush()
}

// startupNoteDetailLines renders every sanitized detail, in discovery order,
// including duplicates. No line or entry cap belongs here: this is the
// explicit drill-down promised by the bounded startup summary.
func startupNoteDetailLines(report startupNoteReport) []string {
	lines := []string{
		"extension startup diagnostics",
		"discovery order; duplicate reports retained",
	}
	if len(report.Details) == 0 {
		return append(lines, "", "no extension startup diagnostics were recorded")
	}

	groups := make([]string, 0, len(report.Groups))
	for _, group := range report.Groups {
		groups = append(groups, fmt.Sprintf("%s %d/%d", group.Category, group.Unique, group.Total))
	}
	sourceLabel := "sources (unique/total): "
	if report.Dropped > 0 {
		sourceLabel = "retained sources (unique/total): "
	}
	lines = append(lines, sourceLabel+strings.Join(groups, ", "), "")
	for i, note := range report.Details {
		level := strings.TrimSpace(note.level)
		if level == "" {
			level = "info"
		}
		lines = append(lines, fmt.Sprintf("%4d  %-8s %s", i+1, level, note.text))
	}
	return lines
}

// startupNotesView is deliberately a plain scrolling record. Diagnostics may
// contain paths and punctuation, so markdown or syntax highlighting would add
// interpretation to a surface whose job is to show the exact sanitized text.
type startupNotesView struct {
	lines       []string
	visual      []string
	visualWidth int
	offset      int
}

func newStartupNotesView(report startupNoteReport) *startupNotesView {
	return &startupNotesView{lines: startupNoteDetailLines(report)}
}

func (v *startupNotesView) key(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		return true, nil
	case "up", "k":
		v.scroll(-1)
	case "down", "j":
		v.scroll(1)
	case "pgup", "ctrl+u":
		v.scroll(-20)
	case "pgdown", "ctrl+d":
		v.scroll(20)
	case "g":
		v.offset = 0
	case "G":
		v.offset = len(v.visual)
		if v.offset == 0 {
			v.offset = len(v.lines)
		}
		v.scroll(0)
	}
	return false, nil
}

func (v *startupNotesView) mouse(msg tea.MouseMsg) tea.Cmd {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		v.scroll(-3)
	case tea.MouseButtonWheelDown:
		v.scroll(3)
	}
	return nil
}

func (v *startupNotesView) scroll(delta int) {
	v.offset += delta
	if v.offset < 0 {
		v.offset = 0
	}
}

func (v *startupNotesView) view(width, height int, th *theme) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	header := startupNotesHeader(width, th)
	if height == 1 {
		return header
	}
	footerRows := 0
	if height >= 3 {
		footerRows = 1
	}
	bodyHeight := height - 1 - footerRows
	visual := v.visualLines(width)
	if maximum := len(visual) - bodyHeight; v.offset > maximum {
		v.offset = maximum
	}
	if v.offset < 0 {
		v.offset = 0
	}

	end := min(v.offset+bodyHeight, len(visual))
	visible := append([]string(nil), visual[v.offset:end]...)
	for len(visible) < bodyHeight {
		visible = append(visible, "")
	}

	rows := append([]string{header}, visible...)
	if footerRows == 1 {
		footer := ""
		if len(visual) > bodyHeight {
			percent := min((v.offset+bodyHeight)*100/len(visual), 100)
			footer = th.faint.Render(workspaceFit(" "+itoa(percent)+"%", width))
		}
		rows = append(rows, footer)
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

func startupNotesHeader(width int, th *theme) string {
	title := " extension startup diagnostics"
	hint := "  ↑↓ scroll · pgup/pgdn · g/G · esc"
	if lipgloss.Width(title)+lipgloss.Width(hint) > width {
		hint = "  ↑↓ · esc"
	}
	if lipgloss.Width(title)+lipgloss.Width(hint) > width {
		hint = ""
		title = fitStartupHighlight(title, max(width, 2))
	}
	return fitCells(th.bold.Render(title)+th.faint.Render(hint), width)
}

func (v *startupNotesView) visualLines(width int) []string {
	width = max(width, 1)
	if v.visualWidth == width && v.visual != nil {
		return v.visual
	}
	var visual []string
	for _, line := range v.lines {
		if line == "" {
			visual = append(visual, "")
			continue
		}
		visual = append(visual, wrapStartupDetailLine(line, width)...)
	}
	v.visualWidth = width
	v.visual = visual
	return v.visual
}

// wrapStartupDetailLine hard-wraps without normalizing or dropping a byte of
// text. Diagnostics commonly contain a single long path, hash, or server ID;
// word-only wrapping would let those run past the viewport and make the tail
// effectively uninspectable.
func wrapStartupDetailLine(line string, width int) []string {
	if line == "" {
		return []string{""}
	}
	if width < 1 {
		width = 1
	}
	lines := strings.Split(wrapCells(line, width), "\n")
	for i := range lines {
		// A single CJK/emoji grapheme may be wider than a one-cell terminal.
		// It cannot be split, so render a bounded marker instead of allowing an
		// untracked physical wrap.
		lines[i] = fitCells(lines[i], width)
	}
	return lines
}

var _ fullscreen = (*startupNotesView)(nil)
