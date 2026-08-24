package main

// The opening frame. What a routing tool should show first is the ladder:
// every rung in its heat color, the active one barred, each priced in one
// word. That is the product stating its thesis before the first prompt, and
// it replaces a column of dim key-value lines nobody read.

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/terminaltext"
)

func (m *tuiModel) addBanner(sess *session.Session, resumed bool) {
	th, app := m.th, m.app
	state := sess.State()
	activeRank := m.activeRank()

	brand := th.bold.Render("switchboard")
	if v := currentVersion(); v != "" {
		brand += " " + th.dim.Render(terminaltext.Escape(v))
	}
	// The mark is the ladder itself: four ascending bars in the heat ramp,
	// cool to warm. The product's thesis as its wordmark, and a mark no
	// neighboring tool can wear, because nothing else routes.
	mark := ""
	for i, g := range []string{"▁", "▃", "▅", "▇"} {
		mark += th.rung(i).Render(g)
	}
	lines := []string{mark + " " + brand, ""}

	if len(app.config.Tiers) > 0 {
		widest := 0
		idWidest := 0
		for _, t := range app.config.Tiers {
			if n := ansi.StringWidth(terminaltext.Escape(t.Target.Display())); n > widest {
				widest = n
			}
			if n := ansi.StringWidth(terminaltext.Escape(t.ID)); n > idWidest {
				idWidest = n
			}
		}
		idWidth := min(idWidest, 16)
		targetWidth := min(widest, 40)
		for rank, t := range app.config.Tiers {
			id := padRight(fitCells(terminaltext.Escape(t.ID), idWidth), idWidth)
			target := padRight(fitCells(terminaltext.Escape(t.Target.Display()), targetWidth), targetWidth)
			bar := "  "
			if rank == activeRank {
				bar = th.rung(rank).Render("▌ ")
			}
			row := bar + th.rung(rank).Render(id) + "  " +
				th.text.Render(target) +
				"  " + th.faint.Render(terminaltext.Escape(meteringWord(app.catalog, t)))
			lines = append(lines, row)
		}
	} else {
		lines = append(lines, th.dim.Render("  no ladder configured; /models binds one"))
	}

	facts := []string{terminaltext.Escape(app.workspace), terminaltext.Escape(string(app.loop.Perms.Mode())), terminaltext.Escape(app.loop.Perms.Execution().Summary())}
	lines = append(lines,
		th.faint.Render("  "+strings.Join(facts, " · ")),
		th.faint.Render("  session "+terminaltext.Escape(state.ID)+terminaltext.Escape(sessionNote(state, resumed))+" · /help for commands"),
	)
	if app.onboarded {
		lines = append(lines, th.dim.Render(
			"  first time here: shift+tab changes what runs without asking, /race compares two rungs, and sb completion zsh adds tab completion to your shell"))
	}
	if lost := sess.TruncatedBytes(); lost > 0 {
		lines = append(lines, th.warn.Render(fmt.Sprintf(
			"  recovered from an interrupted write; %d bytes at the end of the log were unreadable and were dropped", lost)))
	}
	lines = append(lines, "")

	m.tr.add(&entry{kind: kindRaw, text: strings.Join(lines, "\n"), rank: -1})
	m.tr.scrollToBottom()
}

func sessionNote(state session.State, resumed bool) string {
	if !resumed {
		return ""
	}
	return fmt.Sprintf(", resumed with %d messages", len(state.Messages))
}

// meteringWord is the one-word price of a rung: what choosing it consumes.
func meteringWord(cat *catalog.Catalog, t config.Tier) string {
	info, _, ok := cat.Lookup(t.Target)
	if !ok {
		return "unpriced"
	}
	switch info.Metering {
	case catalog.Local:
		return "local"
	case catalog.Plan:
		return "plan"
	}
	if info.Free() {
		return "free"
	}
	if band, bok := info.Band(0); bok {
		return band.InputPerMTok.String() + "/MTok in"
	}
	return "metered"
}
