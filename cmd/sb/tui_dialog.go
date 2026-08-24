package main

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/terminaltext"
)

// dialog is a modal that takes over the input zone until it resolves. The
// transcript stays visible above it.
type dialog interface {
	update(key tea.KeyMsg, th *theme) (done bool, cmd tea.Cmd)
	view(width int, th *theme) string
	// cancel resolves every dialog through the same path as an explicit safe
	// dismissal. It must release any goroutine or operation waiting behind the
	// modal; simply hiding a dialog is never sufficient.
	cancel() tea.Cmd
}

// heightAwareDialog is the main TUI's bounded rendering extension. Keeping it
// separate from dialog preserves the standalone onboarding model and small
// test dialogs while letting the session TUI pass the rows it can actually
// paint. Every production dialog implements it; the broker still has a safe
// fallback for third-party or test implementations.
type heightAwareDialog interface {
	viewWithin(width, height int, th *theme) string
}

const dialogUnlimitedHeight = 1 << 20

func renderDialogWithin(d dialog, width, height int, th *theme) string {
	if d == nil || width <= 0 || height <= 0 {
		return ""
	}
	var view string
	if bounded, ok := d.(heightAwareDialog); ok {
		view = bounded.viewWithin(width, height, th)
	} else {
		view = d.view(width, th)
	}
	lines := strings.Split(view, "\n")
	for i := range lines {
		lines[i] = fitCells(lines[i], width)
	}
	if len(lines) <= height {
		return strings.Join(lines, "\n")
	}
	focus := dialogLine(lines, "▌", true)
	return strings.Join(dialogWindow(lines, height, focus, 0, len(lines)-1), "\n")
}

// dialogWindow retains a focused row and any mandatory context, then spends
// the remaining rows nearest the focus. It preserves source order even when
// the retained rows are discontiguous. That last property is what lets a
// six-row permission prompt pin the host-access warning, safe default, and
// currently selected answer without moving Enter onto a hidden choice.
func dialogWindow(lines []string, height, focus int, mandatory ...int) []string {
	if height <= 0 || len(lines) == 0 {
		return nil
	}
	if len(lines) <= height {
		return lines
	}
	focus = workspaceClamp(focus, 0, len(lines)-1)
	keep := make(map[int]bool, height)
	order := append([]int{focus}, mandatory...)
	for _, index := range order {
		if index >= 0 && index < len(lines) && len(keep) < height {
			keep[index] = true
		}
	}
	for distance := 1; len(keep) < height && distance < len(lines); distance++ {
		for _, index := range []int{focus - distance, focus + distance} {
			if index >= 0 && index < len(lines) {
				keep[index] = true
				if len(keep) == height {
					break
				}
			}
		}
	}
	for index := 0; len(keep) < height && index < len(lines); index++ {
		keep[index] = true
	}
	out := make([]string, 0, height)
	for index, line := range lines {
		if keep[index] {
			out = append(out, line)
		}
	}
	return out
}

func dialogLine(lines []string, needle string, last bool) int {
	match := -1
	for index, line := range lines {
		if strings.Contains(ansi.Strip(line), needle) {
			match = index
			if !last {
				return match
			}
		}
	}
	return match
}

func boundedBoxContent(content string, height, focus int, mandatory ...int) string {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	contentHeight := max(height-2, 1) // the rounded frame owns top and bottom
	return strings.Join(dialogWindow(lines, contentHeight, focus, mandatory...), "\n")
}

// dialogToken lets an asynchronous waiter withdraw the exact modal it asked
// for when its context ends. A pointer is used only as an opaque, comparable
// identity; no dialog state crosses goroutines.
// The byte keeps distinct allocations distinct under Go's zero-size pointer
// rule; two &struct{} values are otherwise permitted to compare equal.
type dialogToken struct{ _ byte }

type queuedDialog struct {
	dialog dialog
	token  *dialogToken
}

// openDialog is the single modal broker. A new prompt never replaces one the
// user can already see: permission asks, model questions, and asynchronous
// pickers wait in arrival order and each retains its own cleanup path.
func (m *tuiModel) openDialog(d dialog) { m.openDialogFor(d, nil) }

