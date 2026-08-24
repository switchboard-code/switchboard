package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

func TestCostCLIGrandTotalDeduplicatesForkedProviderCalls(t *testing.T) {
	cat, target := pricedTarget(t)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	source, err := store.Create(workspace, target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []provider.Message{provider.UserText("one call"), {Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "done"}}}} {
		if err := source.AppendMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	if err := source.AppendUsage(session.Usage{Target: string(target.ID()), Usage: provider.Usage{InputTokens: 10}, CostMicroUSD: int64(catalog.USD)}); err != nil {
		t.Fatal(err)
	}
	fork, err := store.ForkSession(source, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	defer fork.Close()

	left, err := session.ReadAccountingLedger(source.Path())
	if err != nil {
		t.Fatal(err)
	}
	right, err := session.ReadAccountingLedger(fork.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(left.Calls) != 1 || len(right.Calls) != 1 || left.Calls[0].CallID == "" || left.Calls[0].CallID != right.Calls[0].CallID {
		t.Fatalf("fork did not preserve durable call identity: left=%+v right=%+v", left.Calls, right.Calls)
	}

	var out strings.Builder
	if err := runCostCLI(&out, store, cat, workspace); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if strings.Count(text, "$1.00") < 3 { // two inherited rows plus one unique grand total
		t.Fatalf("per-session accounting disappeared:\n%s", text)
	}
	if strings.Contains(text, "$2.00 estimated") {
		t.Fatalf("grand total double-counted one provider call across a fork:\n%s", text)
	}
}

func TestCostCLIReportsRetryReserveSeparately(t *testing.T) {
	cat, target := pricedTarget(t)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	sess, err := store.Create(workspace, target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.AppendUsage(session.Usage{Target: string(target.ID()), CostMicroUSD: 400_000}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendRetryReserve(100_000); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := runCostCLI(&out, store, cat, workspace); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "$0.4000 + $0.1000 retry reserve (not observed cost)") ||
		!strings.Contains(text, "$0.1000 retry reserve remains separate") {
		t.Fatalf("retry reserve was hidden or merged:\n%s", text)
	}
	if strings.Contains(text, "$0.5000") {
		t.Fatalf("reserve was merged into observed cost:\n%s", text)
	}
}

func TestWorkspaceAccountingSaturatesInsteadOfWrapping(t *testing.T) {
	got, _ := aggregateWorkspaceAccounting([]session.AccountingLedger{{Calls: []session.Usage{
		{CallID: "a", CostMicroUSD: math.MaxInt64},
		{CallID: "b", CostMicroUSD: 1},
	}}})
	if got != catalog.MaxMoney || got < 0 {
		t.Fatalf("workspace total wrapped: %d", got)
	}
}

func TestWorkspaceAccountingDoesNotCountRaceArmUsageAndOriginMirrorTwice(t *testing.T) {
	shared := session.Usage{CallID: "prefix", CostMicroUSD: 10}
	ledgers := []session.AccountingLedger{
		{SessionID: "origin", Calls: []session.Usage{shared}, ExternalCharges: []session.AccountingCharge{
			{ID: "attempt:a", CostMicroUSD: 40}, {ID: "attempt:b", CostMicroUSD: 60},
		}},
		{SessionID: "arm-a", Calls: []session.Usage{shared, {CallID: "a", CostMicroUSD: 40}}},
		{SessionID: "arm-b", Calls: []session.Usage{shared, {CallID: "b", CostMicroUSD: 60}}, Races: []session.Race{{
			A: session.RaceArm{SessionID: "arm-a", CostMicroUSD: 40},
			B: session.RaceArm{SessionID: "arm-b", CostMicroUSD: 60},
		}}},
	}
	observed, _ := aggregateWorkspaceAccounting(ledgers)
	if observed != 110 { // shared prefix once, then each real arm call once
		t.Fatalf("race lineage total = %d, want 110", observed)
	}
}

func TestWorkspaceAccountingDeduplicatesAttemptReserveAcrossForks(t *testing.T) {
	charge := session.AccountingCharge{ID: "attempt:one", CostMicroUSD: 123}
	_, reserve := aggregateWorkspaceAccounting([]session.AccountingLedger{
		{SessionID: "a", RetryReserves: []session.AccountingCharge{charge}},
		{SessionID: "b", RetryReserves: []session.AccountingCharge{charge}},
	})
	if reserve != 123 {
		t.Fatalf("retry reserve lineage total = %d, want 123", reserve)
	}
}

func TestSummaryReportsRetryReserveOnItsOwnLine(t *testing.T) {
	cat, target := pricedTarget(t)
	lines := summaryLines(session.State{
		CostMicroUSD:         400_000,
		RetryReserveMicroUSD: 100_000,
		CatalogRevision:      "rev",
	}, cat, target)
	text := strings.Join(lines, "\n")
	if !strings.Contains(text, "estimated $0.4000") ||
		!strings.Contains(text, "retry reserve $0.1000") ||
		!strings.Contains(text, "not observed cost") {
		t.Fatalf("summary merged or hid retry reserve:\n%s", text)
	}
}

func TestObservedDollarsStayVisibleAcrossLocalPaidMoves(t *testing.T) {
	cat, paid := pricedTarget(t)
	local := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "qwen3:4b"}
	for _, tc := range []struct {
		name        string
		startedOn   provider.RouteTarget
		currentlyOn provider.RouteTarget
	}{
		{name: "local-to-paid", startedOn: local, currentlyOn: paid},
		{name: "paid-to-local", startedOn: paid, currentlyOn: local},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := session.State{
				ID: "session", Target: string(tc.startedOn.ID()), CostMicroUSD: 420_000,
				CatalogRevision: "test-rev",
			}
			var priced, localCount, plan, unpriced, mixed int
			word := costWord(cat, state, &priced, &localCount, &plan, &unpriced, &mixed)
			if !strings.Contains(word, "$0.4200") || priced != 1 || localCount != 0 || plan != 0 || unpriced != 0 {
				t.Fatalf("cost row hid routed dollars: word=%q counts=%d/%d/%d/%d", word, priced, localCount, plan, unpriced)
			}

			summary := strings.Join(summaryLines(state, cat, tc.currentlyOn), "\n")
			if !strings.Contains(summary, "estimated $0.4200") || strings.Contains(summary, "nothing to bill") {
				t.Fatalf("summary hid routed dollars:\n%s", summary)
			}

			report := buildHeadlessReport(state, cat, config.Tier{ID: "t1", Target: tc.currentlyOn}, nil)
			if report.Cost.EstimatedUSD == nil || *report.Cost.EstimatedUSD != 420_000 || report.Cost.Metering != string(catalog.PerToken) {
				t.Fatalf("headless report hid routed dollars: %+v", report.Cost)
			}
			raw, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), `"estimated_usd":"0.420000"`) {
				t.Fatalf("headless JSON hid routed dollars: %s", raw)
			}
		})
	}
}

