package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/switchboard-code/switchboard/internal/terminaltext"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// The transcript is the scrollback. Entries render to styled lines once and
// are cached per width, so repainting is a slice over a flat line buffer and
// never a re-render (§14: virtualized scrollback, diffs rendered once and
// cached). Only the entry currently streaming re-renders, and that one goes
// through the plain fast path.

type entryKind int

const (
	kindUser entryKind = iota
	kindAssistant
	kindThinking
	kindTool
	kindNotice
	kindRoute
	kindInfo
	kindTodo
	// kindRaw holds pre-styled lines rendered verbatim, for the banner: the
	// one place composition is done by the builder, not the renderer.
	kindRaw
)

type toolEntry struct {
	id     string
	name   string
	desc   string
	done   bool
	failed bool
	took   time.Duration
	detail string
}

type entry struct {
	kind entryKind
	text string // user, assistant, thinking, notice, info

	live bool // still streaming; render with the fast path, never the cache

	tool toolEntry

	level        string   // notice level
	rail         bool     // a "done" notice closing a tool rail draws └
	routeSummary string   // collapsed route line
	routeLines   []string // the full decision record
	todos        []tools.TodoItem
	expanded     bool

	// padTop marks an entry that opens with a blank line: a turn boundary
	// (user card, first prose after rails) breathes. Decided once, at add
	// time, when everything before it is final.
	padTop bool

	// rank is the ladder position this entry happened on, captured at
	// creation so a t1-era line keeps t1's color after an escalation; -1
	// means the entry has no rung association and renders neutral.
	rank int

	cache map[int][]string
}

type transcript struct {
	entries []*entry
	starts  []int // flat offset where each entry's lines begin
	flat    []string

	// searchable mirrors flat line for line, stripped of styling and
	// lowercased: what ctrl+f matches against. It is maintained in the
	// same splices that maintain flat, because a cache with its own
	// update path is a cache that drifts - and it exists because
	// stripping per keystroke put a 500-turn rescan past the §14 input
	// budget.
	searchable []string

	width int
	th    *theme
	md    *markdown

	offset int // lines scrolled up from the bottom
	height int // the viewport view last drew, for the scroll clamp

	// A width change rewraps flat rows. While scrolled back, reflowLine keeps
	// one semantic row at the same screen position until view supplies the new
	// viewport height; offset alone cannot do that because it counts wrapped
	// rows from the bottom.
	reflowLine      int
	reflowScreenRow int
	reflowPending   bool

	// marks overlays the one-cell page margin: flat line index to a
	// pre-rendered single-cell marker. The search owns the map; the
	// transcript only paints it, on a copy, so the flat buffer never
	// carries search state.
	marks map[int]string

	// sel is a drag selection in flat line indices, painted on a copy at view
	// time for the same reason as the marks: flat is the render cache and
	// carries no interaction state. selMoved distinguishes a drag from a
	// click that never left its line.
	selAnchor int
	selEnd    int
	selOn     bool
	selMoved  bool

	// onChange lets the owning model refresh flat-row interaction state after
	// asynchronous streaming/tool splices. The transcript remains usable on
	// its own in tests; nil means no owner-side state is active.
	onChange func()
}

func newTranscript(width int, th *theme, md *markdown) *transcript {
	return &transcript{width: width, th: th, md: md}
}

func (t *transcript) add(e *entry) *entry {
	// A user card or the turn's first prose opens with air. The decision is
	// safe to bake into the cache because everything before this entry is
	// already final when it arrives.
	if (e.kind == kindUser || e.kind == kindAssistant) && len(t.flat) > 0 && t.flat[len(t.flat)-1] != "" {
		e.padTop = true
	}
	t.entries = append(t.entries, e)
	t.starts = append(t.starts, len(t.flat))
	t.invalidate(len(t.entries) - 1)
	return e
}

func (t *transcript) last() *entry {
	if len(t.entries) == 0 {
		return nil
	}
	return t.entries[len(t.entries)-1]
}

// appendText extends an entry's raw text and re-renders just that entry.
func (t *transcript) appendText(i int, s string) {
	t.entries[i].text += s
	t.invalidate(i)
}

func (t *transcript) indexOf(e *entry) int {
	for i, x := range t.entries {
		if x == e {
			return i
		}
	}
	return -1
}

// finalize flips a streaming entry to completed, which re-renders it once
// through glamour.
func (t *transcript) finalize(e *entry) {
	if e == nil || !e.live {
		return
	}
	e.live = false
	if i := t.indexOf(e); i >= 0 {
		t.invalidate(i)
	}
}