func (m *tuiModel) openDialogFor(d dialog, token *dialogToken) {
	if d == nil {
		return
	}
	if m.dlg == nil {
		m.dlg = d
		m.dialogToken = token
		return
	}
	m.dialogQueue = append(m.dialogQueue, queuedDialog{dialog: d, token: token})
}

// completeDialog advances after the active dialog has already resolved. It
// does not call cancel: a selected action and a dismissal are different facts.
func (m *tuiModel) completeDialog() {
	m.dlg = nil
	m.dialogToken = nil
	if len(m.dialogQueue) == 0 {
		return
	}
	next := m.dialogQueue[0]
	copy(m.dialogQueue, m.dialogQueue[1:])
	m.dialogQueue[len(m.dialogQueue)-1] = queuedDialog{}
	m.dialogQueue = m.dialogQueue[:len(m.dialogQueue)-1]
	m.dlg = next.dialog
	m.dialogToken = next.token
}

// cancelDialogToken removes a prompt whose waiting context ended. It may be
// active or queued; either way its polymorphic cancellation releases the
// corresponding waiter and the next modal becomes visible.
func (m *tuiModel) cancelDialogToken(token *dialogToken) tea.Cmd {
	if token == nil {
		return nil
	}
	if m.dlg != nil && m.dialogToken == token {
		cmd := m.dlg.cancel()
		m.completeDialog()
		return cmd
	}
	for i, pending := range m.dialogQueue {
		if pending.token != token {
			continue
		}
		cmd := pending.dialog.cancel()
		copy(m.dialogQueue[i:], m.dialogQueue[i+1:])
		m.dialogQueue[len(m.dialogQueue)-1] = queuedDialog{}
		m.dialogQueue = m.dialogQueue[:len(m.dialogQueue)-1]
		return cmd
	}
	return nil
}

