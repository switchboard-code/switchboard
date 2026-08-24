package main

// /steer and ctrl+s: the user's own words into a turn already in flight.
//
// The queue answers "run this after"; steering answers "read this now". The
// loop's round boundary is the only seam where a user message is legal in
// every wire format, so a steer waits there at most — it never rewrites what
// was sent, and it never lands under an in-flight round. What misses the turn
// entirely is not dropped: at turn end it leads the prompt queue, because it
// was typed before anything queued behind it.

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/provider"
)

// pendingSteer keeps the exact user-visible correction beside the expanded
// provider text. Compaction needs that durable authored projection to grant
// scope-changing authority to the user's words without granting it to an
// @mentioned file that rode in with them.
type pendingSteer struct {
	prompt   string
	authored string
}

func (a *tuiApp) queueSteer(text string) {
	a.queueSteerAuthored(text, text)
}

func (a *tuiApp) queueSteerAuthored(prompt, authored string) {
	a.steerMu.Lock()
	defer a.steerMu.Unlock()
	a.steers = append(a.steers, pendingSteer{prompt: prompt, authored: authored})
}

func (a *tuiApp) takeSteers() []string {
	pending := a.takePendingSteers()
	out := make([]string, len(pending))
	for i := range pending {
		out[i] = pending[i].prompt
	}
	return out
}

func (a *tuiApp) takeSteerAuthored() []string {
	pending := a.takePendingSteers()
	out := make([]string, len(pending))
	for i := range pending {
		out[i] = pending[i].authored
	}
	return out
}

func (a *tuiApp) takePendingSteers() []pendingSteer {
	a.steerMu.Lock()
	defer a.steerMu.Unlock()
	out := a.steers
	a.steers = nil
	return out
}

// steerRound is the user's slice of the injection seam, first in the
// composition: between two rounds the user's words outrank every machine
// note. The [steer] lead is the marking injected text already carries, so
// /retry's opening detection reads one as what it is — something that rode in
// mid-turn, never a turn's opening.
func (a *tuiApp) steerRound() []provider.Message {
	steers := a.takePendingSteers()
	out := make([]provider.Message, 0, len(steers))
	for _, steer := range steers {
		message := provider.UserText("[steer] " + steer.prompt).
			WithAuthoredText("[steer] " + steer.authored)
		message.UserSteer = true
		out = append(out, message)
	}
	return out
}

// steerKey is ctrl+s. With a turn running, the composed text steers it; at a
// quiet prompt the key is an ordinary send, because steering nothing is just
// typing.
func (m *tuiModel) steerKey() tea.Cmd {
	if !m.busy && !m.turnPlanning && !m.operationActive {
		return m.submit()
	}
	text := strings.TrimSpace(m.ta.Value())
	if text == "" {
		return nil
	}
	if m.race != nil {
		// Race arms run their own loops with no injection seam by design;
		// accepting the text would promise a delivery that does not exist.
		return noticeCmd("warn", "a race's arms cannot hear you; the session that continues can be steered after the verdict")
	}
	m.ta.Reset()
	m.growInput()
	m.sugClosed = false
	m.sugSel = 0
	// Same history as a send: a correction recalled with up-arrow is a
	// correction you can steer again without retyping. The history seam
	// redacts recognized credentials before either copy is written.
	m.rememberPrompt(text)
	if m.operationActive {
		return m.queuePromptAfterOperation(text)
	}
	return m.steer(text)
}

// queuePromptAfterOperation keeps both steer entry points honest while a
// session operation owns the loop. Compacting, resuming, learning, and the
// other exclusive operations have no model-round boundary to receive a
// steer, so the user's words become the next ordinary prompt instead.
func (m *tuiModel) queuePromptAfterOperation(text string) tea.Cmd {
	m.queue = append(m.queue, text)
	m.addNotice("", "queued; it runs when the current operation finishes")
	return nil
}

// steer hands text to the running turn. Mentions expand the way they would
// for a send, and the secret gate is the same one a plain turn passes,
// because the destination is the same provider. A picture cannot ride a
// round boundary — the injection seam carries text — so one is named rather
// than silently dropped.
func (m *tuiModel) steer(text string) tea.Cmd {
	expanded, images := m.expandMentions(text)
	leaks := credential.ScanPrompt(expanded)
	// A secret decision is asynchronous user input. Bind it to the exact turn
	// and session whose next round could receive the steer; if that boundary
	// disappears while the dialog is open, the correction must not surface in
	// a later, unrelated turn. Direct unit callers with no active turn retain
	// the small unbound seam used to exercise redaction itself.
	boundToTurn := !m.operationActive && (m.busy || m.turnPlanning)
	turnGeneration := m.turnGeneration
	sessionID := currentSessionID(m)
	send := func(p string) tea.Cmd {
		if boundToTurn && (m.operationActive || (!m.busy && !m.turnPlanning) ||
			m.turnGeneration != turnGeneration || currentSessionID(m) != sessionID) {
			m.addNotice("warn", "not steered: that turn ended while the credential decision was open; submit the correction as a new prompt if it is still needed")
			return nil
		}
		display := text
		if len(leaks) > 0 && p != expanded {
			display = credential.Redact(display, leaks)
		}
		m.app.queueSteerAuthored(p, display)
		m.addUser(display)
		note := "steers the running turn; it lands at the next round boundary"
		if len(images) > 0 {
			note = "steers the running turn as text at the next round boundary; the attached image(s) cannot ride it"
		}
		m.addNotice("", note)
		return nil
	}
	if len(leaks) > 0 {
		return m.openSecretGate(leaks, expanded, send)
	}
	return send(expanded)
}

func cmdSteer(m *tuiModel, args string) tea.Cmd {
	text := strings.TrimSpace(args)
	if text == "" {
		return noticeCmd("", "/steer <text>, or type and press ctrl+s, sends your words into the running turn at its next round boundary; with nothing running they start a turn; /tasks steer <id> <text> steers a delegate")
	}
	if m.race != nil {
		return noticeCmd("warn", "a race's arms cannot hear you; the session that continues can be steered after the verdict")
	}
	if m.operationActive {
		return m.queuePromptAfterOperation(text)
	}
	if m.busy || m.turnPlanning {
		return m.steer(text)
	}
	return m.startTurn(text, "")
}
