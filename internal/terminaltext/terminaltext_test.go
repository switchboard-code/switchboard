package terminaltext

import (
	"strings"
	"testing"
)

func TestEscapeMakesTerminalAndBidiControlsVisible(t *testing.T) {
	input := "ok\x1b[2J\x1b]0;spoof\x07\u009b\u202ereverse"
	got := Escape(input)
	for _, unsafe := range []string{"\x1b", "\x07", "\u009b", "\u202e"} {
		if strings.Contains(got, unsafe) {
			t.Fatalf("escaped text retained control %q: %q", unsafe, got)
		}
	}
	for _, visible := range []string{`\x1b`, `\x07`, `\u009b`, `\u202e`} {
		if !strings.Contains(got, visible) {
			t.Fatalf("escaped text omitted visible marker %q: %q", visible, got)
		}
	}
}

func TestDisplayPreservesLinesAndMakesTabsDeterministic(t *testing.T) {
	input := "first\tcolumn\nsecond\rOVERWRITE\x1b]52;c;Y2xpcGJvYXJk\x07\u202espoof"
	got := Display(input)
	if !strings.Contains(got, `first\tcolumn`+"\nsecond") {
		t.Fatalf("display lost ordinary layout: %q", got)
	}
	for _, unsafe := range []string{"\t", "\r", "\x1b", "\x07", "\u202e"} {
		if strings.Contains(got, unsafe) {
			t.Fatalf("display retained unsafe control %q: %q", unsafe, got)
		}
	}
	for _, visible := range []string{`\x0d`, `\x1b`, `\x07`, `\u202e`} {
		if !strings.Contains(got, visible) {
			t.Fatalf("display omitted visible marker %q: %q", visible, got)
		}
	}
}
