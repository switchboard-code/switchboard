package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/terminaltext"
)

// statusLine is the always-on readout §14 requires, drawn as one continuous
// surface across the bottom: routing visible at rest. Left to right — the
// active rung's chip and target, then the session's routing history as one
// dot per landed move, the ladder strip (every rung as a block in its heat
// color, the active one raised), the streaming sparkline while a turn runs,
// mode, effort, spend, context, and the clock. When the terminal narrows,
// the newest luxuries leave first: sparkline, clock, effort, dots.
//
//	▌t3 kimi▐ kimi/coding/kimi-for-coding-highspeed  ·· ▂▂█▂ ▁▃▆▅ ~42 tok/s acceptEdits plan ctx 34% 12:34
func (m *tuiModel) statusLine() string {
	th := m.th
	width := max(m.width, 0)
	if width == 0 {
		return ""
	}
	rank := m.activeRank()

	chip := terminaltext.Escape(m.app.tier.ID)
	if m.app.tier.Label != "" {
		chip += " · " + terminaltext.Escape(m.app.tier.Label)
	}
	chipStyle := th.tierChip
	if rank >= 0 {
		chipStyle = th.rungChip(rank)
	}

	target := ""
	if i := strings.Index(m.tierLine, "  "); i >= 0 {
		target = terminaltext.Escape(strings.TrimSpace(m.tierLine[i:]))
	}

	// Right-side segments, in display order. Optional ones carry a drop
	// priority: when the bar does not fit, the highest number leaves first.
	// Every segment has a priority because even the important facts cannot be
	// allowed to spill onto a second terminal row. Mode, spend, and context are
	// the last facts to leave; the rung chip is the final compact identity.
	type segment struct {
		s    string
		drop int
	}
	var segs []segment
	add := func(s string, drop int) {
		if s != "" {
			segs = append(segs, segment{s: s, drop: drop})
		}
	}
	add(m.moveDots(), 70)
	add(m.ladderStrip(), 40)
	add(m.sparkline(), 100)
	// One filled chip on the bar reads as deliberate; three read as a
	// toolbar. The rung chip keeps its fill; mode is its hue as text — except
	// default, whose chip ground is the neutral gray that vanishes as a
	// foreground, so the quiet mode speaks in the quiet color.
	if chipS, ok := th.modeChip[string(m.mode)]; ok && m.mode != "default" {
		modeStyle := lipgloss.NewStyle().Foreground(chipS.GetBackground())
		add(th.onBar(modeStyle).Render(terminaltext.Escape(string(m.mode))), 10)
	} else {
		add(th.onBar(th.dim).Render(terminaltext.Escape(string(m.mode))), 10)
	}
	if effort := effortOf(m.app.tier.Target); effort != "" {
		add(th.onBar(th.dim).Render("think "+terminaltext.Escape(effort)), 80)
	}
	add(m.watchChip(), 30)
	if m.updateAvail != "" {
		add(th.onBar(th.warn).Render("↑ "+terminaltext.Escape(m.updateAvail)), 50)
	}
	costStyle := th.ok
	switch {
	case m.costPct >= 85:
		costStyle = th.err
	case m.costPct >= 60:
		costStyle = th.warn
	}
	add(th.onBar(costStyle).Render(terminaltext.Escape(m.costLine)), 20)
	add(m.ctxPct(), 15)
	add(m.clock(), 90)
	if m.tr.offset > 0 {
		add(th.onBar(th.dim).Render(fmt.Sprintf("↑%d", m.tr.offset)), 25)
	}

	sep := th.onBar(lipgloss.NewStyle()).Render("  ")
	rightWidth := func() int {
		w := 0
		for i, s := range segs {
			if i > 0 {
				w += lipgloss.Width(sep)
			}
			w += lipgloss.Width(s.s)
		}
		return w
	}

	// A user can name a tier with wide glyphs or a very long label. Give the
	// chip a proportional ceiling before considering the rest of the bar; its
	// own padding is included in that ceiling.
	chipLimit := min(width, max(width/3, 5))
	chip = fitCells(chip, max(chipLimit-2, 1))
	chipRendered := chipStyle.Render(" " + chip + " ")
	chipW := lipgloss.Width(chipRendered)

	// First ensure that the compact identity plus retained facts fits. This is
	// what makes 20-column split panes safe rather than merely uncommon.
	for chipW+rightWidth()+boolWidth(len(segs) > 0) > width && len(segs) > 0 {
		dropAt := 0
		for i := 1; i < len(segs); i++ {
			if segs[i].drop > segs[dropAt].drop {
				dropAt = i
			}
		}
		segs = append(segs[:dropAt], segs[dropAt+1:]...)
	}
	if avail := width - rightWidth() - boolWidth(len(segs) > 0); chipW > avail {
		chip = fitCells(chip, max(avail-2, 1))
		chipRendered = chipStyle.Render(" " + chip + " ")
		chipW = lipgloss.Width(chipRendered)
	}

	// The target takes whatever remains between the stable chip and facts. It
	// is cell-truncated, never byte-sliced, so CJK, emoji, and combining marks
	// stay intact.
	targetBudget := width - chipW - rightWidth() - boolWidth(len(segs) > 0) - 1
	if targetBudget > 0 {
		target = fitCells(target, targetBudget)
	} else {
		target = ""
	}
	left := chipRendered
	if target != "" {
		left += th.onBar(th.dim).Render(" " + target)
	}

	var right []string
	for _, s := range segs {
		right = append(right, s.s)
	}
	rightStr := strings.Join(right, sep)
	gap := width - lipgloss.Width(left) - lipgloss.Width(rightStr)
	if rightStr != "" && gap < 1 {
		gap = 1
	}
	line := left + th.onBar(lipgloss.NewStyle()).Render(strings.Repeat(" ", max(gap, 0))) + rightStr
	// Keep a final guard at the paint boundary. All sizing above is cell-aware,
	// but a future segment should not be able to reintroduce a wrapped status
	// row merely by forgetting to participate in the priority scheme.
	return fitCells(line, width)
}