func (t *transcript) finalizeAll() {
	for i, e := range t.entries {
		if e.live {
			e.live = false
			t.invalidate(i)
		}
	}
}

// invalidate re-renders entry i and splices its lines into the flat buffer.
func (t *transcript) invalidate(i int) {
	e := t.entries[i]
	// Expansion and late tool/shell completion mutate an existing entry. Its
	// cached collapsed rendering no longer describes the entry we are about to
	// splice back into the transcript.
	e.cache = nil
	lines := t.render(e)
	oldStart := t.starts[i]
	oldEnd := len(t.flat)
	if i+1 < len(t.entries) {
		oldEnd = t.starts[i+1]
	}
	// Once somebody scrolls back, new output must not pull the viewport toward
	// the live tail. Find the first line this splice actually changes rather
	// than using the entry boundary: a streaming entry can be much taller than
	// the viewport while only its last wrapped line is changing.
	changeStart := oldStart
	oldLines := t.flat[oldStart:oldEnd]
	for changeStart-oldStart < len(oldLines) && changeStart-oldStart < len(lines) &&
		oldLines[changeStart-oldStart] == lines[changeStart-oldStart] {
		changeStart++
	}
	viewStart := 0
	if t.height > 0 {
		viewEnd := len(t.flat) - t.offset
		viewStart = max(viewEnd-t.height, 0)
	}
	plain := make([]string, len(lines))
	for j, l := range lines {
		plain[j] = strings.ToLower(plainLine(l))
	}
	delta := len(lines) - (oldEnd - oldStart)
	if delta == 0 {
		copy(t.flat[oldStart:], lines)
		copy(t.searchable[oldStart:], plain)
		t.changed()
		return
	}
	// A mouse drag is expressed in flat-row coordinates. Tool completions and
	// expansion can splice rows ahead of an in-progress drag, so carry both
	// endpoints across the splice before repainting. Otherwise a release that
	// races an async completion copies whatever inherited the old row numbers,
	// rather than the lines the user actually selected.
	if t.selOn {
		t.selAnchor = remapFlatIndex(t.selAnchor, oldStart, oldEnd, len(lines))
		t.selEnd = remapFlatIndex(t.selEnd, oldStart, oldEnd, len(lines))
	}
	if t.reflowPending {
		t.reflowLine = remapFlatIndex(t.reflowLine, oldStart, oldEnd, len(lines))
	}
	tail := append([]string(nil), t.flat[oldEnd:]...)
	flat := append(t.flat[:oldStart], lines...)
	t.flat = append(flat, tail...)
	plainTail := append([]string(nil), t.searchable[oldEnd:]...)
	searchable := append(t.searchable[:oldStart], plain...)
	t.searchable = append(searchable, plainTail...)
	for j := i + 1; j < len(t.starts); j++ {
		t.starts[j] += delta
	}
	if t.offset > 0 && changeStart >= viewStart {
		// offset is measured from the bottom, so absorbing the splice delta
		// keeps the same absolute rows under the viewport. Changes wholly above
		// the viewport instead shift those rows naturally and need no adjustment.
		t.offset += delta
		t.clampOffset()
	}
	t.changed()
}

func (t *transcript) changed() {
	if t.onChange != nil {
		t.onChange()
	}
}

// remapFlatIndex carries one row coordinate through replacement of
// [oldStart, oldEnd) by newLen rows. Coordinates in the replaced entry keep
// their relative row where possible; coordinates after it move by the splice
// delta.
func remapFlatIndex(index, oldStart, oldEnd, newLen int) int {
	if index < oldStart {
		return index
	}
	if index >= oldEnd {
		return index + newLen - (oldEnd - oldStart)
	}
	if newLen <= 0 {
		return oldStart - 1
	}
	return oldStart + min(index-oldStart, newLen-1)
}

func (t *transcript) render(e *entry) []string {
	if !e.live {
		if lines, ok := e.cache[t.width]; ok {
			return lines
		}
	}
	lines := t.composed(e)
	if !e.live {
		if e.cache == nil {
			e.cache = map[int][]string{}
		}
		e.cache[t.width] = lines
	}
	return lines
}

