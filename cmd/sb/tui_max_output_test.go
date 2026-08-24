package main

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
)

func customOllamaChoice() modelChoice {
	return modelChoice{
		ref:      "ollama/custom-not-in-catalog",
		provider: "ollama",
		surface:  "local",
		desc:     "pulled locally",
	}
}

func TestUnlistedModelRequiresPositiveOutputCapBeforeTierChoice(t *testing.T) {
	m := modelsTestModel(t)
	choice := customOllamaChoice()
	if got := modelChoiceOutputAllowance(m.app.providers, choice); got != math.MaxInt {
		t.Fatalf("unlisted omitted allowance = %d, want unknown MaxInt", got)
	}

	prompt, ok := chooseTierOrOutputCapCmd(m, choice)().(textPromptMsg)
	if !ok {
		t.Fatalf("unlisted model opened %T, want output-cap prompt", chooseTierOrOutputCapCmd(m, choice)())
	}
	if !strings.Contains(prompt.title, "positive maximum output") || prompt.generation == 0 || prompt.sessionID == "" {
		t.Fatalf("output prompt lost its purpose or async ownership: %+v", prompt)
	}
	ownerGeneration, ownerSession := prompt.generation, prompt.sessionID
	before := len(m.app.config.Tiers)

	for _, bad := range []string{"0", "-1", "not-a-number", strings.Repeat("9", 100)} {
		retry, retryOK := prompt.submit(bad)().(textPromptMsg)
		if !retryOK {
			t.Fatalf("invalid %q produced %T, want the prompt again", bad, prompt.submit(bad)())
		}
		if !strings.Contains(retry.help, "positive") || !strings.Contains(retry.help, "unbounded") {
			t.Fatalf("invalid %q did not explain the correction: %q", bad, retry.help)
		}
		if retry.generation != ownerGeneration || retry.sessionID != ownerSession {
			t.Fatalf("invalid %q changed async ownership from %d/%q to %d/%q",
				bad, ownerGeneration, ownerSession, retry.generation, retry.sessionID)
		}
		if len(m.app.config.Tiers) != before {
			t.Fatalf("invalid %q published a rung", bad)
		}
		prompt = retry
	}

	tierPicker, ok := prompt.submit("4096")().(pickerMsg)
	if !ok || !strings.HasPrefix(tierPicker.title, "which tier") {
		t.Fatalf("positive cap produced %#v, want tier picker", prompt.submit("4096")())
	}
	if len(m.app.config.Tiers) != before {
		t.Fatal("opening the tier picker published the choice early")
	}
	tierID := tierPicker.items[len(tierPicker.items)-1].id
	result := tierPicker.action(tierID)()
	if notice, noticeOK := result.(noticeMsg); !noticeOK || notice.level == "error" {
		t.Fatalf("capped bind failed: %#v", result)
	}

	saved, err := config.LoadFile(m.app.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	tier, ok := saved.Tier(tierID)
	if !ok || tier.Target.Params.MaxOutputTokens != 4096 {
		t.Fatalf("saved tier = %+v, want explicit max_output 4096", tier)
	}
}

func TestOnboardingOutputCapCancellationPublishesNothing(t *testing.T) {
	cfg := &config.Config{Path: filepath.Join(t.TempDir(), config.FileName)}
	m := &onboardModel{
		reg: newProviders("http://127.0.0.1:1", cfg), cfg: cfg, th: darkTheme(),
		step: stepModel, choice: customOllamaChoice(),
	}
	step(t, m, m.outputCapOrEffort()())
	if _, ok := m.dlg.(*textDialog); !ok {
		t.Fatalf("unknown-output onboarding choice opened %T, want cap prompt", m.dlg)
	}
	step(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if !m.cancelled || !m.quitting || len(cfg.Tiers) != 0 {
		t.Fatalf("cap cancellation = cancelled %v quitting %v tiers %d", m.cancelled, m.quitting, len(cfg.Tiers))
	}
	if _, err := os.Stat(cfg.Path); !os.IsNotExist(err) {
		t.Fatalf("cancelled onboarding cap wrote the config: %v", err)
	}
}

func TestOutputCapPromptCancellationPublishesNothing(t *testing.T) {
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyEsc},
		{Type: tea.KeyEnter}, // empty visible entry is cancellation too
	} {
		t.Run(key.String(), func(t *testing.T) {
			m := modelsTestModel(t)
			before := len(m.app.config.Tiers)
			prompt := chooseTierOrOutputCapCmd(m, customOllamaChoice())().(textPromptMsg)
			dialog := newTextDialog(prompt)
			done, cmd := dialog.update(key, m.th)
			if !done || cmd != nil {
				t.Fatalf("cancel = done %v cmd %v, want a quiet close", done, cmd != nil)
			}
			if len(m.app.config.Tiers) != before {
				t.Fatal("cancelled cap prompt changed the ladder")
			}
			if _, err := os.Stat(m.app.config.Path); !os.IsNotExist(err) {
				t.Fatalf("cancelled cap prompt wrote the config: %v", err)
			}
		})
	}
}

