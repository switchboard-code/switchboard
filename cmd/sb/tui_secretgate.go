package main

// The outbound half of the credential posture. internal/credential keeps
// the keys this program was entrusted with from leaking; this gate catches
// the ones the user is about to leak themselves — a key pasted into a
// prompt, an @mentioned .env, a `!env` transcript riding into the next
// turn. A prompt is about to be written to the session log and sent to a
// provider, and both are places a credential is promised never to appear,
// so the send holds until the user says what happens: redact, send as
// typed, or drop it. Esc means drop — the default direction for a dialog
// about a credential has to be the safe one.

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/credential"
)

// openSecretGate holds an outbound prompt behind the pick. proceed carries
// the send path — a plain turn, a /tN override, a race — so the gate is
// one chokepoint whatever surface the prompt was headed for. The findings
// render as kind and prefix only; a dialog quoting the key would be the
// gate committing the leak it exists to stop.
func (m *tuiModel) openSecretGate(leaks []credential.Leak, prompt string, proceed func(string) tea.Cmd, onDrop ...func() tea.Cmd) tea.Cmd {
	drop := func() tea.Cmd {
		if len(onDrop) > 0 && onDrop[0] != nil {
			return onDrop[0]()
		}
		return nil
	}
	found := make([]string, len(leaks))
	for i, l := range leaks {
		found[i] = l.String()
	}
	m.openDialog(&pickerDialog{
		title:          "the prompt contains " + strings.Join(found, ", "),
		navigationOnly: true,
		items: []pickerItem{
			{id: "redact", label: "redact and send", desc: "each key becomes a placeholder naming what stood there"},
			{id: "send", label: "send as typed", desc: "the key goes to the provider and into the session log"},
			{id: "drop", label: "don't send", desc: "the prompt is dropped; nothing leaves this machine"},
		},
		sel: 2, // a stray Enter drops; arrows are required to authorize egress
		onPick: func(id string) tea.Cmd {
			switch id {
			case "redact":
				return proceed(credential.Redact(prompt, leaks))
			case "send":
				return proceed(prompt)
			}
			m.addNotice("", "not sent; the prompt was dropped before anything left this machine")
			return drop()
		},
		onCancel: drop,
	})
	return nil
}

// openSecretGateForStorage is the gate's durable-copy form: the prompt is
// about to sit at rest in a file and refire later, so "send as typed" is not
// an answer — every other durable artifact this program writes redacts
// unconditionally, and a reminder is that artifact. Redact arms the redacted
// copy; anything else stores nothing.
func (m *tuiModel) openSecretGateForStorage(leaks []credential.Leak, prompt string, proceed func(string) tea.Cmd) tea.Cmd {
	found := make([]string, len(leaks))
	for i, l := range leaks {
		found[i] = l.String()
	}
	m.openDialog(&pickerDialog{
		title:          "the prompt contains " + strings.Join(found, ", ") + ", and a schedule keeps it on disk",
		navigationOnly: true,
		items: []pickerItem{
			{id: "redact", label: "redact and arm", desc: "each key becomes a placeholder naming what stood there"},
			{id: "drop", label: "don't arm", desc: "nothing is scheduled and nothing is stored"},
		},
		sel: 1, // a stray Enter stores nothing
		onPick: func(id string) tea.Cmd {
			if id == "redact" {
				return proceed(credential.Redact(prompt, leaks))
			}
			m.addNotice("", "not scheduled; the prompt was dropped before anything was stored")
			return nil
		},
	})
	return nil
}