// composed is renderUncached plus the page's composition rules, applied in
// the one place every flat rebuild goes through: a one-cell left margin so
// content never presses against the terminal edge, and a breathing line
// after each block. Tool and thinking entries stay tight — the rail groups
// them — and blanks do not stack.
func (t *transcript) composed(e *entry) []string {
	// Flat rows are the viewport's unit for scrolling, clicking, selection,
	// and search. Letting the terminal wrap one behind our back makes all four
	// maps wrong, so every renderer passes through one final cell-aware bound.
	// Renderers still choose their own semantic wrapping; this guard protects
	// the invariant from long unbroken tool names/output and future entry kinds.
	contentWidth := max(t.width-1, 1)
	lines := boundTranscriptLines(t.renderUncached(e), contentWidth)
	margin := " "
	if t.width <= 1 {
		margin = ""
	}
	for i, l := range lines {
		if l != "" {
			lines[i] = margin + l
		}
	}
	if e.padTop {
		lines = append([]string{""}, lines...)
	}
	switch e.kind {
	case kindTool, kindThinking, kindInfo:
		return lines
	}
	// The gap is earned, not fixed: multi-line blocks and user turns breathe,
	// while consecutive one-line notices and route lines pack tight the way
	// the tool rail does. Density around chatter, air around substance.
	if e.kind != kindUser && len(lines) <= 1 {
		return lines
	}
	if len(lines) > 0 && lines[len(lines)-1] != "" {
		lines = append(lines, "")
	}
	return lines
}

func (t *transcript) renderUncached(e *entry) []string {
	w := t.width
	switch e.kind {
	case kindUser:
		return t.renderUser(terminaltext.Display(e.text), w)
	case kindAssistant:
		text := terminaltext.Display(e.text)
		if e.live {
			return wrapPlain(text, max(w-1, 1))
		}
		return t.md.render(text)
	case kindThinking:
		lines := wrapPlain(terminaltext.Display(e.text), max(w-1, 1))
		for i, l := range lines {
			lines[i] = t.th.thinking.Render(l)
		}
		return lines
	case kindTool:
		return t.renderTool(&e.tool, e.expanded, e.rank, w)
	case kindTodo:
		return t.renderTodo(e.todos, e.rank, w)
	case kindNotice:
		return t.renderNotice(e, w)
	case kindRoute:
		return t.renderRoute(e, w)
	case kindRaw:
		// kindRaw is the pre-styled opening banner. It is built before Bubble
		// Tea reports the real terminal size, so fitting belongs here where a
		// resize invalidates and re-renders the width-keyed entry cache. Leave
		// one cell for composed's page margin.
		inner := max(w-1, 0)
		lines := strings.Split(e.text, "\n")
		for i, line := range lines {
			lines[i] = fitCells(line, inner)
		}
		return lines
	default: // kindInfo
		return t.renderInfo(terminaltext.Display(e.text), w)
	}
}

// renderInfo draws what a command answered.
//
// A command's output is not conversation and should not read as it: /doctor,
// /tasks, and /context were dim prose in the same column as the model's
// replies, so a screen of them looked like the session talking to itself.
// This is the transcript's own card language turned to the side that is the
// tool rather than the person — a rail down the left, the first line as the
// heading commands already write, and the body indented under it.
func (t *transcript) renderInfo(text string, w int) []string {
	// The rail costs exactly what it draws. Commands lay their output out in
	// columns, and every column reserved here is one the command's own
	// alignment loses, so nothing is taken beyond the two cells of rail and
	// the page margin the transcript adds.
	inner := max(w-3, 1)
	rail := t.th.faint.Render("│ ")

	heading, body, _ := strings.Cut(text, "\n")
	headingLines := wrapPlain(heading, inner)
	if len(headingLines) == 0 {
		headingLines = []string{""}
	}
	lines := make([]string, 0, len(headingLines)+2)
	for i, line := range headingLines {
		lead := rail
		if i == 0 {
			lead = t.th.faint.Render("╭ ")
		}
		lines = append(lines, lead+t.th.bold.Render(line))
	}
	for _, paragraph := range strings.Split(body, "\n") {
		if strings.TrimSpace(paragraph) == "" {
			lines = append(lines, rail)
			continue
		}
		// Leading indentation is meaningful in these blocks — commands lay
		// out columns with it — so it is preserved and the wrap applies to
		// what is left.
		indent := paragraph[:len(paragraph)-len(strings.TrimLeft(paragraph, " "))]
		indent = fitCells(indent, max(inner-1, 0))
		for _, l := range wrapPlain(strings.TrimLeft(paragraph, " "), max(inner-lipgloss.Width(indent), 1)) {
			lines = append(lines, rail+t.th.dim.Render(indent+l))
		}
	}
	if len(lines) > 1 {
		lines = append(lines, t.th.faint.Render("╰"))
	}
	return lines
}

