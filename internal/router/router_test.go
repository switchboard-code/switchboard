package router

import (
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/costmodel"
	"github.com/switchboard-code/switchboard/internal/provider"
)

func candidate(tier string, rank int, opts ...func(*Candidate)) Candidate {
	c := Candidate{
		Tier:   tier,
		Target: provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "m" + tier},
		Info: catalog.ModelInfo{
			ContextWindow: 200_000,
			Tools:         catalog.ToolsParallel,
			Vision:        true,
		},
		Rank:         rank,
		PromptTokens: 1_000,
	}
	for _, o := range opts {
		o(&c)
	}
	return c
}

func ladder() []Candidate {
	return []Candidate{candidate("t1", 0), candidate("t2", 1), candidate("t3", 2)}
}

func TestAShortRequestStaysLow(t *testing.T) {
	got, err := Heuristic{}.Route(Input{
		Prompt:     "what does main.go print?",
		Candidates: ladder(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Tier != "t1" {
		t.Errorf("tier = %q, want the bottom of the ladder", got.Tier)
	}
	if got.Source != SourceHeuristic {
		t.Errorf("source = %q", got.Source)
	}
	if got.Rationale == "" {
		t.Error("no rationale; §8.1 renders this to the user rather than logging it")
	}
}

// §8.2's own examples: breadth and a failure signature both argue upward.
func TestBreadthAndFailuresArgueUpward(t *testing.T) {
	wide, err := Heuristic{}.Route(Input{
		Prompt:     "refactor the storage layer",
		Session:    SessionFeatures{FilesInContext: 9, DiffSizeSoFar: 400},
		Candidates: ladder(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if wide.Tier == "t1" {
		t.Error("a wide refactor stayed at the bottom of the ladder")
	}
	if !strings.Contains(wide.Rationale, "files in play") {
		t.Errorf("rationale = %q; it has to name what moved the decision", wide.Rationale)
	}

	afterFailure, err := Heuristic{}.Route(Input{
		Prompt:     "fix it",
		Session:    SessionFeatures{PriorFailures: 1, TestFailures: 1, TestsInvolved: true},
		Candidates: ladder(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if afterFailure.Tier == "t1" {
		t.Error("a turn straight after a test failure stayed at the bottom")
	}
}

func TestNonTestFailureIsNotCalledATestFailure(t *testing.T) {
	got, err := Heuristic{}.Route(Input{
		Prompt:     "run tests now",
		Session:    SessionFeatures{PriorFailures: 1, TestsInvolved: true},
		Candidates: ladder(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Rationale, "test failure") {
		t.Fatalf("unrelated tool error was mislabeled: %s", got.Rationale)
	}
}

func TestContextFilterUsesSeparateConservativeBound(t *testing.T) {
	tight := candidate("t1", 0)
	tight.Info.ContextWindow = 1_100
	tight.PromptTokens = 1_000
	tight.ContextTokens = 1_316
	tight.Estimate = costmodel.Estimate{Expected: 123, High: 456}
	roomy := candidate("t2", 1)

	got, err := Heuristic{}.Route(Input{Candidates: []Candidate{tight, roomy}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Tier != "t2" {
		t.Fatalf("conservative context bound chose %s; exclusions=%v", got.Tier, got.Infeasible)
	}
	if tight.PromptTokens != 1_000 || tight.Estimate.Expected != 123 {
		t.Fatal("context widening mutated the dollar-estimate input")
	}
}

func TestContextFilterIncludesReservedOutputEnvelope(t *testing.T) {
	tight := candidate("t1", 0)
	tight.Info.ContextWindow = 200_000
	tight.ContextTokens = 199_000
	tight.ReservedOutputTokens = 8_192
	roomy := candidate("t2", 1)
	roomy.Info.ContextWindow = 1_000_000
	roomy.ContextTokens = tight.ContextTokens
	roomy.ReservedOutputTokens = tight.ReservedOutputTokens

	got, err := (Heuristic{}).Route(Input{Candidates: []Candidate{tight, roomy}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Tier != "t2" {
		t.Fatalf("input-only context check chose %s; exclusions=%v", got.Tier, got.Infeasible)
	}
	joined := strings.Join(got.Infeasible, " ")
	if !strings.Contains(joined, "199000 input") || !strings.Contains(joined, "8192 reserved output") {
		t.Fatalf("context exclusion omitted the envelope: %v", got.Infeasible)
	}
}

func TestContextFilterExplainsUnknownOutputWithoutPrintingSentinel(t *testing.T) {
	unknown := candidate("t1", 0)
	unknown.Info.ContextWindow = 32_768
	unknown.ContextTokens = 100
	unknown.ReservedOutputTokens = math.MaxInt

	decision, err := (Heuristic{}).Route(Input{Candidates: []Candidate{unknown}})
	if err == nil {
		t.Fatal("unknown output bound unexpectedly fit a finite context window")
	}
	text := err.Error() + " " + strings.Join(decision.Infeasible, " ")
	for _, want := range []string{"no finite output bound", "positive tier max_output", "/models", "config"} {
		if !strings.Contains(text, want) {
			t.Fatalf("unknown-bound refusal omitted %q: %s", want, text)
		}
	}
	if strings.Contains(text, strconv.Itoa(math.MaxInt)) {
		t.Fatalf("unknown-bound refusal printed its implementation sentinel: %s", text)
	}
}

func TestContextFilterExplainsExplicitCapReasoningConflict(t *testing.T) {
	conflict := candidate("t1", 0)
	conflict.Target.Params.MaxOutputTokens = 4096
	conflict.Target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}
	conflict.Info.ContextWindow = 200_000
	conflict.ContextTokens = 100
	conflict.ReservedOutputTokens = math.MaxInt

	decision, err := (Heuristic{}).Route(Input{Candidates: []Candidate{conflict}})
	if err == nil {
		t.Fatal("invalid explicit cap unexpectedly fit")
	}
	text := err.Error() + " " + strings.Join(decision.Infeasible, " ")
	for _, want := range []string{"no valid finite output allowance", "configured max_output 4096", "reasoning", "raise max_output", "lower or disable reasoning"} {
		if !strings.Contains(text, want) {
			t.Fatalf("explicit-cap refusal omitted %q: %s", want, text)
		}
	}
	if strings.Contains(text, "set a positive tier max_output") || strings.Contains(text, strconv.Itoa(math.MaxInt)) {
		t.Fatalf("explicit-cap refusal gave omitted-cap/sentinel advice: %s", text)
	}
}

// Intent is not difficulty. The word "refactor" on a one-line request should
// nudge, not jump to the top.
func TestABreadthWordAloneOnlyNudges(t *testing.T) {
	got, err := Heuristic{}.Route(Input{
		Prompt:     "refactor this one function name",
		Candidates: ladder(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Tier == "t3" {
		t.Errorf("a single breadth word jumped to the top of the ladder: %s", got.Rationale)
	}
}

// An infeasible target is not an expensive one. Ordering matters: a target
// excluded by policy must never be reported as one that was out-priced.
func TestFeasibilityIsCheckedBeforeEconomics(t *testing.T) {
	noVision := candidate("t1", 0, func(c *Candidate) { c.Info.Vision = false })
	noTools := candidate("t2", 1, func(c *Candidate) { c.Info.Tools = catalog.ToolsNone })
	tooSmall := candidate("t3", 2, func(c *Candidate) {
		c.Info.ContextWindow = 500
		c.PromptTokens = 90_000
	})
	fine := candidate("t4", 3)

	got, err := Heuristic{}.Route(Input{
		Prompt:       "describe this screenshot and fix the test",
		Candidates:   []Candidate{noVision, noTools, tooSmall, fine},
		Requirements: Requirements{NeedsVision: true, NeedsTools: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Tier != "t4" {
		t.Errorf("tier = %q, want the only feasible one", got.Tier)
	}

	joined := strings.Join(got.Infeasible, "\n")
	for _, want := range []string{"cannot read images", "cannot call tools", "holds 500 tokens"} {
		if !strings.Contains(joined, want) {
			t.Errorf("exclusions do not explain %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "ceiling") {
		t.Error("an infeasible target was reported as too expensive")
	}
}

// A destination that is not approved is infeasible, and no budget makes it
// feasible again.
func TestUnapprovedDestinationIsInfeasible(t *testing.T) {
	elsewhere := candidate("t1", 0)
	elsewhere.Target.Provider = "somewhere-else"

	_, err := Heuristic{}.Route(Input{
		Prompt:       "hello",
		Candidates:   []Candidate{elsewhere},
		Requirements: Requirements{ApprovedProviders: []string{"anthropic"}},
	})
	if err == nil {
		t.Fatal("an unapproved destination was routed to")
	}
	if !strings.Contains(err.Error(), "approved destination") {
		t.Errorf("err = %v", err)
	}
}

// A ceiling is checked against the upper bound. Using the expectation would
// approve a turn that is only affordable on average, which is not what a
// ceiling means.
func TestTheBudgetCeilingUsesTheUpperBound(t *testing.T) {
	pricey := candidate("t2", 1)
	pricey.Estimate = costmodel.Estimate{Expected: 100, High: 5_000}
	cheap := candidate("t1", 0)
	cheap.Estimate = costmodel.Estimate{Expected: 50, High: 200}

	got, err := Heuristic{}.Route(Input{
		Prompt:     "refactor the storage layer across the codebase",
		Session:    SessionFeatures{FilesInContext: 9},
		Candidates: []Candidate{cheap, pricey},
		Budgets:    Budgets{MaxCost: 1_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Tier != "t1" {
		t.Errorf("tier = %q; the expensive one exceeds the ceiling at its upper bound", got.Tier)
	}
	if !strings.Contains(strings.Join(got.Infeasible, " "), "ceiling") {
		t.Errorf("the exclusion did not name the ceiling: %v", got.Infeasible)
	}
}

// §8.1: a pin short-circuits selection only after the hard checks, and an
// infeasible pin is an actionable error rather than a quiet substitution.
func TestAPinIsHonouredButNotAboveThePolicyChecks(t *testing.T) {
	got, err := Heuristic{}.Route(Input{
		Prompt: "anything", Pin: "t3", Candidates: ladder(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Tier != "t3" || got.Source != SourceUserPin {
		t.Errorf("decision = %+v", got)
	}

	blind := candidate("t1", 0, func(c *Candidate) { c.Info.Vision = false })
	_, err = Heuristic{}.Route(Input{
		Prompt: "describe this image", Pin: "t1",
		Candidates:   []Candidate{blind},
		Requirements: Requirements{NeedsVision: true},
	})
	if err == nil {
		t.Fatal("an infeasible pin was served rather than refused")
	}
	if !strings.Contains(err.Error(), "pinned") {
		t.Errorf("err = %v; it has to say the pin was the problem", err)
	}
}

// Confidence is low by construction: these are rules over shallow features, and
// §8.2 composes the chain on confidence thresholds.
func TestConfidenceStaysModest(t *testing.T) {
	got, _ := Heuristic{}.Route(Input{Prompt: "hi", Candidates: ladder()})
	if got.Confidence > 0.7 {
		t.Errorf("confidence = %.2f; a rules router should not claim more", got.Confidence)
	}
}

func TestCandidateOrderCannotChangeTheChosenRank(t *testing.T) {
	ordered := []Candidate{candidate("t1", 0), candidate("t2", 1), candidate("t3", 2)}
	reversed := []Candidate{ordered[2], ordered[1], ordered[0]}
	for _, candidates := range [][]Candidate{ordered, reversed} {
		got, err := (Heuristic{}).Route(Input{Prompt: "refactor this function", Candidates: candidates})
		if err != nil {
			t.Fatal(err)
		}
		if got.Tier != "t2" {
			t.Fatalf("candidate order %v chose %s, want rank-one t2", candidates, got.Tier)
		}
	}
}

func TestHardCeilingUsesConservativeCostWithoutChangingDisplayedEstimate(t *testing.T) {
	cheap := candidate("t1", 0)
	cheap.Estimate = costmodel.Estimate{Expected: 100, High: 150}
	cheap.CeilingCost = 900

	_, err := (Heuristic{}).Route(Input{
		Prompt: "hi", Candidates: []Candidate{cheap}, Budgets: Budgets{MaxCost: 500},
	})
	if err == nil || !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("err = %v, want conservative ceiling refusal", err)
	}
}

func TestExhaustedDollarBudgetStillAllowsZeroDollarTarget(t *testing.T) {
	free := candidate("local", 0)
	free.Estimate = costmodel.Estimate{}
	free.CeilingCost = 0
	paid := candidate("paid", 1)
	paid.CeilingCost = 1

	got, err := (Heuristic{}).Route(Input{
		Prompt:     "fix the repository-wide failure",
		Candidates: []Candidate{free, paid},
		Budgets:    Budgets{MaxCost: 0, MaxCostSet: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Tier != "local" {
		t.Fatalf("exhausted dollar budget chose %s, want the zero-dollar target", got.Tier)
	}
	if !strings.Contains(strings.Join(got.Infeasible, " "), "ceiling") {
		t.Fatalf("paid target was not excluded by the exhausted ceiling: %v", got.Infeasible)
	}
}

func TestPaidTargetWithoutConservativePriceIsHardInfeasible(t *testing.T) {
	unpriceable := candidate("t1", 0)
	unpriceable.CatalogKnown = true
	unpriceable.Info.Metering = catalog.PerToken
	unpriceable.Info.Pricing = []catalog.PriceBand{{InputPerMTok: catalog.PerMTok(1)}}
	unpriceable.CeilingCost = 0
	priceable := candidate("t2", 1)
	priceable.CatalogKnown = true
	priceable.Info.Metering = catalog.PerToken
	priceable.Info.Pricing = []catalog.PriceBand{{InputPerMTok: catalog.PerMTok(1)}}
	priceable.CeilingCost = 1

	got, err := (Heuristic{}).Route(Input{Candidates: []Candidate{unpriceable, priceable}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Tier != "t2" || !strings.Contains(strings.Join(got.Infeasible, " "), "no positive conservative cost bound") {
		t.Fatalf("route = %+v, want priceable fallback and explicit exclusion", got)
	}

	unknown := candidate("unknown", 0)
	if got, err := (Heuristic{}).Route(Input{Candidates: []Candidate{unknown}}); err != nil || got.Tier != "unknown" {
		t.Fatalf("catalog-missing target was treated as a known unpriceable target: got=%+v err=%v", got, err)
	}

	free := candidate("free", 0)
	free.CatalogKnown = true
	free.Info.Metering = catalog.PerToken
	free.Info.Pricing = []catalog.PriceBand{{}}
	if got, err := (Heuristic{}).Route(Input{Candidates: []Candidate{free}}); err != nil || got.Tier != "free" {
		t.Fatalf("explicit all-zero per-token target was rejected: got=%+v err=%v", got, err)
	}
}

func TestStrongEvidenceCanReachTheTopOfALongerLadder(t *testing.T) {
	candidates := []Candidate{
		candidate("t1", 0), candidate("t2", 1), candidate("t3", 2), candidate("t4", 3),
	}
	got, err := (Heuristic{}).Route(Input{
		Prompt: "fix it", Session: SessionFeatures{PriorFailures: 3, TestFailures: 3, TestsInvolved: true}, Candidates: candidates,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Tier != "t4" {
		t.Fatalf("tier = %s, want the top rung after repeated test failures", got.Tier)
	}
}