func TestCatalogAllowanceDoesNotPersistRedundantTargetCap(t *testing.T) {
	m := modelsTestModel(t)
	choice := modelChoice{
		ref:              "openaicompat/catalogued-model",
		provider:         "openaicompat",
		surface:          "generic",
		catalogMaxOutput: 16_384,
	}
	if got := modelChoiceOutputAllowance(m.app.providers, choice); got != 16_384 {
		t.Fatalf("catalog allowance = %d, want 16384", got)
	}

	tierPicker, ok := chooseTierOrOutputCapCmd(m, choice)().(pickerMsg)
	if !ok {
		t.Fatalf("catalogued model opened %T, want tier picker without cap prompt", chooseTierOrOutputCapCmd(m, choice)())
	}
	tierID := tierPicker.items[len(tierPicker.items)-1].id
	if result := tierPicker.action(tierID)(); result == nil {
		t.Fatal("catalogued bind produced nothing")
	} else if notice, ok := result.(noticeMsg); !ok || notice.level == "error" {
		t.Fatalf("catalogued bind failed: %#v", result)
	}

	saved, err := config.LoadFile(m.app.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	tier, _ := saved.Tier(tierID)
	if tier.Target.Params.MaxOutputTokens != 0 {
		t.Fatalf("catalog evidence became redundant target max_output %d", tier.Target.Params.MaxOutputTokens)
	}
}

func TestCatalogRebindPreservesExistingRungAndFallbackCap(t *testing.T) {
	m := modelsTestModel(t)
	existing, ok := m.app.config.Tier("t1")
	if !ok {
		t.Fatal("fixture has no t1")
	}
	existing.Target.Params.MaxOutputTokens = 2_048
	m.app.config.Tiers[0].Target = existing.Target
	fallback, err := config.ParseTarget("ollama/custom-fallback", "", "")
	if err != nil {
		t.Fatal(err)
	}
	fallback.Params.MaxOutputTokens = existing.Target.Params.MaxOutputTokens
	m.app.config.Tiers[0].Fallbacks = []provider.RouteTarget{fallback}

	choice := modelChoice{
		ref: "openaicompat/catalogued-model", provider: "openaicompat", surface: "generic",
		catalogMaxOutput: 16_384,
	}
	if result := bindCmd(m, choice, "t1", "")(); result == nil {
		t.Fatal("catalog rebind produced nothing")
	} else if notice, ok := result.(noticeMsg); !ok || notice.level == "error" {
		t.Fatalf("catalog rebind failed: %#v", result)
	}
	rebound, _ := m.app.config.Tier("t1")
	if rebound.Target.Params.MaxOutputTokens != existing.Target.Params.MaxOutputTokens ||
		len(rebound.Fallbacks) != 1 || rebound.Fallbacks[0].Params.MaxOutputTokens != existing.Target.Params.MaxOutputTokens {
		t.Fatalf("catalog rebind erased rung policy: before %+v after %+v", existing, rebound)
	}
}

func TestCatalogRebindRepromptsWhenRungCapExceedsVerifiedMaximum(t *testing.T) {
	m := modelsTestModel(t)
	existing, _ := m.app.config.Tier("t1")
	existing.Target.Params.MaxOutputTokens = 20_000
	fallback, err := config.ParseTarget("ollama/custom-fallback", "", "")
	if err != nil {
		t.Fatal(err)
	}
	fallback.Params.MaxOutputTokens = 20_000
	m.app.config.Tiers[0] = existing
	m.app.config.Tiers[0].Fallbacks = []provider.RouteTarget{fallback}
	choice := modelChoice{
		ref: "openaicompat/catalogued-model", provider: "openaicompat", surface: "generic",
		catalogMaxOutput: 16_384,
	}

	prompt, ok := bindCmd(m, choice, "t1", "")().(textPromptMsg)
	if !ok {
		t.Fatalf("conflicting catalog rebind produced %T, want corrective cap prompt", bindCmd(m, choice, "t1", "")())
	}
	for _, want := range []string{"t1", "20000", "verified maximum", "16384", "no greater"} {
		if !strings.Contains(prompt.help, want) {
			t.Fatalf("corrective prompt omitted %q: %q", want, prompt.help)
		}
	}
	if prompt.initial != "" {
		t.Fatalf("corrective prompt prefilled %q; a delayed Enter could accept it", prompt.initial)
	}
	if done, cmd := newTextDialog(prompt).update(tea.KeyMsg{Type: tea.KeyEnter}, darkTheme()); !done || cmd != nil {
		t.Fatalf("stray Enter on corrective prompt advanced it: done=%v cmd=%v", done, cmd != nil)
	}
	unchanged, _ := m.app.config.Tier("t1")
	if unchanged.Target.ID() != existing.Target.ID() || unchanged.Fallbacks[0].Params.MaxOutputTokens != 20_000 {
		t.Fatalf("opening corrective prompt mutated the rung: %+v", unchanged)
	}

	retry, ok := prompt.submit("20000")().(textPromptMsg)
	if !ok || !strings.Contains(retry.help, "verified maximum") {
		t.Fatalf("oversized replacement produced %#v, want corrective prompt again", prompt.submit("20000")())
	}
	result := retry.submit("8192")()
	if notice, ok := result.(noticeMsg); !ok || notice.level == "error" {
		t.Fatalf("valid replacement cap failed: %#v", result)
	}
	rebound, _ := m.app.config.Tier("t1")
	if rebound.Target.Params.MaxOutputTokens != 8_192 || rebound.Fallbacks[0].Params.MaxOutputTokens != 8_192 {
		t.Fatalf("corrected rung cap did not reach primary and fallback: %+v", rebound)
	}
}

func TestLiveLocalModelKeepsExactCatalogAllowance(t *testing.T) {
	const modelID = "catalogued-local-model"
	cat := catalogWithLocalModels(t, localModelSpec{
		name: modelID, contextWindow: 32_768, priceMaxInput: 32_768,
	})
	var captured capturedRequest
	server := customOllamaServer(t, modelID, &captured)
	m := modelsTestModel(t)
	m.app.catalog = cat
	m.app.providers = newProviders(server.URL, m.app.config)

	_, choices := gatherModelChoices(context.Background(), m.app.providers, cat, m.app.config)
	choice, ok := choices["ollama/"+modelID+" local"]
	if !ok {
		t.Fatal("live local model was not offered")
	}
	if choice.catalogMaxOutput != 100 {
		t.Fatalf("live/catalog duplicate lost max output evidence: got %d want 100", choice.catalogMaxOutput)
	}
	if _, ok := chooseTierOrOutputCapCmd(m, choice)().(pickerMsg); !ok {
		t.Fatalf("catalogued live model opened %T, want tier picker without a redundant cap prompt",
			chooseTierOrOutputCapCmd(m, choice)())
	}

	var browsed modelChoice
	picker, ok := browseSurfaceCmdWithCatalog(m.app.providers, cat, m.app.config, modelChoice{
		provider: "ollama", surface: "local", browse: true,
	}, func(choice modelChoice) tea.Cmd {
		browsed = choice
		return noticeCmd("", "selected")
	})().(pickerMsg)
	if !ok {
		t.Fatal("local surface did not open its model picker")
	}
	if result := picker.action(modelID); result == nil {
		t.Fatal("browsed model selection produced no continuation")
	}
	if browsed.catalogMaxOutput != 100 {
		t.Fatalf("surface browse lost exact model catalog evidence: got %d want 100", browsed.catalogMaxOutput)
	}
}

func TestOutputCapPromptFitsNarrowShortTerminal(t *testing.T) {
	for _, height := range []int{6, 10} {
		t.Run(strconv.Itoa(height), func(t *testing.T) {
			m := modelsTestModel(t)
			m.Update(tea.WindowSizeMsg{Width: 20, Height: height})
			prompt := chooseTierOrOutputCapCmd(m, customOllamaChoice())().(textPromptMsg)
			prompt = prompt.submit("0")().(textPromptMsg)
			m.dlg = newTextDialog(prompt)

			view := m.View()
			assertTUIViewBounds(t, view, 20, height)
			plain := ansi.Strip(view)
			for _, want := range []string{"positive", "unbounded", "▌", "enter"} {
				if !strings.Contains(plain, want) {
					t.Fatalf("%dx%d cap prompt hid %q:\n%s", 20, height, want, plain)
				}
			}
		})
	}
}

func TestOutputCapBindSaveFailureLeavesNoGhostRung(t *testing.T) {
	m := modelsTestModel(t)
	existing, ok := m.app.config.Tier("t1")
	if !ok {
		t.Fatal("fixture has no t1")
	}
	existing.Target.Params.MaxOutputTokens = 2048
	fallback, err := config.ParseTarget("ollama/original-fallback", "", "")
	if err != nil {
		t.Fatal(err)
	}
	fallback.Params.MaxOutputTokens = 2048
	m.app.config.Tiers[0] = existing
	m.app.config.Tiers[0].Fallbacks = []provider.RouteTarget{fallback}
	before := append([]config.Tier(nil), m.app.config.Tiers...)
	before[0].Fallbacks = append([]provider.RouteTarget(nil), before[0].Fallbacks...)

	// Save cannot rename a file over this directory. The staged binding must
	// not become live merely because the durable publication was refused.
	m.app.config.Path = t.TempDir()
	result := bindCmd(m, modelChoice{
		ref: "ollama/replacement", provider: "ollama", surface: "local", maxOutput: 4096,
	}, "t1", "")()
	notice, ok := result.(noticeMsg)
	if !ok || notice.level != "error" || !strings.Contains(notice.text, "saving configuration") {
		t.Fatalf("failed bind produced %#v, want actionable error notice", result)
	}
	if !reflect.DeepEqual(m.app.config.Tiers, before) {
		t.Fatalf("failed save left a ghost binding:\nbefore: %+v\nafter:  %+v", before, m.app.config.Tiers)
	}
}

func TestModelBindingUIShowsSurfaceAndRungCap(t *testing.T) {
	m := modelsTestModel(t)
	api := modelChoice{
		ref: "openai/shared-model", provider: "openai", surface: "api",
		effortLevels: []string{"high"}, catalogMaxOutput: 16_384,
	}
	subscription := api
	subscription.surface = "subscription"

	apiPicker := chooseTierCmd(m, api)().(pickerMsg)
	subscriptionPicker := chooseTierCmd(m, subscription)().(pickerMsg)
	if !strings.Contains(apiPicker.title, "openai/api/shared-model") ||
		!strings.Contains(subscriptionPicker.title, "openai/subscription/shared-model") ||
		apiPicker.title == subscriptionPicker.title {
		t.Fatalf("same model on distinct surfaces is ambiguous: %q / %q", apiPicker.title, subscriptionPicker.title)
	}
	effort := chooseEffortCmd(m, subscription, "t1")().(pickerMsg)
	if !strings.Contains(effort.title, "openai/subscription/shared-model") {
		t.Fatalf("effort picker hid serving surface: %q", effort.title)
	}

	result := bindCmd(m, modelChoice{
		ref: "ollama/custom-visible", provider: "ollama", surface: "local", maxOutput: 4096,
	}, "t1", "")()
	notice, ok := result.(noticeMsg)
	if !ok || notice.level == "error" {
		t.Fatalf("visible capped bind failed: %#v", result)
	}
	for _, want := range []string{"ollama/local/custom-visible", "max:4096", "primary and fallbacks"} {
		if !strings.Contains(notice.text, want) {
			t.Fatalf("binding notice omitted %q: %q", want, notice.text)
		}
	}
}