// The transcript's glyph language is the patch panel: a heavy bar marks what
// the user plugged in, thin rails carry tool activity, and a diamond junction
// marks the router switching jacks. Box-drawing characters only, because
// they render everywhere a terminal does.
//
// A user turn renders as a card on the surface ground, bar on the first line
// and the rest padded flush: in a stream of rung-colored rails the eye needs
// what *you* said to land as one object, not as lines that happen to share a
// prefix. Every segment carries the ground, the way the status bar does.
func (t *transcript) renderUser(text string, w int) []string {
	inner := max(w-4, 1) // the 1-cell page margin, the two-cell bar, a right pad
	on := t.th.onSurface
	bar := on(t.th.user).Render("▌ ")
	pad := on(t.th.user).Render("  ")
	var lines []string
	for i, l := range wrapPlain(text, inner) {
		lead := pad
		if i == 0 {
			lead = bar
		}
		gap := max(w-1-2-lipgloss.Width(l), 0)
		lines = append(lines, lead+on(t.th.text).Render(l)+on(t.th.text).Render(strings.Repeat(" ", gap)))
	}
	return lines
}

func (t *transcript) renderTool(tool *toolEntry, expanded bool, rank int, w int) []string {
	rail := t.th.faint
	if rank >= 0 {
		rail = t.th.rung(rank)
	}
	headBody := t.th.bold.Render(terminaltext.Escape(tool.name))
	if tool.desc != "" {
		headBody += t.th.dim.Render(" " + terminaltext.Display(tool.desc))
	}
	head := prefixedWrappedLines(rail.Render("│ "), headBody, w-1)
	if !tool.done {
		return head
	}
	// Completion is a verdict glyph, not a word: ✓ and ✗ read at scroll speed,
	// where "ok" and "failed" read at reading speed.
	status := t.th.ok.Render("✓") + t.th.dim.Render(" "+formatDuration(tool.took))
	if tool.failed {
		status = t.th.err.Render("✗ " + formatDuration(tool.took))
	}
	lines := append(head, rail.Render("└ ")+status)

	detail := strings.TrimRight(terminaltext.Display(tool.detail), "\n")
	if detail == "" {
		return lines
	}
	if !expanded {
		// A failure shows its tail in full below; repeating the first line
		// inline as well would print a one-line error twice.
		if first := firstLine(detail); first != "" && !tool.failed {
			remaining := max(w-1-lipgloss.Width(lines[1])-3, 0)
			if remaining > 0 {
				lines[1] += t.th.dim.Render(" · " + fitCells(first, remaining))
			}
		}
		if tool.failed {
			lines = append(lines, indentWrappedLines(t.th.err, tailLines(detail, 24), 4, w-1)...)
		}
		return lines
	}
	style := t.th.dim
	if tool.failed {
		style = t.th.err
	}
	lines = append(lines, indentWrappedLines(style, tailLines(detail, 200), 4, w-1)...)
	return lines
}

// renderTodo draws the task list the model maintains. It replaces the tool
// rail entry for a todo call, so the transcript shows the list itself where
// every other tool shows a verdict line: the list is the result. The glyphs
// are the transcript's own: ✓ for done, ▸ for the one active item, · for
// pending, and the rail carries the rung color like every tool line.
func (t *transcript) renderTodo(items []tools.TodoItem, rank int, w int) []string {
	rail := t.th.faint
	if rank >= 0 {
		rail = t.th.rung(rank)
	}
	done := 0
	for _, item := range items {
		if item.Status == tools.TodoDone {
			done++
		}
	}
	head := rail.Render("│ ") + t.th.bold.Render("tasks") +
		t.th.dim.Render(fmt.Sprintf(" %d/%d", done, len(items)))
	lines := []string{head}
	for i, item := range items {
		lead := "│ "
		if i == len(items)-1 {
			lead = "└ "
		}
		textLines := wrapPlain(terminaltext.Display(item.Text), max(w-5, 1))
		if len(textLines) == 0 {
			textLines = []string{""}
		}
		var mark string
		style := t.th.dim
		switch item.Status {
		case tools.TodoDone:
			mark = t.th.ok.Render("✓ ")
		case tools.TodoActive:
			mark = t.th.bold.Render("▸ ")
			style = t.th.bold
		default:
			mark = t.th.dim.Render("· ")
		}
		for j, text := range textLines {
			rowLead, rowMark := lead, mark
			if j > 0 {
				rowLead, rowMark = "│ ", "  "
				if i == len(items)-1 {
					rowLead = "  "
				}
			}
			lines = append(lines, rail.Render(rowLead)+rowMark+style.Render(text))
		}
	}
	return lines
}

