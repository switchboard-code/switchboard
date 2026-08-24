package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
	route "github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/session"
)

// drain runs a command chain until it produces a terminal message, feeding
// each pickerMsg's first matching item back through its action, which is how
// a user walks the /models dialogs.
func pick(t *testing.T, cmd tea.Cmd, choose func(pickerMsg) string) tea.Msg {
	t.Helper()
	for i := 0; i < 5; i++ {
		if cmd == nil {
			t.Fatal("the flow ended with no message")
		}
		msg := cmd()
		p, ok := msg.(pickerMsg)
		if !ok {
			return msg
		}
		id := choose(p)
		cmd = p.action(id)
	}
	t.Fatal("the dialog chain never terminated")
	return nil
}

func modelsTestModel(t *testing.T) *tuiModel {
	t.Helper()
	m := testModel(t)
	m.app.config.Path = filepath.Join(t.TempDir(), config.FileName)
	m.app.providers = newProviders("http://127.0.0.1:1", m.app.config)
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	m.app.catalog = cat
	return m
}

func TestModelsBindsANewRungAndSaves(t *testing.T) {
	m := modelsTestModel(t)

	var boundModel string
	msg := pick(t, cmdModels(m, ""), func(p pickerMsg) string {
		switch {
		case strings.HasPrefix(p.title, "bind a model"):
			// Bind the first catalog model; the local server is not running
			// in this test, so every item is a catalog entry.
			boundModel = p.items[0].id
			return boundModel
		case strings.HasPrefix(p.title, "which tier"):
			last := p.items[len(p.items)-1]
			if last.id != "t2" {
				t.Fatalf("the new rung on a one-rung ladder should be t2, got %q", last.id)
			}
			return last.id
		case strings.HasPrefix(p.title, "reasoning effort"):
			return "" // provider default
		default:
			t.Fatalf("unexpected picker %q", p.title)
			return ""
		}
	})

	n, ok := msg.(noticeMsg)
	if !ok || n.level == "error" {
		t.Fatalf("binding did not succeed: %#v", msg)
	}
	if len(m.app.config.Tiers) != 2 {
		t.Fatalf("the ladder has %d rungs, want 2", len(m.app.config.Tiers))
	}

	saved, err := config.LoadFile(m.app.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := saved.Tier("t2"); !ok {
		t.Fatal("the binding was not persisted")
	}
}

func anthropicModelListServer(t *testing.T, models ...string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[`))
		for index, model := range models {
			if index > 0 {
				_, _ = w.Write([]byte(","))
			}
			_, _ = w.Write([]byte(`{"id":"` + model + `"}`))
		}
		_, _ = w.Write([]byte(`]}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestModelsAnthropicSnapshotsKeepExactEvidenceThroughSaveResumeContextAndBudget(t *testing.T) {
	const (
		opusSnapshot    = "claude-opus-5-20260824"
		haikuSnapshot   = "claude-haiku-4-5-20251001"
		unrecordedHaiku = "claude-haiku-4-5-20260824"
		unknownSnapshot = "claude-mystery-5-20260824"
	)
	server := anthropicModelListServer(t, opusSnapshot, haikuSnapshot, unrecordedHaiku, unknownSnapshot)
	t.Setenv("ANTHROPIC_API_KEY", "test-only")

	m := modelsTestModel(t)
	m.app.config.SetProviderBaseURL(config.ProviderSurfaceKey("anthropic", "first-party"), server.URL)
	items, choices := gatherModelChoices(context.Background(), m.app.providers, m.app.catalog, m.app.config)
	if len(items) == 0 {
		t.Fatal("/models returned no choices")
	}

	choiceID := func(model string) string { return "anthropic/" + model + " first-party" }
	opus, ok := choices[choiceID(opusSnapshot)]
	if !ok {
		t.Fatalf("live Opus snapshot was not offered: keys=%v", modelChoiceKeys(choices))
	}
	if opus.catalogMaxOutput != 128_000 || !slicesContain(opus.effortLevels, "xhigh") {
		t.Fatalf("Opus snapshot lost alias evidence: %+v", opus)
	}
	haiku := choices[choiceID(haikuSnapshot)]
	if haiku.catalogMaxOutput != 64_000 || len(haiku.effortLevels) != 0 {
		t.Fatalf("Haiku snapshot inherited the adaptive dialect: %+v", haiku)
	}
	unrecorded := choices[choiceID(unrecordedHaiku)]
	if unrecorded.catalogMaxOutput != 0 || len(unrecorded.effortLevels) != 0 {
		t.Fatalf("unrecorded Haiku snapshot inherited alias evidence: %+v", unrecorded)
	}
	if prompt, ok := chooseTierOrOutputCapCmd(m, unrecorded)().(textPromptMsg); !ok ||
		!strings.Contains(prompt.title, "positive maximum output") {
		t.Fatalf("unrecorded Haiku snapshot opened %#v, want an explicit-cap prompt", prompt)
	}
	onboard := &onboardModel{reg: m.app.providers, cfg: m.app.config, choice: unrecorded}
	if _, ok := onboard.outputCapOrEffort()().(textPromptMsg); !ok {
		t.Fatalf("onboarding admitted an unrecorded Haiku snapshot without an explicit cap")
	}
	unknown := choices[choiceID(unknownSnapshot)]
	if unknown.catalogMaxOutput != 0 || len(unknown.effortLevels) != 0 {
		t.Fatalf("unrecognized live model inherited another model's evidence: %+v", unknown)
	}

	result := pick(t, chooseTierOrOutputCapCmd(m, opus), func(p pickerMsg) string {
		switch {
		case strings.HasPrefix(p.title, "which tier"):
			return p.items[len(p.items)-1].id
		case strings.HasPrefix(p.title, "reasoning effort"):
			return "xhigh"
		default:
			t.Fatalf("unexpected /models dialog %q", p.title)
			return ""
		}
	})
	if notice, ok := result.(noticeMsg); !ok || notice.level == "error" {
		t.Fatalf("snapshot selection failed: %#v", result)
	}

	saved, err := config.LoadFile(m.app.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	tier, ok := saved.Tier("t2")
	if !ok || tier.Target.ModelID != opusSnapshot || tier.Target.Params.Reasoning == nil ||
		!tier.Target.Params.Reasoning.Enabled || tier.Target.Params.Reasoning.Effort != "xhigh" {
		t.Fatalf("saved snapshot binding = %+v", tier)
	}
	resumed, configured, err := tierForSessionState(saved, session.State{RuntimeBinding: session.RuntimeBinding{
		Tier: "t2", Target: tier.Target.ID(), Pinned: true,
	}})
	if err != nil || !configured || resumed.Target.ID() != tier.Target.ID() {
		t.Fatalf("snapshot resume = %+v configured=%v err=%v", resumed, configured, err)
	}

	const window = 1_000_000
	atLimit := candidateForTierContext(tier, 1, m.app.catalog, 10, window-8_192, 0)
	if !atLimit.CatalogKnown || atLimit.Info.ContextWindow != window || atLimit.ReservedOutputTokens != 8_192 || atLimit.CeilingCost <= 0 {
		t.Fatalf("snapshot admission evidence = %+v", atLimit)
	}
	routeInput := route.Input{
		Candidates:   []route.Candidate{atLimit},
		Requirements: route.Requirements{NeedsTools: true, ApprovedProviders: []string{"anthropic"}},
		Pin:          tier.ID,
	}
	if _, err := (route.Heuristic{}).Route(routeInput); err != nil {
		t.Fatalf("snapshot was refused at the exact context boundary: %v", err)
	}
	routeInput.Candidates[0].ContextTokens++
	if _, err := (route.Heuristic{}).Route(routeInput); err == nil {
		t.Fatal("snapshot was admitted one token past its alias-derived context envelope")
	}

	budget := &budgetState{}
	budget.set(atLimit.CeilingCost - 1)
	guard := budgetGate(budget, m.app.catalog,
		func() provider.RouteTarget { return tier.Target },
		func() catalog.Money { return 0 },
		func() string { return "snapshot" })
	if err := guard.before(atLimit.ContextTokens, 1); err == nil || !errors.Is(err, errBudgetUnavailable) {
		t.Fatalf("snapshot budget bound was not enforced: %v", err)
	}
}

func modelChoiceKeys(choices map[string]modelChoice) []string {
	keys := make([]string, 0, len(choices))
	for key := range choices {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func slicesContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestModelsRefusesToRemoveTheActiveRung(t *testing.T) {
	m := modelsTestModel(t)
	msg := pick(t, removeRungCmd(m), func(p pickerMsg) string {
		return m.app.tier.ID
	})
	n, ok := msg.(noticeMsg)
	if !ok || n.level != "error" {
		t.Fatalf("removing the active tier should refuse, got %#v", msg)
	}
	if len(m.app.config.Tiers) != 1 {
		t.Fatal("the active rung was removed anyway")
	}
}

func TestFailedRungRemovalSaveKeepsTheLiveRung(t *testing.T) {
	m := modelsTestModel(t)
	if err := m.app.config.BindTier("t2", "deep", "ollama/larger", "", "high"); err != nil {
		t.Fatal(err)
	}
	m.app.config.Path = t.TempDir() // a directory cannot be replaced by the config file

	msg := pick(t, removeRungCmd(m), func(p pickerMsg) string { return "t2" })
	notice, ok := msg.(noticeMsg)
	if !ok || notice.level != "error" || !strings.Contains(notice.text, "failed to save") {
		t.Fatalf("failed rung removal = %#v, want save error notice", msg)
	}
	if tier, ok := m.app.config.Tier("t2"); !ok || tier.Target.ModelID != "larger" {
		t.Fatalf("failed save removed or changed the live rung: %+v, present=%v", tier, ok)
	}
}

func TestHighestRungSkipsGaps(t *testing.T) {
	cfg := &config.Config{}
	for _, id := range []string{"t1", "t4"} {
		if err := cfg.BindTier(id, "", "ollama/x", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if got := highestRung(cfg); got != 4 {
		t.Fatalf("highestRung = %d, want 4", got)
	}
}
