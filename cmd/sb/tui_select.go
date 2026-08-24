package main

// Drag-to-select in the transcript. With the mouse reporting to sb, a plain
// drag no longer reaches the terminal's own selection, so the transcript
// grows one of its: press anchors a line, motion extends it, release copies
// the dragged lines as plain text and clears the highlight. A press that
// never left its line is a click, not a selection — the expand toggle keeps
// its gesture.

import (
	"encoding/base64"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// clipboardMsg reports a finished copy, so the transcript can say how much
// went.
type clipboardMsg struct{ lines int }

// lineAt maps a viewport row from the last view to its flat line index, -1
// outside the content. The math mirrors view's own window, the way entryAt's
// does, so the click, the selection, and what is drawn cannot disagree.
func (t *transcript) lineAt(row int) int {
	if t.height <= 0 || row < 0 || row >= t.height {
		return -1
	}
	end := len(t.flat) - t.offset
	start := end - t.height
	if start < 0 {
		start = 0
	}
	line := start + row
	if line >= end || line >= len(t.flat) {
		return -1
	}
	return line
}

func (t *transcript) beginSelect(line int) {
	t.selOn = line >= 0
	t.selMoved = false
	t.selAnchor, t.selEnd = line, line
}

func (t *transcript) extendSelect(line int) {
	if !t.selOn || line < 0 {
		return
	}
	if line != t.selEnd {
		t.selMoved = true
		t.selEnd = line
	}
}

func (t *transcript) clearSelect() { t.selOn = false }

// selectionText is the dragged span as plain text: styling stripped, ragged
// right edges trimmed, blank edges dropped. The count is of the lines taken.
func (t *transcript) selectionText() (string, int) {
	if !t.selOn || !t.selMoved {
		return "", 0
	}
	lo, hi := t.selAnchor, t.selEnd
	if lo > hi {
		lo, hi = hi, lo
	}
	if hi >= len(t.flat) {
		hi = len(t.flat) - 1
	}
	if lo > hi || lo < 0 {
		return "", 0
	}
	lines := make([]string, 0, hi-lo+1)
	for _, line := range t.flat[lo : hi+1] {
		lines = append(lines, strings.TrimRight(stripStyling(line), " "))
	}
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return "", 0
	}
	return strings.Join(lines, "\n"), len(lines)
}

// stripStyling removes the CSI (SGR) sequences a rendered line carries. The
// searchable mirror cannot answer here: it is lowercased for matching, and a
// copy must keep its case.
func stripStyling(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && !((s[j] >= 'a' && s[j] <= 'z') || (s[j] >= 'A' && s[j] <= 'Z')) {
				j++
			}
			i = j // the letter terminates the sequence; the loop's ++ steps past it
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// copySelection copies the dragged span and reports it. The text reaches the
// clipboard two ways: OSC 52 for the terminals that take it, written in one
// shot on the channel the bell already uses, and pbcopy on macOS for the
// terminal that takes no escape.
func (m *tuiModel) copySelection() tea.Cmd {
	text, n := m.tr.selectionText()
	if n == 0 {
		return nil
	}
	return func() tea.Msg {
		if m.clipboardWrite != nil {
			m.clipboardWrite(text)
		}
		return clipboardMsg{lines: n}
	}
}

func writeClipboard(text string) {
	_ = writeClipboardAll(text)
}

// writeClipboardAll is the one transcript-egress boundary for drag selection,
// /copy, and workspace locations. OSC 52 covers terminal-owned clipboards;
// nativeClipboardWrite adds a platform clipboard where it can do so without a
// repository-controlled helper lookup.
func writeClipboardAll(text string) error {
	_, oscErr := os.Stderr.WriteString("\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\a")
	native, nativeErr := nativeClipboardWrite(text)
	if native {
		return nativeErr
	}
	return oscErr
}