// renderNotice speaks the transcript's glyph vocabulary: the glyph carries
// the color and the severity, the text stays quiet. Word prefixes read as
// debug output; a mark reads as the page's own voice.
func (t *transcript) renderNotice(e *entry, w int) []string {
	style, glyph, body := t.th.dim, "·", t.th.dim
	switch e.level {
	case "warn":
		style, glyph, body = t.th.warn, "△", t.th.warn
	case "error":
		style, glyph, body = t.th.err, "✗", t.th.err
	case "route":
		// The junction marker wears the rung the move landed on; a route
		// event with no destination rank stays on the accent.
		style, glyph = t.th.accent, "◆"
		if e.rank >= 0 {
			style = t.th.rung(e.rank)
		}
	case "advisor":
		style, glyph = t.th.accent, "◇"
	case "watch":
		style, glyph = t.th.ok, "✓"
	case "done":
		// The turn's closing verdict. It closes a tool rail with the rail's
		// own └ when one is directly above; after prose it stands alone,
		// because a corner with nothing over it reads as a broken rail.
		rail := t.th.dim
		if e.rank >= 0 {
			rail = t.th.rung(e.rank)
		}
		lead := t.th.ok.Render("✓ ")
		if e.rail {
			lead = rail.Render("└ ") + t.th.ok.Render("✓ ")
		}
		bodyWidth := max(w-1-lipgloss.Width(lead), 1)
		wrapped := wrapPlain(terminaltext.Display(e.text), bodyWidth)
		if len(wrapped) == 0 {
			wrapped = []string{""}
		}
		lines := make([]string, 0, len(wrapped))
		for i, line := range wrapped {
			prefix := strings.Repeat(" ", lipgloss.Width(lead))
			if i == 0 {
				prefix = lead
			}
			lines = append(lines, prefix+t.th.dim.Render(line))
		}
		return lines
	}
	var lines []string
	for i, l := range wrapPlain(terminaltext.Display(e.text), max(w-3, 1)) {
		if i == 0 {
			lines = append(lines, style.Render(glyph+" ")+body.Render(l))
		} else {
			lines = append(lines, "  "+body.Render(l))
		}
	}
	return lines
}

// renderRoute draws a router decision collapsed to one line, per §14, with
// the full record behind ctrl-o. The diamond is the junction marker, colored
// by the rung the decision landed on: the one glyph that means "the router
// switched jacks here".
func (t *transcript) renderRoute(e *entry, w int) []string {
	marker := t.th.accent
	if e.rank >= 0 {
		marker = t.th.rung(e.rank)
	}
	summary := wrapPlain(terminaltext.Display(e.routeSummary), max(w-3, 1))
	if len(summary) == 0 {
		summary = []string{""}
	}
	lines := make([]string, 0, len(summary)+len(e.routeLines))
	for i, text := range summary {
		lead := "  "
		if i == 0 {
			lead = marker.Render("◆ ")
		}
		lines = append(lines, lead+t.th.dim.Render(text))
	}
	if !e.expanded {
		return lines
	}
	for _, l := range e.routeLines {
		l = terminaltext.Display(l)
		for _, wl := range wrapPlain(l, max(w-5, 1)) {
			lines = append(lines, t.th.dim.Render("    "+wl))
		}
	}
	return lines
}