func boolWidth(ok bool) int {
	if ok {
		return 1
	}
	return 0
}

// moveDots is the session's routing history at a glance: one dot per landed
// switch, each in the rung it landed on, newest last. The dots have to agree
// with /why about how much the session moved; both are fed by every rebind.
func (m *tuiModel) moveDots() string {
	if len(m.moves) == 0 {
		return ""
	}
	moves := m.moves
	if len(moves) > 8 {
		moves = moves[len(moves)-8:]
	}
	var b strings.Builder
	for _, rank := range moves {
		b.WriteString(m.th.onBar(m.th.rung(rank)).Render("•"))
	}
	return b.String()
}

// ladderStrip draws the whole ladder as one block per rung in its heat
// color, the active rung raised to a full block: position and depth in four
// cells. Each block is state, not decoration — its color is a rung, its
// height is whether work runs there now.
func (m *tuiModel) ladderStrip() string {
	tiers := m.app.config.Tiers
	if len(tiers) < 2 || len(tiers) > 8 {
		return ""
	}
	rank := m.activeRank()
	var b strings.Builder
	for i := range tiers {
		block := "▂"
		if i == rank {
			block = "█"
		}
		b.WriteString(m.th.onBar(m.th.rung(i)).Render(block))
	}
	return b.String()
}

