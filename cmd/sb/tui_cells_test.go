package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestCellHelpersRenderTabsAsPrintableCells(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{name: "leading", text: "\talpha"},
		{name: "after ascii", text: "abc\tdef"},
		{name: "after wide unicode", text: "界\t🙂e\u0301Z"},
		{name: "inside ansi style", text: "\x1b[31mred\t界🙂\x1b[0m"},
	}

	for _, test := range tests {
		for _, width := range []int{2, 3, 5, 8, 13} {
			t.Run(fmt.Sprintf("%s/width_%d", test.name, width), func(t *testing.T) {
				got := wrapCells(test.text, width)
				if strings.ContainsRune(got, '\t') {
					t.Fatalf("wrapped output retained a physical tab: %q", got)
				}
				for row, line := range strings.Split(got, "\n") {
					if cells := ansi.StringWidth(line); cells > width {
						t.Fatalf("row %d occupies %d cells at width %d: %q", row, cells, width, line)
					}
				}

				// Wrapping may add ANSI resets at row boundaries, but it must
				// preserve the visible text and the location of each tab marker.
				visible := strings.ReplaceAll(ansi.Strip(got), "\n", "")
				want := strings.ReplaceAll(ansi.Strip(test.text), "\t", `\t`)
				if visible != want {
					t.Fatalf("visible wrapped text = %q, want %q", visible, want)
				}
			})
		}
	}
}

func TestFitAndPadMeasureThePrintableTabMarker(t *testing.T) {
	styled := "\x1b[1m界\tab\x1b[0m"
	for _, width := range []int{1, 2, 3, 4, 7} {
		got := fitCells(styled, width)
		if strings.ContainsRune(got, '\t') {
			t.Fatalf("width %d fitted output retained a physical tab: %q", width, got)
		}
		if cells := ansi.StringWidth(got); cells > width {
			t.Fatalf("width %d fitted output occupies %d cells: %q", width, cells, got)
		}
	}

	got := padRight(styled, 9)
	if strings.ContainsRune(got, '\t') {
		t.Fatalf("padded output retained a physical tab: %q", got)
	}
	if cells := ansi.StringWidth(got); cells != 9 {
		t.Fatalf("padded output occupies %d cells, want 9: %q", cells, got)
	}
	if plain := ansi.Strip(got); !strings.Contains(plain, `界\tab`) {
		t.Fatalf("padded output lost the visible tab marker: %q", plain)
	}
}

func TestTranscriptTabsCannotCreateUntrackedPhysicalRows(t *testing.T) {
	for _, width := range []int{7, 11, 20} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			tr := testTranscript(t, width)
			tr.add(&entry{kind: kindUser, text: "a\t界🙂\tend"})
			tr.add(&entry{kind: kindAssistant, text: "\x1b[31mred\t界🙂\x1b[0m", live: true})
			tr.add(&entry{kind: kindThinking, text: "think\t界🙂"})
			tr.add(&entry{kind: kindTool, expanded: true, tool: toolEntry{
				name: "exec", desc: "tabbed", detail: "col\t界🙂\n\tlast", done: true,
			}})

			if len(tr.flat) == 0 {
				t.Fatal("fixture rendered no transcript rows")
			}
			for row, line := range tr.flat {
				if strings.ContainsRune(line, '\t') {
					t.Fatalf("flat row %d retained a physical tab: %q", row, line)
				}
				if cells := ansi.StringWidth(line); cells > width {
					t.Fatalf("flat row %d occupies %d cells at width %d: %q", row, cells, width, line)
				}
			}

			// Every flat row is now exactly one physical terminal row. If a
			// literal tab survived, the terminal could wrap it into an extra
			// row without the viewport or selection map accounting for it.
			const height = 6
			view := tr.view(height)
			rows := strings.Split(view, "\n")
			if len(rows) != height {
				t.Fatalf("viewport has %d logical rows, want %d: %q", len(rows), height, view)
			}
			for row, line := range rows {
				if strings.ContainsRune(line, '\t') || ansi.StringWidth(line) > width {
					t.Fatalf("viewport row %d can occupy more than one physical row: %q", row, line)
				}
			}
		})
	}
}
