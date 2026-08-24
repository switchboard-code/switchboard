package main

import (
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/switchboard-code/switchboard/internal/terminaltext"
)

// padRight pads to terminal cells rather than bytes or runes. It deliberately
// does not truncate; callers choose whether losing content is appropriate.
func padRight(s string, width int) string {
	s = printableTabs(s)
	if missing := width - ansi.StringWidth(s); missing > 0 {
		return s + strings.Repeat(" ", missing)
	}
	return s
}

// fitCells bounds text without splitting ANSI sequences or grapheme clusters.
func fitCells(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = printableTabs(s)
	if ansi.StringWidth(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, "…")
}

// wrapCells wraps styled text at word boundaries without splitting escape
// sequences, wide characters, emoji, or combining grapheme clusters. A short
// critical token such as an executable or path moves intact to the next line;
// only a token wider than the whole dialog is hard-wrapped. Unlike fitCells it
// keeps all content, which is important for confirmation dialogs where
// truncation could hide the consequence being approved.
func wrapCells(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = printableTabs(s)
	soft := ansi.Wordwrap(s, width, "")
	var lines []string
	for _, line := range strings.Split(soft, "\n") {
		if ansi.StringWidth(line) > width {
			lines = append(lines, strings.Split(ansi.Hardwrap(line, width, true), "\n")...)
		} else {
			lines = append(lines, line)
		}
	}
	lines = independentSGRLines(lines, strings.Contains(s, "\x1b["))
	return strings.Join(lines, "\n")
}

// printableTabs is the last guard before text is measured in terminal cells.
// x/ansi intentionally gives a literal tab no width, but real terminals move
// to a configurable tab stop. A visible escape is lossless, deterministic,
// and matches the diff and turn-review surfaces. Rendering it here also
// protects trusted/styled callers that did not pass through terminaltext.
func printableTabs(s string) string {
	return strings.ReplaceAll(s, "\t", `\t`)
}

// independentSGRLines makes each physical row safe to render on its own.
// Viewports and compact dialogs routinely start or stop between wrapped rows;
// without an explicit resume/reset pair, color can disappear on continuation
// rows or leak into the composer and permission chrome below them.
func independentSGRLines(lines []string, styled bool) []string {
	if !styled {
		return lines
	}
	active := ""
	for i, line := range lines {
		rowStyle := active
		active = diffSGRState(active, line)
		lines[i] = ansi.ResetStyle + rowStyle + line + ansi.ResetStyle
	}
	return lines
}

// wrapCellsBounded wraps like wrapCells and caps the vertical space consumed
// by untrusted prose. The ellipsis is deliberate: silently dropping the tail
// of a question or permission explanation would make the dialog misleading.
func wrapCellsBounded(s string, width, maxLines int) string {
	if width <= 0 || maxLines <= 0 {
		return ""
	}
	lines := strings.Split(wrapCells(s, width), "\n")
	if len(lines) <= maxLines {
		return strings.Join(lines, "\n")
	}
	lines = lines[:maxLines]
	last := lines[len(lines)-1]
	if ansi.StringWidth(last) < width {
		last += "…"
	} else {
		// The extra printable cell forces Truncate to reserve a cell for the
		// marker while it continues scanning the string for trailing SGR resets.
		last = ansi.Truncate(last+"x", width, "…")
	}
	lines[len(lines)-1] = last
	out := strings.Join(lines, "\n")
	if strings.Contains(s, "\x1b") {
		// Hard wrapping can leave a style reset in a discarded row. Never let
		// that style leak into the next dialog line.
		out += "\x1b[0m"
	}
	return out
}

// safeTextInputView escapes control bytes in a rendering-only copy. The
// actual model retains the exact value for editing and submission.
func safeTextInputView(input textinput.Model) string {
	value := input.Value()
	escaped := terminaltext.Escape(value)
	if escaped == value {
		return input.View()
	}
	position := input.Position()
	prefix := []rune(value)
	if position < len(prefix) {
		prefix = prefix[:position]
	}
	copy := input
	copy.SetValue(escaped)
	copy.SetCursor(len([]rune(terminaltext.Escape(string(prefix)))))
	return copy.View()
}

// safeTextareaView renders a control-escaped copy while keeping the editable
// model byte-for-byte intact. Textarea is not safely shallow-copyable (its
// viewport and cache are pointers), so the display copy is rebuilt only when
// the value actually contains terminal or bidi controls.
func safeTextareaView(input textarea.Model, width int) string {
	value := input.Value()
	escaped := terminaltext.Display(value)
	if escaped == value {
		return input.View()
	}

	row := input.Line()
	lines := strings.Split(value, "\n")
	row = max(min(row, len(lines)-1), 0)
	info := input.LineInfo()
	col := min(info.StartColumn+info.ColumnOffset, len([]rune(lines[row])))
	prefix := string([]rune(lines[row])[:col])
	escapedCol := len([]rune(terminaltext.Display(prefix)))

	render := textarea.New()
	render.Prompt = input.Prompt
	render.Placeholder = input.Placeholder
	render.ShowLineNumbers = input.ShowLineNumbers
	render.EndOfBufferCharacter = input.EndOfBufferCharacter
	render.KeyMap = input.KeyMap
	render.FocusedStyle = input.FocusedStyle
	render.BlurredStyle = input.BlurredStyle
	render.CharLimit = 0
	render.MaxHeight = input.MaxHeight
	render.MaxWidth = input.MaxWidth
	render.SetWidth(max(width, 1))
	render.SetHeight(input.Height())
	render.SetValue(escaped)
	for render.Line() > row {
		render.CursorUp()
	}
	render.CursorStart()
	render.SetCursor(escapedCol)
	render.Cursor = input.Cursor
	wasFocused := input.Focused()
	render.Focus()
	// An otherwise inert update asks the widget to reposition its private
	// viewport around the reconstructed cursor.
	render, _ = render.Update(tea.KeyMsg{})
	if !wasFocused {
		render.Blur()
	}
	return render.View()
}

// dialogDimensions leaves the same one-cell gutter on either side as the
// input composer. Lip Gloss Width includes horizontal padding but excludes
// the border, so the content width is two cells narrower than the box width.
func dialogDimensions(width int) (boxWidth, contentWidth int) {
	boxWidth = max(width-4, 1)
	contentWidth = max(boxWidth-2, 1)
	return boxWidth, contentWidth
}
