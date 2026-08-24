package main

import (
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// markdown renders completed assistant blocks. It is deliberately only for
// completed blocks: re-running a full renderer on every stream delta is the
// standard source of long-session jank (§14). In-flight text goes through the
// plain wrap path and is re-rendered once, here, when the block completes.
type markdown struct {
	width int
	dark  bool
	// profile is the profile used to build r. Glamour 1.0 still emits text
	// attributes such as bold under its Ascii profile, so render strips those
	// controls at the boundary below.
	profile termenv.Profile
	r       *glamour.TermRenderer
}

func newMarkdown(width int, dark bool) *markdown {
	m := &markdown{width: width, dark: dark}
	m.rebuild()
	return m
}

func (m *markdown) rebuild() {
	width := m.width - 2
	if width < 20 {
		width = 20
	}
	m.profile = lipgloss.ColorProfile()
	r, err := glamour.NewTermRenderer(
		glamour.WithWordWrap(width),
		glamour.WithPreservedNewLines(),
		glamour.WithColorProfile(m.profile),
		styleFor(m.dark),
	)
	if err == nil {
		m.r = r
	}
}

// styleFor adapts glamour's stock styles to a transcript. Stock ships three
// defects for this use: a blank line before and after every render, a
// two-column margin on every paragraph, and a hardcoded #373737 band behind
// fenced code that matches no theme on earth. The transcript owns gaps and
// gutters, and code reads better as foreground-only syntax color over the
// page's own ground; inline code keeps its chip background, which is the one
// place a background earns its keep.
func styleFor(dark bool) glamour.TermRendererOption {
	cfg := styles.LightStyleConfig
	if dark {
		cfg = styles.DarkStyleConfig
	}
	cfg.Document.BlockPrefix = ""
	cfg.Document.BlockSuffix = ""
	zero := uint(0)
	cfg.Document.Margin = &zero
	if cfg.CodeBlock.Chroma != nil {
		chroma := *cfg.CodeBlock.Chroma
		chroma.Background.BackgroundColor = nil
		cfg.CodeBlock.Chroma = &chroma
	}
	return glamour.WithStyles(cfg)
}

func (m *markdown) setWidth(width int) {
	if width == m.width {
		return
	}
	m.width = width
	m.rebuild()
}

func (m *markdown) setDark(dark bool) {
	if dark == m.dark {
		return
	}
	m.dark = dark
	m.rebuild()
}

// render converts markdown to styled lines with trailing blank lines trimmed.
// On any renderer failure the text comes back wrapped and plain: a rendering
// bug must never eat model output.
func (m *markdown) render(text string) []string {
	if m.r != nil {
		if out, err := m.r.Render(text); err == nil {
			if m.profile == termenv.Ascii {
				out = ansi.Strip(out)
			}
			return trimBlankLines(strings.Split(strings.TrimRight(out, "\n"), "\n"))
		}
	}
	return wrapPlain(text, m.width)
}

// wrapPlain is the in-flight fast path: word wrap and nothing else.
func wrapPlain(text string, width int) []string {
	width = max(width, 1)
	var lines []string
	for para := range strings.Lines(text) {
		para = strings.TrimRight(para, "\n")
		if para == "" {
			lines = append(lines, "")
			continue
		}
		// The transcript's viewport is measured in terminal cells. Wordwrap's
		// rune-oriented width let CJK, emoji, combining marks, and long tokens
		// produce rows wider than the terminal, which in turn invalidated the
		// scroll and mouse-selection row map. wrapCells preserves graphemes and
		// ANSI state, then hard-wraps the tokens that have no breakpoint.
		lines = append(lines, strings.Split(wrapCells(para, width), "\n")...)
	}
	return trimBlankLines(lines)
}

func trimBlankLines(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	// Leading blanks too: the transcript owns the gaps between blocks, and a
	// renderer that brings its own margin doubles them.
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	return lines
}
