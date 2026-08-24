package main

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/permission"
)

// Every dialog returned before the ctrl+c case, so a modal swallowed the
// interrupt. A subagent's approval arrives unbidden and mid-errand, and a user
// who does not want to grant it had no way out of the turn that raised it.
func TestCtrlCEscapesAPermissionDialogAndAnswersNo(t *testing.T) {
	m := testModel(t)
	respond := make(chan permission.Response, 1)
	m.openDialog(newPermissionDialog(
		permission.Request{Tool: "exec", Argv: []string{"rm", "-rf", "/"}},
		permission.Outcome{Decision: permission.Ask}, respond))

	m.key(tea.KeyMsg{Type: tea.KeyCtrlC})

	if m.dlg != nil {
		t.Fatal("ctrl+c left the modal up")
	}
	// The loop is blocked on this channel; leaving without answering hangs it.
	select {
	case got := <-respond:
		if got.Approved {
			t.Fatal("escaping a prompt approved the command")
		}
	default:
		t.Fatal("ctrl+c left the waiting loop with no answer")
	}
	if m.dialogToken != nil || len(m.dialogQueue) != 0 {
		t.Error("the cancelled dialog was kept by the broker")
	}
}

// An approval that arrives while the user is typing must not turn an ordinary
// composer letter into authority. The dialog starts on no and only arrow-key
// navigation followed by Enter can approve.
func TestPermissionDialogDoesNotTreatComposerLettersAsApproval(t *testing.T) {
	m := testModel(t)
	m.ta.SetValue("deploy ")
	respond := make(chan permission.Response, 1)
	m.Update(askMsg{
		req: permission.Request{Tool: "exec", Argv: []string{"ls"}},
		out: permission.Outcome{Decision: permission.Ask}, respond: respond,
	})

	for _, key := range []rune{'y', 'a', 'j', 'k'} {
		m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{key}})
		select {
		case got := <-respond:
			t.Fatalf("bare composer letter %q resolved the dialog: %+v", key, got)
		default:
		}
	}
	if m.dlg == nil {
		t.Fatal("a bare composer letter closed the dialog")
	}
	// Enter is another ordinary composer key. Because the dialog arrived
	// asynchronously before that key was painted, its safe default must deny.
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	if got := <-respond; got.Approved {
		t.Fatalf("the safe default approved after a delayed arrival: %+v", got)
	}

	respond = make(chan permission.Response, 1)
	m.openDialog(newPermissionDialog(
		permission.Request{Tool: "exec", Argv: []string{"ls"}},
		permission.Outcome{Decision: permission.Ask}, respond))
	m.key(tea.KeyMsg{Type: tea.KeyDown})
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	select {
	case got := <-respond:
		if !got.Approved || got.Remember {
			t.Fatalf("deliberate approval = %+v", got)
		}
	default:
		t.Fatal("arrow then enter did not approve")
	}
}
