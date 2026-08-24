package main

import (
	"context"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/switchboard-code/switchboard/internal/terminaltext"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// questionMsg carries the ask tool's question from the loop's goroutine into
// the program, the permission ask's pattern: the loop blocks on respond
// until the user answers or the turn is cancelled.
type questionMsg struct {
	q       tools.Question
	respond chan tools.Answer
	token   *dialogToken
}

// tuiQuestioner resolves the ask tool against a dialog. A program that has
// already quit leaves no one to answer, and the cancelled context is what
// unblocks the loop — never a fabricated answer.
type tuiQuestioner struct {
	p            tuiMessageSender
	lifetimeDone <-chan struct{}
}

func (a *tuiQuestioner) AskUser(ctx context.Context, q tools.Question) (tools.Answer, error) {
	if tuiLifetimeStopped(a.lifetimeDone) {
		return tools.Answer{}, tuiWaitCancellation(ctx)
	}
	respond := make(chan tools.Answer, 1)
	token := &dialogToken{}
	a.p.Send(questionMsg{q: q, respond: respond, token: token})
	select {
	case ans := <-respond:
		if tuiLifetimeStopped(a.lifetimeDone) || ctx.Err() != nil {
			return tools.Answer{}, tuiWaitCancellation(ctx)
		}
		return ans, nil
	case <-ctx.Done():
		a.p.Send(cancelDialogMsg{token: token})
		return tools.Answer{}, ctx.Err()
	case <-a.lifetimeDone:
		return tools.Answer{}, tuiWaitCancellation(ctx)
	}
}

// questionDialog is the ask tool's face: the options, a row for an answer of
// the user's own, and esc as the decline. Declining resolves rather than
// dismissing, because the model is blocked mid-turn on this channel and a
// closed dialog that answered nothing would hang the turn.
type questionDialog struct {
	q       tools.Question
	respond chan tools.Answer
	sel     int
	marked  []bool
	typing  bool
	input   textinput.Model
}

func newQuestionDialog(q tools.Question, respond chan tools.Answer) *questionDialog {
	ti := textinput.New()
	ti.Prompt = ""
	return &questionDialog{
		q:       q,
		respond: respond,
		sel:     -1,
		marked:  make([]bool, len(q.Options)),
		input:   ti,
	}
}

// otherRow is the index of the type-your-own row, one past the options.
func (d *questionDialog) otherRow() int { return len(d.q.Options) }

func (d *questionDialog) resolve(ans tools.Answer) bool {
	select {
	case d.respond <- ans:
	default:
	}
	return true
}

// picks collects the marked labels in the order they were offered, so the
// model reads the answer in the shape it asked the question.
func (d *questionDialog) picks() []string {
	var out []string
	for i, opt := range d.q.Options {
		if d.marked[i] {
			out = append(out, opt.Label)
		}
	}
	return out
}

func (d *questionDialog) update(key tea.KeyMsg, th *theme) (bool, tea.Cmd) {
	if d.typing {
		switch key.String() {
		case "esc":
			d.typing = false
			d.input.Reset()
			return false, nil
		case "enter":
			text := strings.TrimSpace(d.input.Value())
			if text == "" {
				d.typing = false
				d.input.Reset()
				return false, nil
			}
			return d.resolve(tools.Answer{Text: text}), nil
		}
		var cmd tea.Cmd
		d.input, cmd = d.input.Update(key)
		return false, cmd
	}

	switch key.String() {
	case "esc":
		return true, d.cancel()
	case "up":
		if d.sel < 0 {
			d.sel = 0
		} else if d.sel > 0 {
			d.sel--
		}
	case "down":
		if d.sel < 0 {
			d.sel = 0
		} else if d.sel < d.otherRow() {
			d.sel++
		}
	case " ":
		if d.q.Multi && d.sel >= 0 && d.sel < len(d.q.Options) {
			d.marked[d.sel] = !d.marked[d.sel]
		}
	case "enter":
		// Questions can appear between two keystrokes while the model is
		// working. Enter is inert until navigation makes an answer explicit.
		if d.sel < 0 {
			return false, nil
		}
		if d.sel == d.otherRow() {
			d.typing = true
			d.input.Focus()
			return false, textinput.Blink
		}
		if d.q.Multi {
			// Marks win when any exist; enter on a bare list answers with
			// the highlighted option, so a single-answer user of a multi
			// question is never forced through the marking step.
			if picked := d.picks(); len(picked) > 0 {
				return d.resolve(tools.Answer{Picked: picked}), nil
			}
		}
		return d.resolve(tools.Answer{Picked: []string{d.q.Options[d.sel].Label}}), nil
	}
	return false, nil
}

func (d *questionDialog) cancel() tea.Cmd {
	d.typing = false
	d.input.Reset()
	d.resolve(tools.Answer{Declined: true})
	return nil
}

func (d *questionDialog) view(width int, th *theme) string {
	return d.viewWithin(width, dialogUnlimitedHeight, th)
}

func (d *questionDialog) viewWithin(width, height int, th *theme) string {
	boxWidth, contentWidth := dialogDimensions(width)
	d.input.Width = max(contentWidth-3, 1)
	d.input.SetCursor(d.input.Position())
	if height <= 10 {
		lines := []string{fitCells(th.bold.Render(" "+terminaltext.Escape(d.q.Question)), contentWidth)}
		questionLine := 0
		focus := 0
		if d.typing {
			focus = len(lines)
			lines = append(lines, fitCells(th.accent.Render("▌ ")+safeTextInputView(d.input), contentWidth))
			lines = append(lines, th.faint.Render(fitCells("enter answer · esc back", contentWidth)))
		} else {
			firstChoice := len(lines)
			for i, opt := range d.q.Options {
				label := terminaltext.Escape(opt.Label)
				if opt.Detail != "" {
					label += " · " + terminaltext.Escape(opt.Detail)
				}
				if d.q.Multi {
					mark := "[ ] "
					if d.marked[i] {
						mark = "[x] "
					}
					label = mark + label
				}
				prefix := "  "
				style := th.dim
				if i == d.sel {
					prefix = th.accent.Render("▌ ")
					style = th.bold
					focus = len(lines)
				}
				lines = append(lines, fitCells(prefix+style.Render(label), contentWidth))
			}
			otherLine := len(lines)
			prefix := "  "
			style := th.dim
			if d.sel == d.otherRow() {
				prefix = th.accent.Render("▌ ")
				style = th.bold
				focus = otherLine
			}
			lines = append(lines, fitCells(prefix+style.Render("type your own answer"), contentWidth))
			if d.sel < 0 {
				focus = firstChoice
			}
			hint := "esc decline · ↑↓ · enter"
			if d.q.Multi {
				hint = "esc decline · ↑↓ · space"
			}
			lines = append(lines, th.faint.Render(fitCells(hint, contentWidth)))
		}
		hintLine := len(lines) - 1
		contentHeight := max(height-2, 1)
		content := strings.Join(dialogWindow(lines, contentHeight, focus, questionLine, hintLine), "\n")
		box := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(th.accent.GetForeground()).
			Padding(0, 1).
			Width(boxWidth)
		return box.Render(content)
	}

	var b strings.Builder
	writeLine := func(line string) {
		b.WriteString(wrapCells(line, contentWidth))
		b.WriteByte('\n')
	}
	writeBounded := func(line string, maxLines int) {
		b.WriteString(wrapCellsBounded(line, contentWidth, maxLines))
		b.WriteByte('\n')
	}
	writeBounded(th.bold.Render(" "+terminaltext.Escape(d.q.Question)), 4)
	b.WriteByte('\n')

	for i, opt := range d.q.Options {
		label := terminaltext.Escape(opt.Label)
		detail := terminaltext.Escape(opt.Detail)
		mark := ""
		if d.q.Multi {
			mark = "[ ] "
			if d.marked[i] {
				mark = "[x] "
			}
		}
		row := mark + label
		if detail != "" {
			row += "  " + th.dim.Render(detail)
		}
		if i == d.sel && !d.typing {
			row = th.accent.Render(" ▌ ") + th.bold.Render(mark+label)
			if detail != "" {
				row += "  " + th.dim.Render(detail)
			}
			writeBounded(row, 2)
		} else {
			writeBounded(th.dim.Render("   ")+row, 2)
		}
	}

	if d.typing {
		writeLine(th.accent.Render(" ▌ ") + safeTextInputView(d.input))
		b.WriteString(wrapCells(th.faint.Render(" enter answer · esc back to the options"), contentWidth))
	} else {
		other := "type your own answer"
		if d.sel == d.otherRow() {
			writeLine(th.accent.Render(" ▌ ") + th.bold.Render(other))
		} else {
			writeLine(th.dim.Render("   " + other))
		}
		hint := " ↑↓ choose · enter answer · esc decline"
		if d.q.Multi {
			hint = " ↑↓ choose · space mark · enter answer · esc decline"
		}
		b.WriteString(wrapCells(th.faint.Render(hint), contentWidth))
	}

	content := strings.TrimRight(b.String(), "\n")
	lines := strings.Split(content, "\n")
	focus := dialogLine(lines, "▌", true)
	if focus < 0 {
		focus = max(len(lines)-2, 0)
	}
	content = boundedBoxContent(content, height, focus, 0, len(lines)-1)
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(th.accent.GetForeground()).
		Padding(0, 1).
		Width(boxWidth)
	return box.Render(content)
}