func TestZeroDollarRoutedMeteringsRemainMixed(t *testing.T) {
	cat, _ := pricedTarget(t)
	local := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "qwen3:4b"}
	planTarget := provider.RouteTarget{Provider: "openai", Surface: "subscription", ModelID: "gpt-5.6-sol"}
	state := session.State{
		ID: "session", Target: string(local.ID()),
		UsageTargets: []string{string(local.ID()), string(planTarget.ID())},
	}
	for _, current := range []provider.RouteTarget{local, planTarget} {
		var priced, localCount, plan, unpriced, mixed int
		word := costWord(cat, state, &priced, &localCount, &plan, &unpriced, &mixed)
		if !strings.Contains(word, "mixed") || !strings.Contains(word, "local + plan") || mixed != 1 {
			t.Fatalf("cost row flattened zero-dollar routed calls: word=%q mixed=%d", word, mixed)
		}
		summary := strings.Join(summaryLines(state, cat, current), "\n")
		if !strings.Contains(summary, "mixed metering") || !strings.Contains(summary, "local + plan") {
			t.Fatalf("summary flattened zero-dollar routed calls:\n%s", summary)
		}
		report := buildHeadlessReport(state, cat, config.Tier{ID: "t1", Target: current}, nil)
		if report.Cost.Metering != "mixed" || !strings.Contains(report.Cost.Note, "local + plan") || report.Cost.EstimatedUSD != nil {
			t.Fatalf("headless report flattened zero-dollar routed calls: %+v", report.Cost)
		}
	}
}