// view returns the visible window. offset counts lines up from the bottom.
// A transcript shorter than the viewport anchors at the top — the session
// starts where the eye starts, and the empty rows fall below the content.
func (t *transcript) view(height int) string {
	if height <= 0 {
		return ""
	}
	t.height = height
	t.applyReflowAnchor(true)
	t.clampOffset()
	total := len(t.flat)
	end := total - t.offset
	if end < 0 {
		end = 0
	}
	start := end - height
	if start < 0 {
		start = 0
	}
	visible := t.flat[start:end]
	painted := false
	if len(t.marks) > 0 {
		visible = append([]string(nil), visible...)
		painted = true
		for j := range visible {
			if mark, ok := t.marks[start+j]; ok {
				if visible[j] == "" {
					visible[j] = fitCells(mark, max(t.width, 0))
				} else if t.width <= 1 {
					// There is no dedicated margin cell at width one. Replacing
					// the row is less informative than a normal view, but it is
					// valid UTF-8 and cannot split the first wide grapheme.
					visible[j] = fitCells(mark, max(t.width, 0))
				} else {
					// Every non-blank line begins with the one-cell page
					// margin; the marker takes that cell.
					visible[j] = mark + visible[j][1:]
				}
			}
		}
	}
	if t.selOn {
		lo, hi := t.selAnchor, t.selEnd
		if lo > hi {
			lo, hi = hi, lo
		}
		if hi >= start && lo < end {
			if !painted {
				visible = append([]string(nil), visible...)
			}
			const reverse = "\x1b[7m"
			for j := max(lo-start, 0); j <= min(hi-start, len(visible)-1); j++ {
				if visible[j] == "" {
					continue
				}
				// Re-assert the highlight after every embedded reset, or it
				// would end at the line's first styled span.
				visible[j] = reverse + strings.ReplaceAll(visible[j], "\x1b[0m", "\x1b[0m"+reverse) + "\x1b[27m"
			}
		}
	}
	if pad := height - len(visible); pad > 0 {
		visible = append(append([]string(nil), visible...), make([]string, pad)...)
	}
	return strings.Join(visible, "\n")
}

// scrollTo centers a flat line in the viewport, clamped to the content.
func (t *transcript) scrollTo(line int) {
	if t.height <= 0 {
		return
	}
	t.reflowPending = false
	// The visible interval is half-open, so the matched line itself consumes
	// one row. Accounting for that row avoids landing one line above a match
	// when the viewport is only one cell tall.
	t.offset = len(t.flat) - line - (t.height+1)/2
	t.clampOffset()
}

func (t *transcript) scrollBy(n int) {
	t.reflowPending = false
	t.offset += n
	t.clampOffset()
}

// clampOffset keeps the offset inside what can actually scroll: when the
// whole transcript fits the viewport there is nothing to scroll past.
func (t *transcript) clampOffset() {
	limit := len(t.flat) - t.height
	if t.height <= 0 {
		limit = len(t.flat)
	}
	if limit < 0 {
		limit = 0
	}
	if t.offset > limit {
		t.offset = limit
	}
	if t.offset < 0 {
		t.offset = 0
	}
}

func (t *transcript) scrollToBottom() {
	t.reflowPending = false
	t.offset = 0
}

type transcriptReflowAnchor struct {
	entry         int
	screenRow     int
	semantic      string
	semanticStart int
	semanticTotal int
	relativeRow   int
	entryRows     int
}

// captureReflowAnchor records the first meaningful row in a scrolled
// viewport as an entry plus an offset in that entry's decoration-free text.
// Removing whitespace and repeated rails makes the stream invariant under
// ordinary word wrapping, including CJK hard-wraps and styled info cards.
func (t *transcript) captureReflowAnchor() *transcriptReflowAnchor {
	if t.offset <= 0 || len(t.flat) == 0 || t.height <= 0 {
		return nil
	}
	end := max(len(t.flat)-t.offset, 0)
	start := max(end-t.height, 0)
	for line := start; line < end; line++ {
		entryIndex := t.entryIndexAt(line)
		if entryIndex < 0 {
			continue
		}
		entryStart, entryEnd := t.entryBounds(entryIndex)
		semantic, starts, lengths := reflowSemanticRows(t.flat[entryStart:entryEnd])
		relative := line - entryStart
		if relative < 0 || relative >= len(lengths) || lengths[relative] == 0 {
			continue
		}
		return &transcriptReflowAnchor{
			entry: entryIndex, screenRow: line - start, semantic: semantic,
			semanticStart: starts[relative], semanticTotal: len([]rune(semantic)),
			relativeRow: relative, entryRows: entryEnd - entryStart,
		}
	}

	// A viewport containing only blank/decorative rows is unusual but still
	// gets a stable entry-relative fallback instead of reverting to the old
	// rows-from-bottom interpretation.
	entryIndex := t.entryIndexAt(start)
	if entryIndex < 0 {
		return nil
	}
	entryStart, entryEnd := t.entryBounds(entryIndex)
	return &transcriptReflowAnchor{
		entry: entryIndex, relativeRow: start - entryStart, entryRows: entryEnd - entryStart,
	}
}

