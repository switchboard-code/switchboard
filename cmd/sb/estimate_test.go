package main

import (
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
)

// The whole point is pricing before sending, in each rung's own metering:
// a local rung never renders dollars, a priced rung renders a range with
// its cache assumption stated, and nothing collapses into $0.00.
func TestEstimatePricesEveryRungInItsOwnMetering(t *testing.T) {
	cat, priced := pricedTarget(t)
	m := testModel(t)
	local := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "qwen3:4b"}
	m.app.catalog = cat
	m.app.config.Tiers = []config.Tier{
		{ID: "t1", Label: "light", Target: local},
		{ID: "t2", Label: "deep", Target: priced},
	}
	m.app.tier = m.app.config.Tiers[0]

	cmdEstimate(m, "refactor the parser to stream")
	out := strings.Join(m.tr.flat, "\n")
	semantic := strings.Join(strings.Fields(stripANSI(out)), " ")

	if !strings.Contains(semantic, "priced on every rung before it is sent") {
		t.Fatalf("the header is missing:\n%s", out)
	}
	if !strings.Contains(semantic, "your prompt") {
		t.Errorf("the prompt's own tokens are not named:\n%s", out)
	}
	if !strings.Contains(semantic, "runs locally — nothing to bill") {
		t.Errorf("the local rung lost its metering word:\n%s", out)
	}
	if !strings.Contains(semantic, "between $") || !strings.Contains(semantic, "expected") || strings.Count(semantic, "$") < 3 {
		t.Errorf("the priced rung has no range:\n%s", out)
	}
	if !strings.Contains(semantic, "priced cold") {
		t.Errorf("an unobserved rung must state the cold assumption:\n%s", out)
	}
	if strings.Contains(out, "$0.00") {
		t.Errorf("something rendered as free money:\n%s", out)
	}
	if !strings.Contains(semantic, "not a quote") {
		t.Errorf("the estimate does not bound its own claim:\n%s", out)
	}
}

// Bare /estimate prices the context alone: asking what the standing
// conversation costs to carry is a question with no prompt attached.
func TestEstimateWithoutAPromptPricesTheContext(t *testing.T) {
	cat, priced := pricedTarget(t)
	m := testModel(t)
	m.app.catalog = cat
	m.app.config.Tiers = []config.Tier{{ID: "t2", Label: "deep", Target: priced}}
	m.app.tier = m.app.config.Tiers[0]

	cmdEstimate(m, "")
	out := strings.Join(m.tr.flat, "\n")
	if strings.Contains(out, "your prompt") {
		t.Errorf("no prompt was given, yet one is priced:\n%s", out)
	}
	if !strings.Contains(out, "tokens would go up") {
		t.Errorf("the upload estimate is missing:\n%s", out)
	}
}