func TestCostCLIKeepsTheThreeMeteringsApart(t *testing.T) {
	cat, pricedTgt := pricedTarget(t)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()

	record := func(target provider.RouteTargetID, cost int64) {
		t.Helper()
		sess, err := store.Create(workspace, target, "test")
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		if err := sess.AppendUsage(session.Usage{
			Usage:        provider.Usage{InputTokens: 1000, OutputTokens: 100},
			CostMicroUSD: cost,
		}); err != nil {
			t.Fatal(err)
		}
	}
	record(pricedTgt.ID(), 420_000) // $0.42 on a dollar-metered target
	record(provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "qwen3:4b"}.ID(), 0)

	var b strings.Builder
	if err := runCostCLI(&b, store, cat, workspace); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"$0.42", "local", "bill dollars", "nothing to bill", "not the provider's invoice"} {
		if !strings.Contains(out, want) {
			t.Errorf("sb cost output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "$0.00") {
		t.Errorf("a local session was rendered as free money rather than as local:\n%s", out)
	}
}

func TestCostCLISaysSoWhenEmpty(t *testing.T) {
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := runCostCLI(&b, store, cat, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "no sessions recorded") {
		t.Fatalf("empty workspace output: %s", b.String())
	}
}

// The per-ask receipt: turns ordered by what they billed, each beside its
// own words, with the unbilled meterings folded rather than rendered as
// free money.
func TestCostTurnsOrdersAsksByBill(t *testing.T) {
	turns := []session.TurnCost{
		{Turn: 1, Prompt: "cheap warmup", PromptAuthoredKnown: true, Calls: 2, Usage: provider.Usage{InputTokens: 900, OutputTokens: 80}},
		{Turn: 2, Prompt: "the expensive refactor", PromptAuthoredKnown: true, Calls: 6, Usage: provider.Usage{InputTokens: 40_000, OutputTokens: 2_000}, CostMicroUSD: 840_000},
		{Turn: 3, Prompt: "a smaller fix", PromptAuthoredKnown: true, Calls: 3, Usage: provider.Usage{InputTokens: 9_000, OutputTokens: 400}, CostMicroUSD: 310_000},
	}
	out := strings.Join(costTurnsLines(turns), "\n")

	expensive := strings.Index(out, "the expensive refactor")
	smaller := strings.Index(out, "a smaller fix")
	if expensive < 0 || smaller < 0 || expensive > smaller {
		t.Errorf("the dearest ask should lead:\n%s", out)
	}
	if !strings.Contains(out, "$0.8400") || !strings.Contains(out, "$0.3100") {
		t.Errorf("the bills are missing:\n%s", out)
	}
	if !strings.Contains(out, "1 turn billed nothing") {
		t.Errorf("the unbilled fold is missing:\n%s", out)
	}
	if strings.Contains(out, "$0.00") {
		t.Errorf("something rendered as free money:\n%s", out)
	}
}

func TestCostTurnsLabelsBackgroundBuckets(t *testing.T) {
	out := strings.Join(costTurnsLines([]session.TurnCost{
		{Turn: 1, Purpose: session.UsagePurposeTurn, Prompt: "user ask", PromptAuthoredKnown: true, Calls: 1, CostMicroUSD: 100},
		{Purpose: session.UsagePurposeAdvisor, Calls: 1, Usage: provider.Usage{InputTokens: 50}, CostMicroUSD: 200},
	}), "\n")
	if !strings.Contains(out, "background/advisor") || !strings.Contains(out, "not attributed to a user turn") {
		t.Fatalf("background work was not explicit:\n%s", out)
	}
}

func TestCostTurnsWithholdsUnknownLegacyPromptBytes(t *testing.T) {
	const expanded = "inspect @private.env EXPANDED_FILE_BYTES INJECTED_TOOL_OUTPUT"
	out := strings.Join(costTurnsLines([]session.TurnCost{{
		Turn: 1, Purpose: session.UsagePurposeTurn, Prompt: expanded,
		PromptAuthoredKnown: false, Calls: 1, CostMicroUSD: 100,
	}}), "\n")
	if strings.Contains(out, "private.env") || strings.Contains(out, "EXPANDED_FILE_BYTES") ||
		strings.Contains(out, "INJECTED_TOOL_OUTPUT") {
		t.Fatalf("legacy provider-visible content escaped /cost turns:\n%s", out)
	}
	if !strings.Contains(out, "authored wording unavailable for this legacy turn") {
		t.Fatalf("withheld legacy provenance is not explained:\n%s", out)
	}
}

// The reader folds usage records onto the turn whose opening they follow.
func TestReadTurnCostsFoldsUsageOntoItsTurn(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := provider.RouteTargetID("ollama/local/qwen3:4b")
	sess, err := store.Create(t.TempDir(), target, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("first ask")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendUsage(session.Usage{Target: string(target), Usage: provider.Usage{InputTokens: 100, OutputTokens: 10}}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendUsage(session.Usage{Target: string(target), Usage: provider.Usage{InputTokens: 200, OutputTokens: 20}, CostMicroUSD: 5_000}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("second ask")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendUsage(session.Usage{Target: string(target), Usage: provider.Usage{InputTokens: 50, OutputTokens: 5}}); err != nil {
		t.Fatal(err)
	}
	path := sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	turns, err := session.ReadTurnCosts(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("two turns recorded, %d read: %+v", len(turns), turns)
	}
	first := turns[0]
	if first.Calls != 2 || first.Usage.InputTokens != 300 || first.CostMicroUSD != 5_000 ||
		first.Prompt != "first ask" || !first.PromptAuthoredKnown {
		t.Errorf("the first turn's metering drifted: %+v", first)
	}
	if turns[1].Calls != 1 || turns[1].Usage.InputTokens != 50 {
		t.Errorf("the second turn's metering drifted: %+v", turns[1])
	}
}
