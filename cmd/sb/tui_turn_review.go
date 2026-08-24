package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/terminaltext"
)

type turnReviewLoadedMsg struct {
	sessionID  string
	generation uint64
	turnEpoch  uint64
	recorder   *checkpoint.Recorder
	cursor     checkpoint.ReviewCursor
	bound      bool
	index      int
	label      string
	lines      []string
	err        error
}

// cmdReview loads checkpoint evidence away from Bubble Tea's update loop.
// The command is intentionally not busy-safe: a running turn may still be
// advancing the checkpoint that an omitted turn number means to inspect.
func cmdReview(m *tuiModel, args string) tea.Cmd {
	turn, ok := parseTurnReviewArgs(args)
	if !ok {
		return noticeCmd("error", "usage: /review [turn]")
	}
	if m == nil || m.app == nil {
		return func() tea.Msg {
			return turnReviewLoadedMsg{err: errors.New("turn review is unavailable: no active session")}
		}
	}

	recorder := m.app.undo
	workspace := m.app.workspace
	dark := m.th != nil && m.th.dark
	m.workspaceGeneration++
	generation := m.workspaceGeneration
	sessionID := currentSessionID(m)
	turnEpoch := m.turnGeneration
	cursor, index, selectionErr := selectTurnReview(recorder, turn)
	return func() tea.Msg {
		base := turnReviewLoadedMsg{
			sessionID: sessionID, generation: generation, turnEpoch: turnEpoch,
			recorder: recorder, cursor: cursor, bound: selectionErr == nil,
		}
		if selectionErr != nil {
			base.err = selectionErr
			return base
		}
		review, err := loadTurnReviewCursor(recorder, cursor, index, workspace)
		if err != nil {
			base.err = err
			return base
		}
		// Render is already terminal-safe. Display is deliberately repeated at
		// this presentation boundary before Chroma contributes its own trusted
		// SGR sequences; source bytes never become terminal control.
		text := terminaltext.Display(review.Render(0, 0))
		base.index = review.Index
		base.label = review.Label
		base.lines = highlightDiff(text, dark)
		return base
	}
}

func parseTurnReviewArgs(args string) (int, bool) {
	args = strings.TrimSpace(args)
	if args == "" {
		return 0, true
	}
	fields := strings.Fields(args)
	if len(fields) != 1 || fields[0] != args {
		return 0, false
	}
	for _, r := range args {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	turn, err := strconv.Atoi(args)
	return turn, err == nil && turn > 0
}

// turnReviewView is a read-only checkpoint record. Its only actions navigate
// or close the panel; it deliberately has no file, editor, undo, or hunk key.
type turnReviewView struct {
	index int
	label string
	lines []string

	visual      []string
	visualWidth int
	offset      int
}

func (v *turnReviewView) key(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		return true, nil
	case "up", "k":
		v.scroll(-1)
	case "down", "j":
		v.scroll(1)
	case "pgup", "ctrl+u":
		v.scroll(-20)
	case "pgdown", "ctrl+d":
		v.scroll(20)
	case "g":
		v.offset = 0
	case "G":
		v.offset = len(v.visual)
		if v.offset == 0 {
			v.offset = len(v.lines)
		}
		v.scroll(0)
	}
	return false, nil
}

func (v *turnReviewView) mouse(msg tea.MouseMsg) tea.Cmd {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		v.scroll(-3)
	case tea.MouseButtonWheelDown:
		v.scroll(3)
	}
	return nil
}

func (v *turnReviewView) scroll(delta int) {
	v.offset += delta
	if v.offset < 0 {
		v.offset = 0
	}
}

func (v *turnReviewView) view(width, height int, th *theme) string {
	if width < 1 || height < 1 {
		return ""
	}
	if th == nil {
		th = darkTheme()
	}

	title := workspaceFit(fmt.Sprintf(" turn %d agent mutations · read-only", v.index), width)
	hint := workspaceFit(" /diff is repository vs HEAD · ↑↓ scroll · pgup/pgdn · g/G ends · esc/q close", width)
	header := []string{th.bold.Render(title)}
	if height > 1 {
		header = append(header, th.faint.Render(hint))
	}

	footerRows := 0
	if height-len(header) > 1 {
		footerRows = 1
	}
	bodyHeight := max(height-len(header)-footerRows, 0)
	visual := v.visualLines(width)
	if maximum := max(len(visual)-bodyHeight, 0); v.offset > maximum {
		v.offset = maximum
	}
	if v.offset < 0 {
		v.offset = 0
	}

	end := min(v.offset+bodyHeight, len(visual))
	visible := append([]string(nil), visual[v.offset:end]...)
	for len(visible) < bodyHeight {
		visible = append(visible, "")
	}

	rows := append(header, visible...)
	if footerRows == 1 {
		footer := ""
		if len(visual) > bodyHeight && len(visual) > 0 {
			percent := min((v.offset+bodyHeight)*100/len(visual), 100)
			footer = th.faint.Render(workspaceFit(" "+itoa(percent)+"%", width))
		}
		rows = append(rows, footer)
	}
	if len(rows) > height {
		rows = rows[:height]
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

func (v *turnReviewView) visualLines(width int) []string {
	width = max(width, 1)
	if v.visual != nil && v.visualWidth == width {
		return v.visual
	}
	visual := make([]string, 0, len(v.lines))
	for _, line := range v.lines {
		line = strings.ReplaceAll(line, "\t", `\t`)
		wrapped := ansi.Hardwrap(line, width, true)
		rows := strings.Split(wrapped, "\n")
		for i := range rows {
			rows[i] = fitCells(rows[i], width)
		}
		rows = independentSGRLines(rows, strings.Contains(line, "\x1b["))
		visual = append(visual, rows...)
	}
	v.visual = visual
	v.visualWidth = width
	return v.visual
}

var _ fullscreen = (*turnReviewView)(nil)
