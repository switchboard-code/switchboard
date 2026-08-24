package main

import (
	"slices"
	"strings"
	"unicode"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/terminaltext"
)

// --- slash-command suggestions ----------------------------------------------

func (m *tuiModel) suggestions() []commandItem {
	v := m.ta.Value()
	if !strings.HasPrefix(v, "/") || strings.ContainsAny(v, " \n\t") {
		return nil
	}
	prefix := strings.TrimPrefix(v, "/")
	out := matchingCommands(prefix, m.app.config)
	for _, c := range m.custom {
		selector := customSelector(m, c)
		if strings.HasPrefix(selector, prefix) || strings.HasPrefix(c.name, prefix) {
			out = append(out, commandItem{name: selector, desc: customCommandDescription(c)})
		}
	}
	return out
}

func (m *tuiModel) suggestionsVisible() bool {
	return !m.busy && m.dlg == nil && !m.sugClosed && len(m.suggestions()) > 0
}

func (m *tuiModel) suggestionsView() string {
	items := m.suggestions()
	if !m.suggestionsVisible() || len(items) == 0 {
		return ""
	}
	maxRows := 6
	if m.height > 0 {
		// Keep one row each for the transcript and status plus the prompt's
		// content and two-row border. On a short terminal, suggestions yield
		// rows before the conversation or composer does.
		maxRows = min(maxRows, max(m.height-m.ta.Height()-4, 0))
		if maxRows == 0 {
			return ""
		}
	}
	if m.sugSel >= len(items) {
		m.sugSel = len(items) - 1
	}
	shown := items
	start := 0
	if len(items) > maxRows {
		start = m.sugSel - maxRows + 1
		if start < 0 {
			start = 0
		}
		shown = items[start : start+maxRows]
		if len(shown) > maxRows {
			shown = shown[:maxRows]
		}
	}

	rowWidth := max(m.width-1, 1) // inputZoneView adds the one-cell page gutter
	contentWidth := max(rowWidth-1, 0)
	nameWidth := 0
	hasDescription := false
	for _, it := range shown {
		name := "/" + terminaltext.Escape(it.name)
		if it.usage != "" {
			name += " " + terminaltext.Escape(it.usage)
		}
		if n := ansi.StringWidth(name); n > nameWidth {
			nameWidth = n
		}
		if it.desc != "" {
			hasDescription = true
		}
	}
	gapWidth := 0
	descWidth := 0
	if hasDescription && contentWidth >= 20 {
		// Past the narrow-phone case, keep both the command and its purpose
		// visible. Neither column may consume the other merely because one
		// legal label is very long.
		nameWidth = min(nameWidth, max(contentWidth/2, 8))
		gapWidth = 2
		descWidth = max(contentWidth-nameWidth-gapWidth, 0)
	} else {
		nameWidth = min(nameWidth, contentWidth)
	}

	// The selected row is one object: the highlight runs the row's full
	// width, name and description together, the way every picker in the
	// terminal-tool generation the user's hands know behaves.
	var rows []string
	for i, it := range shown {
		name := "/" + terminaltext.Escape(it.name)
		if it.usage != "" {
			name += " " + terminaltext.Escape(it.usage)
		}
		desc := terminaltext.Escape(it.desc)
		name = padRight(fitCells(name, nameWidth), nameWidth)
		if descWidth > 0 {
			desc = padRight(fitCells(desc, descWidth), descWidth)
		} else {
			desc = ""
			name = padRight(name, contentWidth)
		}
		gap := strings.Repeat(" ", gapWidth)
		if start+i == m.sugSel {
			on := func(s lipgloss.Style) lipgloss.Style { return s.Background(m.th.selected.GetBackground()) }
			rows = append(rows, m.th.accent.Render("▌")+
				on(m.th.bold).Render(name)+
				on(m.th.dim).Render(gap+desc))
			continue
		}
		rows = append(rows, " "+name+gap+m.th.dim.Render(desc))
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

func (m *tuiModel) acceptSuggestion() {
	items := m.suggestions()
	if len(items) == 0 {
		return
	}
	if m.sugSel >= len(items) {
		m.sugSel = 0
	}
	m.ta.SetValue("/" + items[m.sugSel].name + " ")
	m.ta.CursorEnd()
	m.resetHistoryNavigation()
	m.sugSel = 0
}

// exactCommand reports whether the input is exactly a command name, so enter
// runs it rather than completing it.
func (m *tuiModel) exactCommand() bool {
	v := strings.TrimPrefix(m.ta.Value(), "/")
	for _, it := range m.suggestions() {
		if it.name == v || slices.Contains(it.aliases, v) {
			return true
		}
	}
	return false
}

// --- history -----------------------------------------------------------------

func (m *tuiModel) historyMove(delta int) {
	if len(m.history) == 0 {
		return
	}
	if m.histIdx < 0 || m.histIdx > len(m.history) {
		m.histIdx = len(m.history)
	}
	if delta < 0 && m.histIdx == len(m.history) {
		m.histDraft = m.ta.Value()
		m.histDraftSet = true
	}
	next := min(max(m.histIdx+delta, 0), len(m.history))
	if next == m.histIdx {
		return
	}
	m.histIdx = next
	if m.histIdx == len(m.history) {
		if m.histDraftSet {
			m.ta.SetValue(m.histDraft)
		} else {
			m.ta.SetValue("")
		}
		m.histDraft = ""
		m.histDraftSet = false
	} else {
		m.ta.SetValue(m.history[m.histIdx])
	}
	m.ta.CursorEnd()
	m.growInput()
}

// resetHistoryNavigation turns the currently visible composer text back into
// the live draft. Editing a recalled entry starts a new draft; no stale saved
// value may replace it on the next down-arrow or session boundary.
func (m *tuiModel) resetHistoryNavigation() {
	m.histIdx = len(m.history)
	m.histDraft = ""
	m.histDraftSet = false
}

// growInput sizes the prompt to what is actually in it.
//
// Counting newlines was not the same question: a paragraph typed without ever
// pressing enter is one logical line and many terminal rows, so the box stayed
// one row tall and the text scrolled out of sight as it was typed.
func (m *tuiModel) growInput() {
	rows := inputRows(m.ta)
	if ceiling := m.inputCeiling(); rows > ceiling {
		rows = ceiling
	}
	if rows < 1 {
		rows = 1
	}
	m.ta.SetHeight(rows)
}

// inputCeiling is how tall the prompt may grow. The transcript is sized from
// whatever is left, so an unbounded prompt would push the conversation off the
// screen; a third of the pane keeps both readable, and a small terminal still
// gets the original six.
func (m *tuiModel) inputCeiling() int {
	const (
		floor   = 6
		ceiling = 16
	)
	if m.height <= 0 {
		return floor
	}
	third := m.height / 3
	switch {
	case third < floor:
		return floor
	case third > ceiling:
		return ceiling
	}
	return third
}

// inputRows is how many terminal rows the typed text occupies once wrapped.
//
// The count comes from the textarea itself rather than from a second word-wrap
// implementation here. Model is a value type and LineInfo has a value
// receiver, so a copy per logical line reports that line's wrapped height
// using the same rules that will draw it; two implementations of the same wrap
// would eventually disagree, and the row that disagreed would be the one with
// the cursor in it.
func inputRows(ta textarea.Model) int {
	total := 0
	for _, line := range strings.Split(ta.Value(), "\n") {
		probe := ta
		probe.SetValue(line)
		probe.CursorEnd()
		height := probe.LineInfo().Height
		if height < 1 {
			height = 1
		}
		total += height
	}
	if total < 1 {
		return 1
	}
	return total
}

// --- slash dispatch -----------------------------------------------------------

// runSlash handles a /command. While a turn runs, commands that would touch
// the session are refused rather than racing it; /exit still works.
func (m *tuiModel) runSlash(v string) tea.Cmd {
	raw := strings.TrimPrefix(v, "/")
	name, rest := raw, ""
	if split := strings.IndexFunc(raw, unicode.IsSpace); split >= 0 {
		name, rest = raw[:split], raw[split:]
	}
	rest = strings.TrimSpace(rest)
	if m.retryRecoveryExists() && !retryRecoveryCommandAllowed(name, rest) {
		return noticeCmd("warn", retryRecoveryGuardText)
	}

	// The explicit namespace is resolved before dynamic tier shorthands. A
	// tier may legally have almost any id, but it must not make the escape
	// hatch for a colliding custom command lie; `/tier <id>` still reaches
	// such an unusually named tier.
	if customName, explicit := customNameFromSelector(name); explicit {
		for _, c := range m.custom {
			if c.name == customName {
				return runCustom(m, c, rest)
			}
		}
		return noticeCmd("error", "unknown custom command "+customName+"; try ctrl+p")
	}

	// A bare tier name switches to it; with a prompt attached it runs just this
	// prompt there, which is §14's command-prefix override.
	if _, ok := m.app.config.Tier(name); ok {
		if m.busy {
			return noticeCmd("warn", "a turn is running; esc to interrupt it first")
		}
		if rest == "" {
			return m.switchTier(name)
		}
		return m.enqueue(rest, name)
	}
	for _, c := range commands() {
		if c.name == name || slices.Contains(c.aliases, name) {
			if m.busy && !c.busySafe {
				return noticeCmd("warn", "a turn is running; esc to interrupt it first")
			}
			return c.run(m, rest)
		}
	}
	for _, c := range m.custom {
		if c.name == name {
			return runCustom(m, c, rest)
		}
	}
	return noticeCmd("error", "unknown command "+name+"; try /help")
}

// cycleMode puts the everyday postures first and the widest grant last:
// default → acceptEdits → auto → plan → yolo → default. Yolo closes the
// cycle so reaching it means passing every narrower mode, and setMode's
// landing warning is what makes the grant conspicuous. Legacy bypass stays
// explicit-only under /mode: without verified confinement it still prompts,
// and the picker is where that is said.
func (m *tuiModel) cycleMode() tea.Cmd {
	order := []permission.Mode{
		permission.ModeDefault,
		permission.ModeAcceptEdits,
		permission.ModeAuto,
		permission.ModePlan,
		permission.ModeYOLO,
	}
	i := slices.Index(order, m.mode)
	next := order[(i+1)%len(order)]
	return m.setMode(next)
}

func (m *tuiModel) setMode(mode permission.Mode) tea.Cmd {
	m.app.loop.Perms.SetMode(mode)
	m.mode = mode
	m.addInfo("mode is now " + string(mode))
	if mode == permission.ModeYOLO {
		warning := "FULL HOST ACCESS: edits, commands, and external tools run without asking; commands are unsandboxed with host filesystem and network reach. Only an explicit deny rule or a no you gave this session still refuses"
		if m.app.capability.Platform == "windows" {
			warning += ". Windows descendant processes may survive cancellation"
		}
		m.addNotice("warn", warning)
	}
	if mode == permission.ModeAuto {
		m.addNotice("", "auto applies ordinary workspace edits; with an active verified sandbox, ordinary non-sensitive commands go to the configured cheap approver; host-direct, external, sensitive, uncertain, and host-loopback-sandbox actions ask you")
	}
	if mode == permission.ModeBypass && !m.app.loop.Perms.Execution().SandboxActive() {
		// Saying this once, plainly, beats letting the user discover it by
		// being prompted anyway and reading it as a bug (§19.3).
		m.addNotice("warn", "commands will still be approved one at a time: bypass needs an active verified sandbox")
	} else if mode == permission.ModeBypass {
		policy := m.app.loop.Perms.Execution().CommandPolicy(false)
		if policy.HostLoopbackShared || policy.HostIPCShared {
			m.addNotice("warn", "commands will still ask: host-local network or IPC services retain authority outside this sandbox; bypass is promptless only when both are isolated")
		}
	}
	return nil
}

func (m *tuiModel) openTierPicker() tea.Cmd {
	if len(m.app.config.Tiers) == 0 {
		return noticeCmd("", "no tiers configured in "+m.app.config.Path)
	}
	var items []pickerItem
	for _, t := range m.app.config.Tiers {
		desc := t.Target.Display()
		if t.Label != "" {
			desc = t.Label + "  " + desc
		}
		items = append(items, pickerItem{
			id:      t.ID,
			label:   t.ID,
			desc:    desc,
			current: t.ID == m.app.tier.ID,
		})
	}
	m.openDialog(&pickerDialog{
		title:  "switch tier",
		items:  items,
		onPick: func(id string) tea.Cmd { return m.switchTier(id) },
	})
	return nil
}