// cancelDialogs drains the broker before invoking callbacks. Clearing each
// batch first means a cleanup callback cannot replace a modal being dismissed;
// if cleanup itself opens another modal, the next pass cancels that one too.
func (m *tuiModel) cancelDialogs() tea.Cmd {
	var cmds []tea.Cmd
	for m.dlg != nil || len(m.dialogQueue) > 0 {
		all := make([]dialog, 0, 1+len(m.dialogQueue))
		if m.dlg != nil && m.dlg != m.resolvingDialog {
			all = append(all, m.dlg)
		}
		for _, pending := range m.dialogQueue {
			if pending.dialog != nil {
				all = append(all, pending.dialog)
			}
		}
		m.dlg = nil
		m.dialogToken = nil
		clear(m.dialogQueue)
		m.dialogQueue = nil

		for _, d := range all {
			if cmd := d.cancel(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}
	return tea.Batch(cmds...)
}

// cancelDialogsForExit closes modal waiters without allowing their safe
// cancellation path to launch unsent work. A race verdict cancellation can
// normally advance the prompt queue; during teardown that would start routing
// or provider work in the gap before tea.Quit runs.
func (m *tuiModel) cancelDialogsForExit() tea.Cmd {
	m.quitting = true
	clear(m.queue)
	m.queue = nil
	return m.cancelDialogs()
}

// permissionDialog resolves a permission Ask. Design principle 4 applies to
// the drawing as much as to the engine: the moment of approval is the moment
// that has to be plain, so an unsandboxed command says so in the box.
type permissionDialog struct {
	req     permission.Request
	out     permission.Outcome
	respond chan permission.Response
	sel     int
}

func newPermissionDialog(req permission.Request, out permission.Outcome, respond chan permission.Response) *permissionDialog {
	// The first Enter after an asynchronous arrival must be a refusal, never
	// approval under a typing finger. One Down then Enter deliberately opts in.
	return &permissionDialog{req: req, out: out, respond: respond, sel: 0}
}

func (d *permissionDialog) resolve(resp permission.Response) bool {
	select {
	case d.respond <- resp:
	default:
	}
	return true
}

func (d *permissionDialog) update(key tea.KeyMsg, th *theme) (bool, tea.Cmd) {
	switch key.String() {
	case "n", "esc":
		return true, d.cancel()
	case "up":
		if d.sel > 0 {
			d.sel--
		}
	case "down":
		if d.sel < 2 {
			d.sel++
		}
	case "enter":
		switch d.sel {
		case 0:
			return true, d.cancel()
		case 1:
			return d.resolve(permission.Response{Approved: true}), nil
		default:
			return d.resolve(permission.Response{Approved: true, Remember: true}), nil
		}
	}
	return false, nil
}

func (d *permissionDialog) cancel() tea.Cmd {
	d.resolve(permission.Response{})
	return nil
}

func (d *permissionDialog) view(width int, th *theme) string {
	return d.viewWithin(width, dialogUnlimitedHeight, th)
}

func (d *permissionDialog) viewWithin(width, height int, th *theme) string {
	desc := approvalDescription(d.req)
	boxWidth, contentWidth := dialogDimensions(width)

	var b strings.Builder
	writeLine := func(line string) {
		b.WriteString(wrapCells(line, contentWidth))
		b.WriteByte('\n')
	}
	writeLine(wrapCellsBounded(th.bold.Render(" approve "+terminaltext.Escape(d.req.Tool))+" "+th.dim.Render(desc), contentWidth, 4))
	if d.out.Reason != "" {
		writeLine(wrapCellsBounded(th.dim.Render(" "+approvalReason(d.out.Reason)), contentWidth, 3))
	}
	if d.out.SandboxAbsent {
		writeLine(th.warn.Render(" FULL HOST ACCESS: this command is not sandboxed; it can access files outside the workspace and the network"))
	}
	if d.req.Effect == permission.EffectExecute && d.req.Network {
		writeLine(th.warn.Render(" FULL NETWORK ACCESS REQUESTED: this command can send workspace data off this machine"))
	}
	if d.req.Effect == permission.EffectExternal {
		writeLine(th.warn.Render(" EXTERNAL EFFECT: this tool runs outside Switchboard's sandbox and may send data or change remote state"))
	}
	b.WriteString("\n")

	always := "yes, and don't ask again for this exact command"
	if d.req.Effect == permission.EffectExternal {
		// The remembered answer for an external tool covers the tool, not one
		// byte-exact invocation; the label has to say what it grants. A web
		// tool carries the host in its path, so its remembered answer is
		// per-host and the label says the host.
		always = "yes, and allow this tool for the rest of the session"
		if d.req.Path != "" {
			always = "yes, and allow " + terminaltext.Escape(d.req.Path) + " for the rest of the session"
		}
	}
	// The border states the stakes: accent for a routine ask, amber the moment
	// the command leaves the sandbox. Color is information here, not chrome,
	// and the selection bar speaks the same color as the frame.
	frame := th.accent
	if d.out.SandboxAbsent || (d.req.Effect == permission.EffectExecute && d.req.Network) || d.req.Effect == permission.EffectExternal {
		frame = th.warn
	}
	options := []string{
		"no · safe default",
		"yes",
		always,
	}
	if height <= 10 {
		identity := fitCells(th.bold.Render(" "+terminaltext.Escape(d.req.Tool))+" "+th.dim.Render(desc), contentWidth)
		consequence := ""
		switch {
		case d.out.SandboxAbsent:
			consequence = "HOST ACCESS"
		case d.req.Effect == permission.EffectExecute && d.req.Network:
			consequence = "NETWORK EGRESS"
		case d.req.Effect == permission.EffectExternal:
			consequence = "EXTERNAL EFFECT"
		}
		compactAlways := "allow exact"
		if d.req.Effect == permission.EffectExternal {
			compactAlways = "allow sess."
		}
		compactOptions := []string{"no · safe", "yes once", compactAlways}
		lines := []string{identity}
		identityLine := 0
		consequenceLine := -1
		if consequence != "" {
			consequenceLine = len(lines)
			lines = append(lines, th.warn.Render(fitCells(consequence, contentWidth)))
		}
		defaultLine := len(lines)
		focus := defaultLine + d.sel
		for i, option := range compactOptions {
			prefix := "  "
			style := th.dim
			if i == d.sel {
				prefix = th.accent.Render("▌ ")
				style = th.bold
			}
			lines = append(lines, fitCells(prefix+style.Render(option), contentWidth))
		}
		hintLine := len(lines)
		lines = append(lines, th.faint.Render(fitCells("↑↓ · enter · esc no", contentWidth)))
		mandatory := []int{identityLine}
		if consequenceLine >= 0 {
			mandatory = append(mandatory, consequenceLine)
		}
		mandatory = append(mandatory, defaultLine, hintLine)
		contentHeight := max(height-2, 1)
		content := strings.Join(dialogWindow(lines, contentHeight, focus, mandatory...), "\n")
		box := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(frame.GetForeground()).
			Padding(0, 1).
			Width(boxWidth)
		return box.Render(content)
	}
	for i, opt := range options {
		if i == d.sel {
			writeLine(wrapCellsBounded(frame.Render(" ▌ ")+th.bold.Render(opt), contentWidth, 3))
		} else {
			writeLine(wrapCellsBounded(th.dim.Render("   "+opt), contentWidth, 3))
		}
	}
	b.WriteString(wrapCells(th.faint.Render(" ↑↓ choose · enter confirm · n/esc no"), contentWidth))
	content := strings.TrimRight(b.String(), "\n")
	lines := strings.Split(content, "\n")
	focus := dialogLine(lines, "▌", true)
	defaultLine := dialogLine(lines, "safe", false)
	mandatory := []int{defaultLine}
	if height-2 < len(lines) {
		consequence := ""
		switch {
		case d.out.SandboxAbsent:
			consequence = " HOST ACCESS · no sandbox"
		case d.req.Effect == permission.EffectExecute && d.req.Network:
			consequence = " NETWORK EGRESS"
		case d.req.Effect == permission.EffectExternal:
			consequence = " EXTERNAL EFFECT"
		}
		if consequence != "" {
			compact := th.warn.Render(fitCells(consequence, contentWidth))
			lines = append([]string{compact}, lines...)
			focus++
			defaultLine++
			mandatory = []int{0, defaultLine}
			content = strings.Join(lines, "\n")
		}
	}
	content = boundedBoxContent(content, height, focus, mandatory...)
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(frame.GetForeground()).
		Padding(0, 1).
		Width(boxWidth)
	return box.Render(content)
}

func borderColor(th *theme) lipgloss.Color {
	if th.dark {
		return lipgloss.Color("238")
	}
	return lipgloss.Color("250")
}

// pickerDialog is the generic chooser behind /tier, /resume, /mode, and
// /theme.
type pickerItem struct {
	id      string
	label   string
	desc    string
	current bool
}

type pickerDialog struct {
	title string
	items []pickerItem
	sel   int
	// navigationOnly is for consequential confirmations that may arrive while
	// ordinary composer text is in flight. Their choices can be reached only
	// with arrows, so a typed filter can never turn a later Enter into egress.
	navigationOnly bool
	// requireSelection leaves an asynchronously opened picker neutral until
	// the user navigates or types a filter. It prevents a delayed result from
	// turning an unrelated Enter into an action on the first row.
	requireSelection bool
	query            string
	onPick           func(id string) tea.Cmd
	onCancel         func() tea.Cmd
}

const pickerQueryMaxRunes = 128

type pickerMatch struct {
	item  pickerItem
	index int
	score int
}

func (d *pickerDialog) update(key tea.KeyMsg, th *theme) (bool, tea.Cmd) {
	matches := d.matches()
	d.clampSelection(len(matches))
	switch key.String() {
	case "esc":
		return true, d.cancel()
	case "up":
		if d.sel < 0 && len(matches) > 0 {
			d.sel = 0
		} else if d.sel > 0 {
			d.sel--
		}
	case "down":
		if d.sel < 0 && len(matches) > 0 {
			d.sel = 0
		} else if d.sel < len(matches)-1 {
			d.sel++
		}
	case "backspace":
		runes := []rune(d.query)
		if len(runes) > 0 {
			d.setQuery(string(runes[:len(runes)-1]))
		}
	case "ctrl+w":
		d.setQuery(trimLastPickerWord(d.query))
	case "ctrl+u":
		d.setQuery("")
	case "enter":
		if d.sel >= 0 && d.sel < len(matches) {
			if d.onPick == nil {
				return true, nil
			}
			return true, d.onPick(matches[d.sel].item.id)
		}
		return false, nil
	default:
		if d.navigationOnly {
			return false, nil
		}
		if key.Type == tea.KeyRunes {
			d.appendQuery(key.Runes)
		} else if key.Type == tea.KeySpace {
			d.appendQuery([]rune{' '})
		}
	}
	return false, nil
}

func (d *pickerDialog) cancel() tea.Cmd {
	if d.onCancel != nil {
		return d.onCancel()
	}
	return nil
}

func (d *pickerDialog) view(width int, th *theme) string {
	return d.viewWithin(width, dialogUnlimitedHeight, th)
}

func (d *pickerDialog) viewWithin(width, height int, th *theme) string {
	if width < 1 {
		return ""
	}
	const maxRows = 10
	matches := d.matches()
	d.clampSelection(len(matches))
	showSpacer := !d.navigationOnly && height >= 8
	fixedRows := 2 // title and hint
	if !d.navigationOnly {
		fixedRows++ // search
	}
	if showSpacer {
		fixedRows++
	}
	availableRows := max(height-fixedRows, 1)
	rowBudget := min(maxRows, availableRows)
	showFold := len(matches) > rowBudget
	if showFold && rowBudget > 1 && availableRows <= maxRows {
		rowBudget-- // keep pagination visible instead of clipping a result later
	}
	start := 0
	if d.sel >= rowBudget {
		start = d.sel - rowBudget + 1
	}
	end := start + rowBudget
	if end > len(matches) {
		end = len(matches)
	}

	markerWidth := min(3, width)
	contentWidth := max(width-markerWidth, 0)
	labelWidth := 0
	hasDescription := false
	for i := start; i < end; i++ {
		item := matches[i].item
		if cells := lipgloss.Width(terminaltext.Escape(item.label)); cells > labelWidth {
			labelWidth = cells
		}
		if item.desc != "" {
			hasDescription = true
		}
	}
	gapWidth := 0
	descWidth := 0
	if hasDescription && contentWidth >= 20 {
		labelWidth = min(labelWidth, max(contentWidth/2, 8))
		gapWidth = 2
		descWidth = max(contentWidth-labelWidth-gapWidth, 0)
	} else {
		labelWidth = min(labelWidth, contentWidth)
	}

	var b strings.Builder
	title := fitCells(" "+terminaltext.Escape(d.title), width)
	b.WriteString(th.bold.Render(title) + "\n")
	if !d.navigationOnly {
		search := th.accent.Render(" search ") + " " + terminaltext.Escape(d.query) + th.faint.Render("▌")
		b.WriteString(fitCells(search, width) + "\n")
		if showSpacer {
			b.WriteString("\n")
		}
	}
	if len(matches) == 0 {
		b.WriteString(th.dim.Render(fitCells(" no matches", width)) + "\n")
	}
	for i := start; i < end; i++ {
		it := matches[i].item
		label := padRight(fitCells(terminaltext.Escape(it.label), labelWidth), labelWidth)
		desc := ""
		if descWidth > 0 {
			desc = padRight(fitCells(terminaltext.Escape(it.desc), descWidth), descWidth)
		} else {
			label = padRight(label, contentWidth)
		}
		gap := strings.Repeat(" ", gapWidth)
		marker := "   "
		if it.current {
			marker = th.ok.Render(" ● ")
		}
		marker = fitCells(marker, markerWidth)
		row := marker + label + gap + th.dim.Render(desc)
		if i == d.sel {
			selectedMarker := " ▌ "
			if markerWidth == 1 {
				selectedMarker = "▌"
			} else if markerWidth == 2 {
				selectedMarker = "▌ "
			}
			row = fitCells(th.accent.Render(selectedMarker), markerWidth) +
				th.bold.Render(label) + gap + th.dim.Render(desc)
		}
		b.WriteString(fitCells(row, width) + "\n")
	}
	// A list cut off at the viewport with nothing to say so reads as the
	// whole list, and the row someone came for sits below the fold unfound.
	if showFold {
		before, after := start, len(matches)-end
		fold := ""
		switch {
		case before > 0 && after > 0:
			fold = fmt.Sprintf("   ↑%d · ↓%d more", before, after)
		case before > 0:
			fold = fmt.Sprintf("   ↑ %d earlier", before)
		default:
			fold = fmt.Sprintf("   ↓ %d more", after)
		}
		b.WriteString(th.dim.Render(fitCells(fold, width)) + "\n")
	}
	hint := " type to filter · ↑↓ choose · enter select · esc cancel"
	if d.navigationOnly {
		hint = " ↑↓ choose · enter confirm · esc cancel"
	}
	if showFold {
		hint = fmt.Sprintf(" %d-%d of %d · %s", start+1, end, len(matches), strings.TrimSpace(hint))
	}
	b.WriteString(th.faint.Render(fitCells(hint, width)))
	lines := strings.Split(b.String(), "\n")
	focus := dialogLine(lines, "▌", true) // the selected row follows the search cursor
	return strings.Join(dialogWindow(lines, height, focus, 0, len(lines)-1), "\n")
}

func (d *pickerDialog) appendQuery(typed []rune) {
	query := []rune(d.query)
	remaining := pickerQueryMaxRunes - len(query)
	if remaining <= 0 {
		return
	}
	for _, r := range typed {
		if remaining == 0 {
			break
		}
		if !unicode.IsPrint(r) {
			continue
		}
		query = append(query, r)
		remaining--
	}
	d.setQuery(string(query))
}

func (d *pickerDialog) setQuery(query string) {
	before := d.matches()
	selected := -1
	if d.sel >= 0 && d.sel < len(before) {
		selected = before[d.sel].index
	}

	d.query = query
	after := d.matches()
	if selected >= 0 {
		for i := range after {
			if after[i].index == selected {
				d.sel = i
				return
			}
		}
	}
	// Whitespace and filter-clearing chords are common composer keystrokes.
	// When an async picker arrives between them and Enter, an empty filter must
	// stay neutral rather than silently arming its first row.
	if d.requireSelection && strings.TrimSpace(query) == "" {
		d.sel = -1
		return
	}
	d.sel = 0
	d.clampSelection(len(after))
}

func (d *pickerDialog) clampSelection(n int) {
	if n == 0 {
		if d.requireSelection {
			d.sel = -1
		} else {
			d.sel = 0
		}
		return
	}
	if d.sel < 0 {
		if d.requireSelection {
			return
		}
		d.sel = 0
	}
	if d.sel >= n {
		d.sel = n - 1
	}
}

func (d *pickerDialog) matches() []pickerMatch {
	query := strings.TrimSpace(strings.ToLower(d.query))
	matches := make([]pickerMatch, 0, len(d.items))
	for i, item := range d.items {
		score, ok := pickerItemScore(query, item)
		if !ok {
			continue
		}
		matches = append(matches, pickerMatch{item: item, index: i, score: score})
	}
	if query != "" {
		sort.SliceStable(matches, func(i, j int) bool {
			if matches[i].score != matches[j].score {
				return matches[i].score < matches[j].score
			}
			return matches[i].index < matches[j].index
		})
	}
	return matches
}

func pickerItemScore(query string, item pickerItem) (int, bool) {
	if query == "" {
		return 0, true
	}
	fields := []struct {
		text   string
		weight int
	}{
		{strings.ToLower(item.id), 0},
		{strings.ToLower(item.label), 8},
		{strings.ToLower(item.desc), 24},
	}

	total := 0
	for _, term := range strings.Fields(query) {
		best := -1
		for _, field := range fields {
			if score, ok := pickerFieldScore(term, field.text); ok {
				score += field.weight
				if best < 0 || score < best {
					best = score
				}
			}
		}
		if best < 0 {
			return 0, false
		}
		total += best
	}
	return total, true
}

func pickerFieldScore(query, field string) (int, bool) {
	if query == field {
		return 0, true
	}
	if strings.HasPrefix(field, query) {
		return 20 + len([]rune(field)) - len([]rune(query)), true
	}
	if at := strings.Index(field, query); at >= 0 {
		return 100 + at*2 + len([]rune(field)) - len([]rune(query)), true
	}

	queryRunes := []rune(query)
	fieldRunes := []rune(field)
	qi := 0
	first := -1
	last := -1
	for i, r := range fieldRunes {
		if qi >= len(queryRunes) || r != queryRunes[qi] {
			continue
		}
		if first < 0 {
			first = i
		}
		last = i
		qi++
	}
	if qi != len(queryRunes) {
		return 0, false
	}
	span := last - first + 1
	gaps := span - len(queryRunes)
	return 300 + first*2 + gaps*4 + len(fieldRunes) - len(queryRunes), true
}

func trimLastPickerWord(query string) string {
	runes := []rune(query)
	for len(runes) > 0 && unicode.IsSpace(runes[len(runes)-1]) {
		runes = runes[:len(runes)-1]
	}
	for len(runes) > 0 && !unicode.IsSpace(runes[len(runes)-1]) {
		runes = runes[:len(runes)-1]
	}
	return string(runes)
}

// textPromptMsg opens the text dialog. It is a message rather than a direct
// assignment for the same reason secretPromptMsg is: the picker that asked for
// it is mid-update, and its close would null the dialog out again.
type textPromptMsg struct {
	title      string
	help       string
	initial    string
	generation uint64
	sessionID  string

	// submit runs with the trimmed entry. An empty entry cancels, so a
	// caller never has to decide what an empty string meant.
	submit func(value string) tea.Cmd

	// allowEmpty makes an empty entry an answer rather than a cancellation,
	// for the prompt whose field is genuinely optional. An address or a model
	// id is not; a skill's arguments are.
	allowEmpty bool
}

// textDialog takes one line of visible text: a server address, a model id.
// It is deliberately not secretDialog with the echo turned back on — a
// dialog that sometimes hides what is typed and sometimes does not is one
// mistake away from showing a key.
type textDialog struct {
	title      string
	help       string
	input      textinput.Model
	submit     func(value string) tea.Cmd
	allowEmpty bool
}

func newTextDialog(msg textPromptMsg) *textDialog {
	ti := textinput.New()
	ti.Prompt = ""
	ti.SetValue(msg.initial)
	ti.CursorEnd()
	ti.Focus()
	return &textDialog{
		title: msg.title, help: msg.help, input: ti,
		submit: msg.submit, allowEmpty: msg.allowEmpty,
	}
}

func (d *textDialog) update(key tea.KeyMsg, th *theme) (bool, tea.Cmd) {
	switch key.String() {
	case "esc":
		return true, d.cancel()
	case "enter":
		value := strings.TrimSpace(d.input.Value())
		if value == "" && !d.allowEmpty {
			return true, nil
		}
		return true, d.submit(value)
	}
	var cmd tea.Cmd
	d.input, cmd = d.input.Update(key)
	return false, cmd
}

func (d *textDialog) cancel() tea.Cmd {
	d.input.Reset()
	return nil
}

func (d *textDialog) view(width int, th *theme) string {
	return d.viewWithin(width, dialogUnlimitedHeight, th)
}

func (d *textDialog) viewWithin(width, height int, th *theme) string {
	boxWidth, contentWidth := dialogDimensions(width)
	d.input.Width = max(contentWidth-1, 1)
	d.input.SetCursor(d.input.Position())
	if height <= 10 {
		lines := []string{fitCells(th.bold.Render(" "+terminaltext.Escape(d.title)), contentWidth)}
		titleLine := 0
		if d.help != "" {
			lines = append(lines, fitCells(th.dim.Render(" "+terminaltext.Escape(d.help)), contentWidth))
		}
		inputLine := len(lines)
		lines = append(lines, fitCells(th.accent.Render("▌ ")+safeTextInputView(d.input), contentWidth))
		footer := "enter save · esc cancel"
		if d.allowEmpty {
			footer = "enter continue · esc"
		}
		footerLine := len(lines)
		lines = append(lines, th.faint.Render(fitCells(footer, contentWidth)))
		contentHeight := max(height-2, 1)
		content := strings.Join(dialogWindow(lines, contentHeight, inputLine, titleLine, footerLine), "\n")
		box := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(borderColor(th)).
			Padding(0, 1).
			Width(boxWidth)
		return box.Render(content)
	}

	var b strings.Builder
	b.WriteString(wrapCellsBounded(th.bold.Render(" "+terminaltext.Escape(d.title)), contentWidth, 2) + "\n")
	if d.help != "" {
		b.WriteString(wrapCellsBounded(th.dim.Render(" "+terminaltext.Escape(d.help)), contentWidth, 4) + "\n")
	}
	b.WriteString("\n" + wrapCells(" "+safeTextInputView(d.input), contentWidth) + "\n")
	footer := " enter save · esc cancel"
	if d.allowEmpty {
		footer = " enter continue · esc cancel"
	}
	b.WriteString(wrapCells(th.faint.Render(footer), contentWidth))

	content := b.String()
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	inputLine := max(len(lines)-2, 0)
	content = boundedBoxContent(content, height, inputLine, 0, len(lines)-1)
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor(th)).
		Padding(0, 1).
		Width(boxWidth)
	return box.Render(content)
}
