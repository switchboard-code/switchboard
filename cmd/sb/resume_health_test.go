package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

func TestResumeHealthChipsNameRecoveryState(t *testing.T) {
	target := provider.RouteTarget{Provider: "anthropic", Surface: "messages", ModelID: "claude-test"}.ID()
	health := session.ResumeHealth{
		Messages:             7,
		Turns:                3,
		EffectiveTarget:      target,
		IncompleteAssistants: 1,
		PendingToolRepairs:   2,
		Continuity:           session.ContinuityPending,
		RetryIntent:          session.RetryIntentStarted,
		RecoveredCorruptTail: true,
		CorruptRecord:        true,
		ReplayLimit:          true,
	}
	got := resumeHealthChips(health, true)
	for _, want := range []string{
		"3 turns", "7 msgs", "interrupted ×1", "repair pending ×2",
		"continuity pending", "retry started", "recovery decision required", "tail recovery", "corrupt", "replay limit", "resume blocked", "anthropic/messages/claude-test",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("health chips %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "ready") || strings.Contains(got, "rt2:") {
		t.Fatalf("recovery health was misleading or opaque: %q", got)
	}
}

func TestResumePickerShowsReadOnlyCandidateHealth(t *testing.T) {
	m := testModel(t)
	target := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "resume-test"}
	sess, err := m.app.store.Create(m.app.workspace, target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("resume health picker with a deliberately long opening label")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
		provider.Text{Text: "ready"},
	}}); err != nil {
		t.Fatal(err)
	}
	id := sess.ID()
	path := sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, time.August, 24, 2, 17, 0, 0, time.Local)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	if cmd := cmdResume(m, ""); cmd != nil {
		t.Fatal("opening the resume picker unexpectedly returned an async command")
	}
	picker, ok := m.dlg.(*pickerDialog)
	if !ok || len(picker.items) != 1 {
		t.Fatalf("resume picker = %#v", m.dlg)
	}
	if picker.sel != -1 || !picker.requireSelection {
		t.Fatalf("resume picker defaults to row %d; a session swap must require navigation", picker.sel)
	}
	desc := picker.items[0].desc
	if picker.items[0].id != id {
		t.Fatalf("resume picker key = %q, want %q", picker.items[0].id, id)
	}
	if want := stamp.Format("2006-01-02 15:04"); !strings.HasPrefix(picker.items[0].label, want) {
		t.Fatalf("resume picker label %q does not expose timestamp %q", picker.items[0].label, want)
	}
	for _, want := range []string{"1t", "2m", "ready", target.Display()} {
		if !strings.Contains(desc, want) {
			t.Errorf("resume metadata %q missing %q", desc, want)
		}
	}
	picker.setQuery(id)
	if matches := picker.matches(); len(matches) != 1 || matches[0].item.id != id {
		t.Fatalf("hidden session id was not searchable: %+v", matches)
	}
	if view := stripANSI(picker.view(80, m.th)); !strings.Contains(view, target.Display()) {
		t.Fatalf("80-column resume picker hid the effective target:\n%s", view)
	}
}

func TestResumePickerKeepsPreservedCorruptSessionDiscoverable(t *testing.T) {
	m := testModel(t)
	sess, err := m.app.store.Create(m.app.workspace, m.app.tier.Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"recover this work", "damage middle", "valid work after damage"} {
		if err := sess.AppendMessage(provider.UserText(text)); err != nil {
			t.Fatal(err)
		}
	}
	id, path := sess.ID(), sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), "damage middle", "DAMAGE MIDDLE", 1)
	if tampered == string(raw) {
		t.Fatal("corrupt-session fixture did not contain its middle payload")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}

	if cmd := cmdResume(m, ""); cmd != nil {
		t.Fatal("opening the resume picker unexpectedly returned an async command")
	}
	picker, ok := m.dlg.(*pickerDialog)
	if !ok || len(picker.items) != 1 || picker.items[0].id != id {
		t.Fatalf("preserved corrupt session is not pickable: %#v", m.dlg)
	}
	desc := picker.items[0].desc
	if !strings.Contains(desc, "corrupt") || !strings.Contains(desc, "resume blocked") || strings.Contains(desc, "ready") {
		t.Fatalf("corrupt resume metadata is misleading: %q", desc)
	}
	picker.setQuery("corrupt")
	if matches := picker.matches(); len(matches) != 1 || matches[0].item.id != id {
		t.Fatalf("corrupt health was not searchable: %+v", matches)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != tampered {
		t.Fatal("opening the resume picker modified the preserved corrupt log")
	}
}

func TestSessionCommandShowsResumeHealth(t *testing.T) {
	m := testModel(t)
	if err := m.app.loop.Session.AppendMessage(provider.UserText("one turn")); err != nil {
		t.Fatal(err)
	}
	if err := m.app.loop.Session.AppendMessage(provider.Message{
		Role: provider.RoleAssistant, Incomplete: true,
		Content: []provider.Block{provider.Text{Text: "cut off"}},
	}); err != nil {
		t.Fatal(err)
	}
	cmdSession(m, "")
	output := strings.Join(m.tr.flat, "\n")
	for _, want := range []string{"health", "1 turn", "2 msgs", "interrupted ×1"} {
		if !strings.Contains(output, want) {
			t.Errorf("/session output missing %q:\n%s", want, output)
		}
	}
}

func TestREPLSessionCommandShowsTheSameResumeHealth(t *testing.T) {
	r, _, readOutput := newOverrideREPL(t, "small")
	if err := r.loop.Session.AppendMessage(provider.UserText("one turn")); err != nil {
		t.Fatal(err)
	}
	if err := r.loop.Session.AppendMessage(provider.Message{
		Role: provider.RoleAssistant, Incomplete: true,
		Content: []provider.Block{provider.Text{Text: "cut off"}},
	}); err != nil {
		t.Fatal(err)
	}
	health := session.ResumeHealthForState(r.loop.Session.State(), false)
	want := "health   " + resumeHealthChips(health, false)
	if done := r.command(context.Background(), "/session"); done {
		t.Fatal("/session exited the REPL")
	}
	if output := readOutput(); !strings.Contains(output, want) {
		t.Fatalf("REPL /session missing shared health %q:\n%s", want, output)
	}
}