// sparkline is the stream's pulse while a turn runs: recent tokens-per-second
// samples as a tiny bar chart in the active rung's heat, with the newest
// estimate spelled out. The ~ is honest — the rate is chars over four, not a
// count the provider reported.
func (m *tuiModel) sparkline() string {
	if !m.busy || len(m.samples) == 0 {
		return ""
	}
	peak := 1.0
	for _, s := range m.samples {
		if s > peak {
			peak = s
		}
	}
	ramp := []rune("▁▂▃▄▅▆▇█")
	var b strings.Builder
	for _, s := range m.samples {
		i := int(s / peak * float64(len(ramp)-1))
		b.WriteRune(ramp[max(i, 0)])
	}
	style := m.th.dim
	if rank := m.activeRank(); rank >= 0 {
		style = m.th.rung(rank)
	}
	last := m.samples[len(m.samples)-1]
	return m.th.onBar(style).Render(b.String()) +
		m.th.onBar(m.th.dim).Render(fmt.Sprintf(" ~%.0f tok/s", last))
}

// ctxPct is the context occupancy as text, colored by how close it is to the
// wall: fine, warm, and about to be a problem. The rail above the bar draws
// the same number as a line.
func (m *tuiModel) ctxPct() string {
	pct, ok := m.ctxPercent()
	if !ok {
		// An unknown window is not the same as an empty one, and it is the
		// state that matters most: auto-compaction cannot fire against a
		// window nobody has stated, so a session on this target runs until
		// the server refuses. Saying nothing here is what made that silent.
		if m.ctxWindow <= 0 && m.app.config.CompactAuto {
			return m.th.onBar(m.th.faint).Render("ctx ") +
				m.th.onBar(m.th.warn).Render("?")
		}
		return ""
	}
	style := m.th.accent
	switch {
	case pct >= 85:
		style = m.th.err
	case pct >= 60:
		style = m.th.warn
	}
	// A tilde where the provider reported nothing: the number is this
	// build's own count, and the estimator is measured to run low.
	shown := fmt.Sprintf("%d%%", pct)
	if m.callEstimated {
		shown = "~" + shown
	}
	return m.th.onBar(m.th.faint).Render("ctx ") + m.th.onBar(style).Render(shown)
}

func (m *tuiModel) ctxPercent() (int, bool) {
	if m.ctxWindow <= 0 || m.callTokens <= 0 {
		return 0, false
	}
	pct := m.callTokens * 100 / m.ctxWindow
	if pct > 100 {
		pct = 100
	}
	return pct, true
}

// ctxRail is the thin line riding the top edge of the status bar: the
// context window's fill drawn across the whole width in the active rung's
// heat. The same fact as the ctx percentage, shaped so a glance while
// scrolled into work still catches it.
func (m *tuiModel) ctxRail() string {
	width := max(m.width, 1)
	pct, _ := m.ctxPercent()
	fill := width * pct / 100
	style := m.th.barFill
	if rank := m.activeRank(); rank >= 0 {
		style = m.th.rung(rank)
	}
	switch {
	case pct >= 85:
		style = m.th.err
	case pct >= 60:
		style = m.th.warn
	}
	return style.Render(strings.Repeat("▁", fill)) +
		m.th.barEmpty.Render(strings.Repeat("▁", width-fill))
}

// clock is how long this session has been open, mm:ss and then h:mm:ss: the
// quiet fact that anchors the day the way a wall clock does.
func (m *tuiModel) clock() string {
	if m.sessionAt.IsZero() {
		return ""
	}
	d := time.Since(m.sessionAt).Round(time.Second)
	h, rem := d/time.Hour, d%time.Hour
	mins, secs := rem/time.Minute, (rem%time.Minute)/time.Second
	text := fmt.Sprintf("%d:%02d", mins, secs)
	if h > 0 {
		text = fmt.Sprintf("%d:%02d:%02d", h, mins, secs)
	}
	return m.th.onBar(m.th.faint).Render(text)
}

// effortOf reports the reasoning effort riding on a target, or "".
func effortOf(t provider.RouteTarget) string {
	if t.Params.Reasoning == nil || !t.Params.Reasoning.Enabled {
		return ""
	}
	return t.Params.Reasoning.Effort
}
