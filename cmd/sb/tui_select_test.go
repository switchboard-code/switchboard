package main

import (
	"strings"
	"testing"
)

func selectTranscript(t *testing.T) *transcript {
	t.Helper()
	tr := testTranscript(t, 80)
	tr.add(&entry{kind: kindUser, text: "first line"})
	tr.add(&entry{kind: kindUser, text: "second line"})
	tr.add(&entry{kind: kindUser, text: "third line"})
	return tr
}

// A row on screen maps to the flat line the view is showing there, scrolled
// or not, or the click and the highlight would land on different text.
func TestLineAtMirrorsTheVisibleWindow(t *testing.T) {
	tr := selectTranscript(t)
	for i := 0; i < 30; i++ {
		tr.add(&entry{kind: kindInfo, text: "filler"})
	}
	tr.view(10) // establishes the viewport height

	total := len(tr.flat)
	if got := tr.lineAt(9); got != total-1 {
		t.Fatalf("bottom row maps to flat %d, want the last line %d", got, total-1)
	}
	tr.scrollBy(4)
	top := tr.lineAt(0)
	if top != total-4-10 {
		t.Fatalf("scrolled top row maps to flat %d, want %d", top, total-4-10)
	}
	if got := tr.lineAt(-1); got != -1 {
		t.Fatalf("a row above the viewport maps to %d, want -1", got)
	}
}

// A drag selects whole lines and the copy is plain text with its case kept;
// a press that never moved is a click, not a selection.
func TestDragSelectsLinesForCopy(t *testing.T) {
	tr := selectTranscript(t)
	tr.view(10)

	tr.beginSelect(tr.lineAt(0))
	tr.extendSelect(tr.lineAt(4))
	text, n := tr.selectionText()
	// User cards carry a blank line of air between them, so five rows hold
	// the three prompts.
	if n != 5 || !strings.Contains(text, "first line") || !strings.Contains(text, "second line") || !strings.Contains(text, "third line") {
		t.Fatalf("selection = %q (%d lines), want the three dragged lines", text, n)
	}
	if strings.Contains(text, "\x1b") {
		t.Fatalf("the copy carries styling: %q", text)
	}

	tr.beginSelect(tr.lineAt(1))
	if _, n := tr.selectionText(); n != 0 {
		t.Fatalf("a motionless press selected %d lines, want none — that is a click", n)
	}
}

// The highlight paints on a copy: the render cache stays clean for the next
// frame, which is the same rule the search marks keep.
func TestSelectionPaintLeavesTheRenderCacheAlone(t *testing.T) {
	tr := selectTranscript(t)
	tr.view(10)
	tr.beginSelect(tr.lineAt(0))
	tr.extendSelect(tr.lineAt(1))

	painted := tr.view(10)
	if !strings.Contains(painted, "\x1b[7m") {
		t.Fatal("the selection did not paint:\n" + painted)
	}
	if strings.Contains(strings.Join(tr.flat, "\n"), "\x1b[7m") {
		t.Fatal("the selection leaked into the flat render cache")
	}
	tr.clearSelect()
	if strings.Contains(tr.view(10), "\x1b[7m") {
		t.Fatal("the highlight survived its release")
	}
}

// A release after a drag copies and reports the count; a release after a
// bare click keeps the expand toggle its gesture.
func TestReleaseCopiesOnlyADrag(t *testing.T) {
	m := testModel(t)
	m.tr = selectTranscript(t)
	m.tr.view(10)

	m.tr.beginSelect(m.tr.lineAt(0))
	m.tr.extendSelect(m.tr.lineAt(4))
	cmd := m.copySelection()
	if cmd == nil {
		t.Fatal("a drag produced no copy command")
	}
	msg, ok := cmd().(clipboardMsg)
	if !ok || msg.lines != 5 {
		t.Fatalf("copy reported %#v, want 5 lines", msg)
	}
}
