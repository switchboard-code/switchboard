package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

func TestReplayHistoryDistinguishesInjectedInterruptedAndToolOutcomes(t *testing.T) {
	m := testModel(t)
	m.tr.reset()
	m.replayHistory(session.State{Messages: []provider.Message{
		provider.UserText("fix the parser"),
		{Role: provider.RoleUser, Injected: true, Content: []provider.Block{provider.Text{Text: "[watch] tests are red"}}},
		{Role: provider.RoleAssistant, Incomplete: true, Content: []provider.Block{provider.Text{Text: "I may have changed"}}},
		{Role: provider.RoleAssistant, Content: []provider.Block{
			provider.ToolUse{ID: "call-ok", Name: "read", Input: json.RawMessage(`{"path":"main.go"}`)},
			provider.ToolUse{ID: "call-lost", Name: "exec", Input: json.RawMessage(`{"command":["go","test","./..."]}`)},
		}},
		{Role: provider.RoleTool, Content: []provider.Block{
			provider.ToolResult{ToolUseID: "call-ok", Name: "read", Content: "package main"},
		}},
	}})

	flat := stripANSI(strings.Join(m.tr.flat, "\n"))
	for _, want := range []string{
		"fix the parser",
		"injected round-boundary context",
		"interrupted model output follows",
		"I may have changed",
		"package main",
		"outcome unknown, inspect before retrying",
	} {
		if !strings.Contains(flat, want) {
			t.Fatalf("resumed transcript omitted %q:\n%s", want, flat)
		}
	}
	userCards := 0
	for _, entry := range m.tr.entries {
		if entry.kind == kindUser {
			userCards++
		}
	}
	if userCards != 1 {
		t.Fatalf("replay rendered %d user cards, want only the authored opening", userCards)
	}
}

func TestReplayHistoryUsesOnlyDurableAuthoredProjection(t *testing.T) {
	m := testModel(t)
	m.tr.reset()
	expanded := provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Block{provider.Text{Text: "inspect @notes.txt\n\nContents of notes.txt:\nMACHINE-EXPANDED-CONTENT"}},
	}.WithAuthoredText("inspect @notes.txt")
	m.replayHistory(session.State{Messages: []provider.Message{expanded}})

	flat := stripANSI(strings.Join(m.tr.flat, "\n"))
	if !strings.Contains(flat, "inspect @notes.txt") {
		t.Fatalf("authored projection was not rendered:\n%s", flat)
	}
	if strings.Contains(flat, "MACHINE-EXPANDED-CONTENT") {
		t.Fatalf("provider expansion was rendered as user-authored:\n%s", flat)
	}
}

func TestReplayHistoryVisiblyWithholdsAmbiguousLegacyOpening(t *testing.T) {
	m := testModel(t)
	m.tr.reset()
	legacy := provider.Message{Role: provider.RoleUser, Content: []provider.Block{
		provider.Text{Text: "possibly typed\n\nLEGACY-PROVIDER-EXPANSION"},
	}}
	m.replayHistory(session.State{Messages: []provider.Message{legacy}})

	flat := stripANSI(strings.Join(m.tr.flat, "\n"))
	if strings.Contains(flat, "LEGACY-PROVIDER-EXPANSION") || strings.Contains(flat, "possibly typed") {
		t.Fatalf("ambiguous legacy wire text was attributed to the user:\n%s", flat)
	}
	if !strings.Contains(flat, "authored wording is unavailable") || !strings.Contains(flat, "provider-expanded opening is hidden") {
		t.Fatalf("legacy omission was not explained:\n%s", flat)
	}
	for _, entry := range m.tr.entries {
		if entry.kind == kindUser {
			t.Fatal("ambiguous legacy opening rendered as a user card")
		}
	}
}

func TestReplayHistoryLabelsSyntheticCompactionMessagesAsHarnessContext(t *testing.T) {
	m := testModel(t)
	m.tr.reset()
	seed := provider.Message{Role: provider.RoleUser, Synthetic: true, Content: []provider.Block{
		provider.Text{Text: compactSeed("parent", testCompactHandoff("Fix the parser."))},
	}}
	continuation := syntheticTurnOpening(compactContinuePrompt, nil)
	m.replayHistory(session.State{Messages: []provider.Message{seed, continuation}})

	flat := stripANSI(strings.Join(m.tr.flat, "\n"))
	for _, want := range []string{"Switchboard compaction handoff", "Fix the parser", "Switchboard automatic continuation"} {
		if !strings.Contains(flat, want) {
			t.Fatalf("synthetic replay omitted %q:\n%s", want, flat)
		}
	}
	if strings.Contains(flat, "authored wording is unavailable") {
		t.Fatalf("synthetic history was presented as a legacy user turn:\n%s", flat)
	}
	for _, entry := range m.tr.entries {
		if entry.kind == kindUser {
			t.Fatal("synthetic history rendered as a user card")
		}
	}
}
