package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// The checks here are at the SGR level deliberately: the style config's
// background field can be dead under a different formatter, so trusting the
// config would test the wrong layer.
func TestMarkdownCarriesNoCodeBand(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	md := newMarkdown(80, true)

	lines := md.render("Some prose.\n\n```go\nfunc x() int { return 1 }\n```\n\nMore prose.")
	joined := strings.Join(lines, "\n")

	// 48;* is a background SGR; fenced code must not paint one. The gray
	// band was #373737 via 48;2;55;55;55 under truecolor and a 236-ish index
	// under 256 colors, so refuse any background at all in the block.
	if strings.Contains(joined, "\x1b[48;") || strings.Contains(joined, ";48;") {
		t.Fatalf("fenced code paints a background band:\n%q", joined)
	}
}

func TestMarkdownStartsAtTheGutter(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	md := newMarkdown(80, true)

	lines := md.render("plain paragraph")
	if len(lines) == 0 {
		t.Fatal("nothing rendered")
	}
	plain := stripANSI(lines[0])
	if strings.HasPrefix(plain, "  ") {
		t.Fatalf("document margin survived; the transcript owns the gutter: %q", plain)
	}
	if strings.TrimSpace(plain) == "" {
		t.Fatalf("leading blank line survived: %q", lines)
	}
}

func TestMarkdownAsciiProfileEmitsNoANSI(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(previous) })

	md := newMarkdown(80, true)
	lines := md.render("# Heading\n\nSome **bold** and `code`.\n\n```go\nfunc main() {}\n```")
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "\x1b") {
		t.Fatalf("ASCII markdown contains an escape sequence: %q", joined)
	}
	for _, want := range []string{"Heading", "Some ", "bold", "code", "func main() {}"} {
		if !strings.Contains(joined, want) {
			t.Errorf("ASCII markdown lost %q: %q", want, joined)
		}
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	inSeq := false
	for _, r := range s {
		switch {
		case inSeq:
			if r == 'm' {
				inSeq = false
			}
		case r == '\x1b':
			inSeq = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
