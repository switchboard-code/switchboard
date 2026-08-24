package main

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	"github.com/switchboard-code/switchboard/internal/tools"
)

func narrowTranscriptFixture(t *testing.T, width int) (*transcript, *entry) {
	t.Helper()
	tr := testTranscript(t, width)
	long := "編集中🙂e\u0301 " + strings.Repeat("非常に長い内容", 12)

	tr.add(&entry{kind: kindUser, text: long})
	tr.add(&entry{kind: kindInfo, text: long + "\n    " + long})
	tr.add(&entry{kind: kindNotice, level: "warn", text: long})
	tr.add(&entry{kind: kindTool, expanded: true, tool: toolEntry{
		name: "mcp__法務🙂e\u0301__" + strings.Repeat("道具", 8),
		desc: long, done: true, failed: true, detail: long + "\n" + long,
	}})
	tr.add(&entry{kind: kindTodo, todos: []tools.TodoItem{
		{Text: long, Status: tools.TodoActive},
		{Text: long, Status: tools.TodoDone},
	}})
	live := tr.add(&entry{kind: kindAssistant, text: long, live: true})
	return tr, live
}

func assertTranscriptRowsFit(t *testing.T, tr *transcript, width int) {
	t.Helper()
	for row, line := range tr.flat {
		if !utf8.ValidString(line) {
			t.Fatalf("width %d row %d is not valid UTF-8: %q", width, row, line)
		}
		if cells := ansi.StringWidth(line); cells > width {
			t.Fatalf("width %d row %d occupies %d cells: %q", width, row, cells, line)
		}
	}
}

func TestTranscriptEntryKindsFitNarrowTerminalCells(t *testing.T) {
	for _, width := range []int{20, 31} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			tr, _ := narrowTranscriptFixture(t, width)
			assertTranscriptRowsFit(t, tr, width)

			plain := ansi.Strip(strings.Join(tr.flat, "\n"))
			for _, want := range []string{"編集中", "mcp__", "tasks", "△"} {
				if !strings.Contains(plain, want) {
					t.Errorf("narrow transcript lost %q:\n%s", want, plain)
				}
			}
		})
	}
}

func TestSuccessfulToolSummaryNeverCutsUnicodeMidRune(t *testing.T) {
	for _, width := range []int{80, 120} {
		tr := testTranscript(t, width)
		tr.add(&entry{kind: kindTool, tool: toolEntry{
			name: "shell", desc: "completed", done: true,
			detail: strings.Repeat("界", 34),
		}})
		if rendered := strings.Join(tr.flat, "\n"); !utf8.ValidString(rendered) {
			t.Fatalf("width %d tool summary cut invalid UTF-8: %q", width, rendered)
		}
		assertTranscriptRowsFit(t, tr, width)
	}
}

func TestNarrowTranscriptStreamingKeepsViewportAndSelectionMapStable(t *testing.T) {
	for _, width := range []int{20, 31} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			tr, live := narrowTranscriptFixture(t, width)
			const height = 8
			tr.view(height)
			tr.scrollBy(5)
			before := tr.view(height)

			tr.appendText(tr.indexOf(live), "\n"+strings.Repeat("追加🙂e\u0301", 40))
			if after := tr.view(height); after != before {
				t.Fatalf("streaming moved a narrow scrolled viewport:\nbefore:\n%s\nafter:\n%s", before, after)
			}
			assertTranscriptRowsFit(t, tr, width)

			start, end := tr.lineAt(1), tr.lineAt(height-2)
			if start < 0 || end <= start {
				t.Fatalf("visible row map = %d..%d, want a selectable span", start, end)
			}
			tr.beginSelect(start)
			tr.extendSelect(end)
			selected, lines := tr.selectionText()
			if lines == 0 || selected == "" || !utf8.ValidString(selected) {
				t.Fatalf("selection = %d lines %q, want valid copied text", lines, selected)
			}

			painted := tr.view(height)
			for row, line := range strings.Split(painted, "\n") {
				if cells := ansi.StringWidth(line); cells > width {
					t.Fatalf("selected viewport row %d occupies %d cells at width %d", row, cells, width)
				}
				flat := tr.lineAt(row)
				entry := tr.entryAt(row)
				if flat < 0 {
					if entry != -1 {
						t.Fatalf("padding row %d mapped to entry %d", row, entry)
					}
					continue
				}
				expected := -1
				for i := len(tr.starts) - 1; i >= 0; i-- {
					if tr.starts[i] <= flat {
						expected = i
						break
					}
				}
				if entry != expected {
					t.Fatalf("viewport row %d / flat row %d maps to entry %d, want %d", row, flat, entry, expected)
				}
			}
		})
	}
}

func TestNarrowTranscriptSelectionFollowsAsyncSpliceAheadOfIt(t *testing.T) {
	for _, width := range []int{20, 31} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			tr := testTranscript(t, width)
			tool := tr.add(&entry{kind: kindTool, tool: toolEntry{
				id: "early", name: "shell", desc: "running",
			}})
			for i := 0; i < 6; i++ {
				tr.add(&entry{kind: kindInfo, text: fmt.Sprintf("history %d", i)})
			}
			targetIndex := len(tr.entries)
			tr.add(&entry{kind: kindInfo, text: "keep this selected across the earlier async tool completion"})
			tr.add(&entry{kind: kindInfo, text: "tail one"})
			tr.add(&entry{kind: kindInfo, text: "tail two"})

			const height = 14
			tr.view(height)
			visibleStart := max(len(tr.flat)-tr.offset-height, 0)
			targetStart := tr.starts[targetIndex]
			targetEnd := tr.starts[targetIndex+1] - 1
			startRow, endRow := targetStart-visibleStart, targetEnd-visibleStart
			if startRow < 0 || endRow >= height {
				t.Fatalf("target rows %d..%d are outside viewport height %d", startRow, endRow, height)
			}
			tr.beginSelect(tr.lineAt(startRow))
			tr.extendSelect(tr.lineAt(endRow))
			beforeText, beforeLines := tr.selectionText()
			beforeView := ansi.Strip(tr.view(height))

			tool.tool.done = true
			tool.tool.failed = true
			tool.tool.detail = strings.Repeat("large async completion ", 40)
			tool.expanded = true
			tr.invalidate(0)

			afterText, afterLines := tr.selectionText()
			if afterText != beforeText || afterLines != beforeLines {
				t.Fatalf("selection moved across earlier splice:\nbefore (%d): %q\nafter  (%d): %q", beforeLines, beforeText, afterLines, afterText)
			}
			if afterView := ansi.Strip(tr.view(height)); afterView != beforeView {
				t.Fatalf("earlier splice moved the visible narrow viewport:\nbefore:\n%s\nafter:\n%s", beforeView, afterView)
			}
			if got := tr.lineAt(startRow); got != tr.selAnchor {
				t.Fatalf("mouse row %d maps to %d after splice, selection anchor is %d", startRow, got, tr.selAnchor)
			}
			assertTranscriptRowsFit(t, tr, width)
		})
	}
}