func (t *transcript) resolveReflowAnchor(anchor transcriptReflowAnchor) (int, int) {
	if anchor.entry < 0 || anchor.entry >= len(t.entries) {
		return -1, 0
	}
	entryStart, entryEnd := t.entryBounds(anchor.entry)
	rows := t.flat[entryStart:entryEnd]
	if len(rows) == 0 {
		return entryStart, anchor.screenRow
	}
	semantic, starts, lengths := reflowSemanticRows(rows)
	relative := proportionalRow(anchor.relativeRow, anchor.entryRows, len(rows))
	if semantic != "" && anchor.semanticTotal > 0 {
		desired := reflowSemanticOffset(anchor, semantic)
		lastMeaningful := relative
		for i, length := range lengths {
			if length == 0 {
				continue
			}
			lastMeaningful = i
			if desired < starts[i]+length {
				relative = i
				break
			}
			relative = lastMeaningful
		}
	}
	return entryStart + min(max(relative, 0), len(rows)-1), anchor.screenRow
}

func (t *transcript) entryBounds(index int) (int, int) {
	start := t.starts[index]
	end := len(t.flat)
	if index+1 < len(t.starts) {
		end = t.starts[index+1]
	}
	return start, end
}

func (t *transcript) entryIndexAt(line int) int {
	if line < 0 || line >= len(t.flat) {
		return -1
	}
	// starts is monotonic and a resumed transcript can hold thousands of
	// entries. A resize should cost viewport rows times log(entries), not a
	// backwards walk over the whole session for every candidate anchor row.
	i := sort.Search(len(t.starts), func(i int) bool {
		return t.starts[i] > line
	}) - 1
	if i >= 0 {
		return i
	}
	return -1
}

func (t *transcript) applyReflowAnchor(clear bool) {
	if !t.reflowPending || t.height <= 0 {
		return
	}
	visibleStart := t.reflowLine - t.reflowScreenRow
	t.offset = len(t.flat) - visibleStart - t.height
	t.clampOffset()
	if clear {
		t.reflowPending = false
	}
}

func proportionalRow(row, oldRows, newRows int) int {
	if newRows <= 1 || oldRows <= 1 {
		return 0
	}
	row = min(max(row, 0), oldRows-1)
	return (row*(newRows-1) + (oldRows-1)/2) / (oldRows - 1)
}

func reflowSemanticOffset(anchor transcriptReflowAnchor, semantic string) int {
	newRunes := []rune(semantic)
	oldRunes := []rune(anchor.semantic)
	oldOffset := min(max(anchor.semanticStart, 0), len(oldRunes))
	if anchor.semantic == semantic {
		return min(oldOffset, len(newRunes))
	}
	// Renderer decorations can vary by width. A window around the old row
	// recovers the exact semantic position when the underlying prose remains;
	// proportional progress is the honest fallback when markdown itself changed.
	windowStart := max(oldOffset-16, 0)
	windowEnd := min(oldOffset+32, len(oldRunes))
	if windowEnd > windowStart {
		needle := string(oldRunes[windowStart:windowEnd])
		if found := strings.Index(semantic, needle); found >= 0 {
			prefixRunes := len([]rune(semantic[:found]))
			return min(prefixRunes+oldOffset-windowStart, len(newRunes))
		}
	}
	if anchor.semanticTotal <= 0 {
		return 0
	}
	return min((oldOffset*len(newRunes)+anchor.semanticTotal/2)/anchor.semanticTotal, len(newRunes))
}

func reflowSemanticRows(lines []string) (string, []int, []int) {
	starts := make([]int, len(lines))
	lengths := make([]int, len(lines))
	var b strings.Builder
	runes := 0
	for i, line := range lines {
		starts[i] = runes
		text := reflowSemanticRow(line)
		lengths[i] = len([]rune(text))
		runes += lengths[i]
		b.WriteString(text)
	}
	return b.String(), starts, lengths
}

func reflowSemanticRow(line string) string {
	runes := []rune(strings.TrimSpace(ansi.Strip(line)))
	for len(runes) > 0 && strings.ContainsRune("│╭╰└▌", runes[0]) {
		runes = []rune(strings.TrimSpace(string(runes[1:])))
	}
	return strings.Join(strings.Fields(string(runes)), "")
}

// maxTextWidth caps prose regardless of terminal width. On a wide monitor,
// uncapped text running edge to edge is the most legible nobody-designed-this
// signal a transcript can send.
const maxTextWidth = 120

