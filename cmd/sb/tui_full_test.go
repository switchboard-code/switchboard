package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/tools"
)

type fullscreenProbe struct {
	close bool
	cmd   tea.Cmd

	gotKey    string
	gotMouse  tea.MouseMsg
	viewW     int
	viewH     int
	viewTheme *theme
}

func (p *fullscreenProbe) key(msg tea.KeyMsg) (bool, tea.Cmd) {
	p.gotKey = msg.String()
	return p.close, p.cmd
}

func (p *fullscreenProbe) mouse(msg tea.MouseMsg) tea.Cmd {
	p.gotMouse = msg
	return p.cmd
}

func (p *fullscreenProbe) view(width, height int, th *theme) string {
	p.viewW, p.viewH, p.viewTheme = width, height, th
	return "fullscreen"
}

func TestFullscreenOwnsKeyAndCanReturnCommand(t *testing.T) {
	type panelMsg struct{}
	probe := &fullscreenProbe{
		cmd: func() tea.Msg { return panelMsg{} },
	}
	m := &tuiModel{full: probe}

	cmd := m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if probe.gotKey != "x" {
		t.Fatalf("fullscreen key = %q, want x", probe.gotKey)
	}
	if m.full != probe {
		t.Fatal("fullscreen closed while returning a command")
	}
	if cmd == nil {
		t.Fatal("fullscreen command was dropped")
	}
	got := cmd()
	if _, ok := got.(panelMsg); !ok {
		t.Fatalf("fullscreen command returned %T, want panelMsg", got)
	}

	probe.close, probe.cmd = true, nil
	if cmd := m.key(tea.KeyMsg{Type: tea.KeyEsc}); cmd != nil {
		t.Fatalf("closing fullscreen command = %v, want nil", cmd)
	}
	if m.full != nil {
		t.Fatal("fullscreen remained open after requesting close")
	}
}

func TestFullscreenOwnsMouseAndView(t *testing.T) {
	th := darkTheme()
	probe := &fullscreenProbe{cmd: func() tea.Msg { return struct{}{} }}
	m := &tuiModel{full: probe, width: 91, height: 27, th: th}

	msg := tea.MouseMsg{Button: tea.MouseButtonWheelDown}
	updated, cmd := m.Update(msg)
	if updated != m || cmd == nil {
		t.Fatalf("mouse update = (%T, %v), want original model and panel command", updated, cmd)
	}
	if probe.gotMouse.Button != tea.MouseButtonWheelDown {
		t.Fatalf("fullscreen mouse button = %v, want wheel down", probe.gotMouse.Button)
	}
	if got := m.View(); got != "fullscreen" {
		t.Fatalf("View() = %q, want fullscreen", got)
	}
	if probe.viewW != 91 || probe.viewH != 27 || probe.viewTheme != th {
		t.Fatalf("fullscreen view args = (%d, %d, %p), want (91, 27, %p)", probe.viewW, probe.viewH, probe.viewTheme, th)
	}
}

func TestModalPreemptsAnOpenFullscreenPanel(t *testing.T) {
	m := testModel(t)
	probe := &fullscreenProbe{}
	m.full = probe
	respond := make(chan tools.Answer, 1)
	m.Update(questionMsg{q: tools.Question{
		Question: "continue with the migration?",
		Options:  []tools.QuestionOption{{Label: "continue"}, {Label: "stop"}},
	}, respond: respond})

	view := m.View()
	if !strings.Contains(view, "continue with the migration?") || view == "fullscreen" {
		t.Fatalf("the modal stayed hidden behind fullscreen:\n%s", view)
	}
	// The asynchronously opened question starts neutral; navigation is the
	// explicit selection that makes Enter an answer.
	m.key(tea.KeyMsg{Type: tea.KeyDown})
	if cmd := m.key(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatalf("answer returned unexpected command %v", cmd)
	}
	select {
	case answer := <-respond:
		if len(answer.Picked) != 1 || answer.Picked[0] != "continue" {
			t.Fatalf("answer = %+v, want continue", answer)
		}
	default:
		t.Fatal("the modal did not receive the key; the loop would remain blocked")
	}
	if probe.gotKey != "" {
		t.Fatalf("fullscreen received modal key %q", probe.gotKey)
	}

	// The panel remains available after the modal closes; preemption is not a
	// destructive close.
	if m.full != probe || m.View() != "fullscreen" {
		t.Fatal("answering the modal discarded the fullscreen panel")
	}
}

func TestDiffViewFullscreenAdapterPreservesInputBehavior(t *testing.T) {
	var _ fullscreen = (*diffView)(nil)

	tests := []struct {
		name        string
		key         tea.KeyMsg
		start, want int
		close       bool
	}{
		{name: "escape closes", key: tea.KeyMsg{Type: tea.KeyEsc}, start: 7, want: 7, close: true},
		{name: "q closes", key: runeKey('q'), start: 7, want: 7, close: true},
		{name: "up", key: tea.KeyMsg{Type: tea.KeyUp}, start: 7, want: 6},
		{name: "k", key: runeKey('k'), start: 7, want: 6},
		{name: "down", key: tea.KeyMsg{Type: tea.KeyDown}, start: 7, want: 8},
		{name: "j", key: runeKey('j'), start: 7, want: 8},
		{name: "page up", key: tea.KeyMsg{Type: tea.KeyPgUp}, start: 25, want: 5},
		{name: "control u", key: tea.KeyMsg{Type: tea.KeyCtrlU}, start: 25, want: 5},
		{name: "page down", key: tea.KeyMsg{Type: tea.KeyPgDown}, start: 7, want: 27},
		{name: "control d", key: tea.KeyMsg{Type: tea.KeyCtrlD}, start: 7, want: 27},
		{name: "top", key: runeKey('g'), start: 7, want: 0},
		{name: "bottom", key: runeKey('G'), start: 7, want: 40},
		{name: "unknown", key: runeKey('x'), start: 7, want: 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &diffView{lines: make([]string, 40), offset: tt.start}
			close, cmd := d.key(tt.key)
			if close != tt.close || cmd != nil || d.offset != tt.want {
				t.Fatalf("key = (close %v, cmd %v, offset %d), want (%v, nil, %d)", close, cmd, d.offset, tt.close, tt.want)
			}
		})
	}

	for _, tt := range []struct {
		name        string
		button      tea.MouseButton
		start, want int
	}{
		{name: "wheel up", button: tea.MouseButtonWheelUp, start: 7, want: 4},
		{name: "wheel down", button: tea.MouseButtonWheelDown, start: 7, want: 10},
		{name: "other", button: tea.MouseButtonLeft, start: 7, want: 7},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := &diffView{offset: tt.start}
			if cmd := d.mouse(tea.MouseMsg{Button: tt.button}); cmd != nil {
				t.Fatalf("mouse command = %v, want nil", cmd)
			}
			if d.offset != tt.want {
				t.Fatalf("mouse offset = %d, want %d", d.offset, tt.want)
			}
		})
	}
}

func runeKey(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}
