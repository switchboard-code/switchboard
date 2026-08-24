package main

// /routing: who moves the rung.
//
// The escalation policy moving the primary mid-task is the product's central
// bet, and a bet is something a user gets to decline. Declining it does not
// stop the session being watched: signals keep being detected and recorded, so
// /why still answers what the router would have done and the advisor still has
// the stream it reads. What stops is the policy acting on them.

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

func cmdRouting(m *tuiModel, args string) tea.Cmd {
	switch strings.ToLower(strings.TrimSpace(args)) {
	case "on":
		return m.setRouting(true)
	case "off":
		return m.setRouting(false)
	case "", "status":
		return noticeCmd("", m.routingStanding())
	}
	return noticeCmd("error", "routing takes on, off, or status")
}

func (m *tuiModel) setRouting(on bool) tea.Cmd {
	if err := m.app.config.SetRouteAutoAndSave(on); err != nil {
		return noticeCmd("error", "saving the routing setting failed: "+err.Error())
	}
	if m.app.watcher != nil {
		m.app.watcher.setPaused(!on)
	}
	if on {
		return noticeCmd("", "routing on: the policy may move the primary on its own signals, and every move says why")
	}
	return noticeCmd("", "routing off: the rung changes only when you change it; signals are still recorded, and /why answers what would have happened")
}

func (m *tuiModel) routingStanding() string {
	if !m.app.config.RouteAutoOn() {
		return "routing is off; /routing on lets the policy move the primary again"
	}
	standing := "routing is on; the policy may move the primary and says why each time"
	if m.app.advisor != nil {
		// They read the same stream for the same reason, so with both on one
		// signal both moves the rung and wakes a second model. Worth saying
		// once rather than wiring one to the other: a setting that silently
		// changes another is a setting nobody can reason about.
		standing += "\nthe advisor reads the same signals, so a stuck turn both moves the rung and wakes it"
	}
	return standing
}