func (t *transcript) setWidth(width int) {
	if width > maxTextWidth {
		width = maxTextWidth
	}
	if width == t.width {
		return
	}
	// Settle a prior resize if multiple size messages arrive before the next
	// paint, then capture the old wrapping while its row map still exists.
	t.applyReflowAnchor(true)
	anchor := t.captureReflowAnchor()
	t.width = width
	t.md.setWidth(width)
	// Width-keyed caches make re-render a cache hit where this width was seen
	// before; either way each entry re-renders once rather than per repaint.
	t.flat = nil
	t.searchable = nil
	t.starts = t.starts[:0]
	for _, e := range t.entries {
		t.starts = append(t.starts, len(t.flat))
		lines := t.render(e)
		t.flat = append(t.flat, lines...)
		for _, l := range lines {
			t.searchable = append(t.searchable, strings.ToLower(plainLine(l)))
		}
	}
	if anchor != nil {
		t.reflowLine, t.reflowScreenRow = t.resolveReflowAnchor(*anchor)
		t.reflowPending = t.reflowLine >= 0
		// Keep offset valid for any interaction before Bubble Tea's next paint;
		// view reapplies the target using the actual post-resize height.
		t.applyReflowAnchor(false)
	} else {
		t.reflowPending = false
		t.clampOffset()
	}
	t.changed()
}

func (t *transcript) setTheme(th *theme) {
	t.th = th
	for _, e := range t.entries {
		e.cache = nil
	}
	w := t.width
	t.width = -1
	t.setWidth(w)
}

func (t *transcript) reset() {
	t.entries = nil
	t.starts = nil
	t.flat = nil
	t.searchable = nil
	t.offset = 0
	t.reflowPending = false
	t.marks = nil
	t.changed()
}

// lastExpandable returns the most recent route or tool entry, which is what
// ctrl-o toggles.
func (t *transcript) lastExpandable() int {
	for i := len(t.entries) - 1; i >= 0; i-- {
		if t.entries[i].kind == kindRoute || t.entries[i].kind == kindTool {
			return i
		}
	}
	return -1
}

// entryAt maps a viewport row from the last view to its entry index, -1
// outside the content: what a mouse click lands on. The math mirrors view's
// own window, so the two cannot disagree about what a row shows.
func (t *transcript) entryAt(row int) int {
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
	// starts is ascending; the entry owning the line is the last one that
	// begins at or before it.
	for i := len(t.starts) - 1; i >= 0; i-- {
		if t.starts[i] <= line {
			return i
		}
	}
	return -1
}

func indentWrappedLines(style lipgloss.Style, lines []string, indent, width int) []string {
	width = max(width, 1)
	indent = min(max(indent, 0), max(width-1, 0))
	pad := strings.Repeat(" ", indent)
	bodyWidth := max(width-indent, 1)
	var out []string
	for _, line := range lines {
		wrapped := wrapPlain(line, bodyWidth)
		if len(wrapped) == 0 {
			wrapped = []string{""}
		}
		for _, l := range wrapped {
			out = append(out, style.Render(pad+l))
		}
	}
	return out
}

func prefixedWrappedLines(prefix, body string, width int) []string {
	width = max(width, 1)
	bodyWidth := max(width-lipgloss.Width(prefix), 1)
	wrapped := strings.Split(wrapCells(body, bodyWidth), "\n")
	if len(wrapped) == 0 {
		wrapped = []string{""}
	}
	out := make([]string, 0, len(wrapped))
	for _, line := range wrapped {
		out = append(out, prefix+line)
	}
	return out
}

func boundTranscriptLines(lines []string, width int) []string {
	if width <= 0 {
		return make([]string, len(lines))
	}
	var out []string
	for _, line := range lines {
		if line == "" {
			out = append(out, "")
			continue
		}
		wrapped := strings.Split(wrapCells(line, width), "\n")
		for _, row := range wrapped {
			// A grapheme may itself be wider than a one-cell viewport. The
			// terminal cannot display it there, but it still must not create an
			// untracked physical row.
			out = append(out, ansi.Truncate(row, width, ""))
		}
	}
	return out
}

func tailLines(s string, n int) []string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = append([]string{fmt.Sprintf("… %d earlier lines …", len(lines)-n)}, lines[len(lines)-n:]...)
	}
	return lines
}

func truncate(s string, n int) string {
	boundaries := graphemeBoundaries(s)
	clusters := len(boundaries) - 1
	if clusters <= n {
		return s
	}
	n = min(max(n, 0), clusters)
	return string([]rune(s)[:boundaries[n]]) + "…"
}
