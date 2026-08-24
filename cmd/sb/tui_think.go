package main

// /think: reasoning effort for the model that is running, changed without
// leaving the session. The change is session-scoped by design — a binding
// that should survive a restart is made in /models, where effort is part of
// the rung — and it is visible twice the moment it lands: the machine target
// identity changes and the readable status label names the reasoning effort.
//
// The words on offer are the running target's own: what its server stated at
// probe time where the surface reports them, and the catalog's entry where
// only the catalog has spoken. A fixed list here would be a second opinion
// about what a target accepts, and it was wrong twice over: xhigh is priced
// on the current Opus and Sonnet models and /models will bind it, while this
// command refused to type it, and the subscription surface's floor stopped at
// high while the endpoint lists xhigh and max for the model that is running.
// Where neither source knows the target — a local model, an unpriced
// endpoint — the four words below stand in, because a picker with no items is
// worse than a conservative one.

import (
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/provider"
)

var fallbackThinkLevels = []string{"low", "medium", "high", "max"}

// thinkLevelsFor names what this target will take. The live probe's answer
// wins when the server stated the model's levels, then the catalog's; ok
// reports whether either spoke, so the caller can say which list the user is
// looking at rather than presenting a guess as a fact.
func (m *tuiModel) thinkLevelsFor(target provider.RouteTarget) (levels []string, fromTarget bool) {
	if stated, ok := m.app.providers.probedEffortLevels(target); ok {
		return stated, true
	}
	if info, _, ok := m.app.catalog.Lookup(target); ok && len(info.EffortLevels) > 0 {
		return info.EffortLevels, true
	}
	return fallbackThinkLevels, false
}

func cmdThink(m *tuiModel, args string) tea.Cmd {
	if args != "" {
		return m.applyThink(args)
	}
	levels, _ := m.thinkLevelsFor(m.app.tier.Target)
	items := []pickerItem{{id: "default", label: "default", desc: "let the provider decide"}}
	for _, level := range levels {
		items = append(items, pickerItem{id: level, label: level})
	}
	current := effortOf(m.app.tier.Target)
	for i := range items {
		items[i].current = items[i].id == current || (current == "" && items[i].id == "default")
	}
	m.openDialog(&pickerDialog{
		title:  "reasoning effort for " + m.app.tier.Target.Display(),
		items:  items,
		onPick: func(level string) tea.Cmd { return m.applyThink(level) },
	})
	return nil
}

func (m *tuiModel) applyThink(level string) tea.Cmd {
	levels, fromTarget := m.thinkLevelsFor(m.app.tier.Target)
	switch {
	case level == "default" || level == "off":
		level = ""
	case slices.Contains(levels, level):
	default:
		known := strings.Join(levels, ", ")
		if fromTarget {
			return noticeCmd("error", m.app.tier.Target.Display()+" takes "+known+", or default")
		}
		// Nothing priced this target, so the list is this command's floor
		// rather than the target's answer. Say which it is: a rejection that
		// reads as the model's word would send the user looking in the wrong
		// place for a word the model may well accept.
		return noticeCmd("error", "no effort levels are recorded for "+m.app.tier.Target.Display()+"; this command takes "+known+", or default")
	}

	tier := m.app.tier
	if level == "" {
		tier.Target.Params.Reasoning = nil
	} else {
		tier.Target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: level}
	}

	// Same client, new inference parameters. bind rebuilds the cache
	// controller too, which matters: effort changes cache identity, and a
	// tracker carried across that boundary would attribute one prefix's
	// cache to another (§6).
	m.app.bind(tier, m.app.loop.Binding().Provider, true)
	m.tierLine = m.app.tierLine()
	m.refreshCtxWindow()

	if level == "" {
		return noticeCmd("", "reasoning effort is the provider's default for this session; a target that cannot reason will say so on the next turn")
	}
	return noticeCmd("", "reasoning effort is "+level+" for this session; /models rebinds a rung if it should persist")
}
