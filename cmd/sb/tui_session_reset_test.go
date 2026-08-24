package main

import (
	"path/filepath"
	"testing"
)

// A compaction carries its watch fold because it continues one conversation,
// but every swap adopts a distinct session log and model request history. The
// per-session injection and presentation ledgers therefore reset in both
// cases; application-wide configuration is deliberately left alone.
func TestCommittedSessionSwapResetsLogScopedState(t *testing.T) {
	for _, tc := range []struct {
		name     string
		keepFold bool
	}{
		{name: "ordinary new log"},
		{name: "compacted new log", keepFold: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testModel(t)
			m.history = []string{"old prompt"}
			m.histIdx = 0
			m.histDraft = "draft from before traversal"
			m.histDraftSet = true
			m.ta.SetValue("old prompt")
			m.app.config.CompactAuto = true
			m.routeLog = []string{"route evidence from the prior log"}
			m.raceLog = []string{"race evidence from the prior log"}
			m.bisectHinted = true
			m.app.publishOccupancy(7_500, 10_000)
			m.app.pressureMu.Lock()
			m.app.pressureWarned = true
			m.app.pressureMu.Unlock()

			m.app.rules = &ruleSet{
				workspace: m.app.workspace,
				fired:     map[string]bool{},
				rules: []pathRule{{
					label: "source.md", globs: []string{"src/*"}, body: "use the source rule",
				}},
			}
			touched := []string{filepath.Join(m.app.workspace, "src", "main.go")}
			if got := m.app.rules.matched(touched); len(got) != 1 {
				t.Fatalf("prior log did not fire the test rule: %d", len(got))
			}

			fresh, err := m.app.store.Create(m.app.workspace, m.app.tier.Target.ID(), "test")
			if err != nil {
				t.Fatal(err)
			}
			binding := m.app.loop.Binding()
			if cmd := m.onSessionSwap(sessionSwapMsg{
				sess: fresh, tier: m.app.tier, client: binding.Provider,
				fresh: true, keepFold: tc.keepFold,
			}); cmd != nil {
				t.Fatal("ordinary committed swap returned a continuation")
			}
			t.Cleanup(func() { _ = m.app.loop.Session.Close() })

			if len(m.routeLog) != 0 || len(m.raceLog) != 0 || m.bisectHinted {
				t.Fatalf("presentation state crossed the swap: routes=%v races=%v bisectHinted=%v",
					m.routeLog, m.raceLog, m.bisectHinted)
			}
			m.app.pressureMu.Lock()
			tokens, window, warned := m.app.pressureTokens, m.app.pressureWindow, m.app.pressureWarned
			m.app.pressureMu.Unlock()
			if tokens != 0 || window != 0 || warned {
				t.Fatalf("pressure state crossed the swap: tokens=%d window=%d warned=%v", tokens, window, warned)
			}
			m.app.publishOccupancy(7_500, 10_000)
			if got := m.app.pressureRound(); len(got) != 1 {
				t.Fatalf("context warning was not re-armed for the adopted log: %d messages", len(got))
			}
			if got := m.app.rules.matched(touched); len(got) != 1 {
				t.Fatalf("path rule was not re-armed for the adopted log: %d", len(got))
			}
			if !m.app.config.CompactAuto {
				t.Fatal("session reset changed an application-wide compaction setting")
			}
			if m.histIdx != len(m.history) || m.histDraftSet || m.histDraft != "" {
				t.Fatalf("session reset retained history traversal: index=%d draft=%q saved=%v",
					m.histIdx, m.histDraft, m.histDraftSet)
			}
			if got := m.ta.Value(); got != "old prompt" {
				t.Fatalf("session reset discarded the visible unsent composer draft: %q", got)
			}
		})
	}
}
