package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

func TestPipedInputRidesTheMentionConvention(t *testing.T) {
	got := attachPipedInput("explain this failure", []byte("panic: nil map\ngoroutine 1\n"))
	want := "explain this failure\n\nContents of standard input (piped in):\n```\npanic: nil map\ngoroutine 1\n```"
	if got != want {
		t.Errorf("attachment drifted from the @path convention:\n%q\nwant\n%q", got, want)
	}
	opening := turnOpeningAuthored(got, "explain this failure", nil)
	if authored, known := opening.AuthoredProjection(); !known || authored != "explain this failure" {
		t.Fatalf("piped opening authored projection = %q known=%v", authored, known)
	}
	if !strings.Contains(opening.Text(), "panic: nil map") {
		t.Fatal("piped provider opening lost standard input")
	}
}

func TestEmptyPipeAttachesNothing(t *testing.T) {
	for _, data := range [][]byte{nil, []byte(""), []byte("  \n\n")} {
		if got := attachPipedInput("prompt", data); got != "prompt" {
			t.Errorf("empty stdin %q grew the prompt to %q", data, got)
		}
	}
}

func TestReadPipedInputHasACompleteBound(t *testing.T) {
	atLimit := bytes.Repeat([]byte{'x'}, int(maxPipedInputBytes))
	got, err := readPipedInput(bytes.NewReader(atLimit))
	if err != nil || !bytes.Equal(got, atLimit) {
		t.Fatalf("exact-limit piped input = %d bytes, %v", len(got), err)
	}
	got, err = readPipedInput(bytes.NewReader(append(append([]byte(nil), atLimit...), 'x')))
	if err == nil || got != nil || !strings.Contains(err.Error(), "exceeds") || !strings.Contains(err.Error(), "1048576") {
		t.Fatalf("over-limit piped input = %d bytes, %v; want complete refusal", len(got), err)
	}
}

func TestLastAssistantTextSkipsToolOnlyMessages(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Block{provider.Text{Text: "do it"}}},
		{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "the answer"}}},
		{Role: provider.RoleAssistant, Content: []provider.Block{
			provider.ToolUse{ID: "1", Name: "read", Input: json.RawMessage(`{}`)},
		}},
	}
	if got := lastAssistantText(msgs); got != "the answer" {
		t.Errorf("got %q, want the last assistant message that said anything", got)
	}
	if got := lastAssistantText(nil); got != "" {
		t.Errorf("no messages produced %q", got)
	}
}

func TestHeadlessReportKeepsTheThreeMeteringsApart(t *testing.T) {
	cat, pricedTgt := pricedTarget(t)

	report := func(target provider.RouteTarget, cost int64) headlessReport {
		t.Helper()
		state := session.State{
			ID:              "sess",
			Target:          string(target.ID()),
			CostMicroUSD:    cost,
			CatalogRevision: "test-rev",
			Usage:           provider.Usage{InputTokens: 100, OutputTokens: 10},
		}
		return buildHeadlessReport(state, cat, config.Tier{ID: "t1", Target: target}, nil)
	}

	local := report(provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "qwen3:4b"}, 0)
	if local.Cost.Metering != "local" || local.Cost.EstimatedUSD != nil {
		t.Errorf("a local session must say local and price nothing: %+v", local.Cost)
	}

	plan := report(provider.RouteTarget{Provider: "openai", Surface: "subscription", ModelID: "gpt-5.6-sol"}, 0)
	if plan.Cost.Metering != "plan" || plan.Cost.EstimatedUSD != nil {
		t.Errorf("a plan session must say plan and price nothing: %+v", plan.Cost)
	}
	if !strings.Contains(plan.Cost.Note, "quota") {
		t.Errorf("the plan note must name quota as what was consumed: %q", plan.Cost.Note)
	}

	priced := report(pricedTgt, 420_000)
	if priced.Cost.EstimatedUSD == nil {
		t.Fatalf("a dollar-metered session must carry its estimate: %+v", priced.Cost)
	}
	raw, err := json.Marshal(priced)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"estimated_usd":"0.420000"`) {
		t.Errorf("the estimate did not render as a decimal dollar string: %s", raw)
	}

	unknown := report(provider.RouteTarget{Provider: "nowhere", Surface: "none", ModelID: "x"}, 0)
	if unknown.Cost.Metering != "unknown" || unknown.Cost.EstimatedUSD != nil {
		t.Errorf("an uncataloged target must say so rather than price itself: %+v", unknown.Cost)
	}
}

func TestHeadlessReportOutcomes(t *testing.T) {
	cat, target := pricedTarget(t)
	tier := config.Tier{ID: "t1", Target: target}
	state := session.State{ID: "sess", Messages: []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "partial"}}},
	}}

	if rep := buildHeadlessReport(state, cat, tier, nil); rep.Outcome != "completed" || rep.Error != "" {
		t.Errorf("a clean turn reported %q / %q", rep.Outcome, rep.Error)
	}
	if rep := buildHeadlessReport(state, cat, tier, context.Canceled); rep.Outcome != "cancelled" {
		t.Errorf("a cancelled turn reported %q", rep.Outcome)
	}
	rep := buildHeadlessReport(state, cat, tier, errors.New("boom"))
	if rep.Outcome != "error" || rep.Error != "boom" {
		t.Errorf("a failed turn reported %q / %q", rep.Outcome, rep.Error)
	}
	if rep.Result != "partial" {
		t.Errorf("a failed turn must still report what was produced, got %q", rep.Result)
	}
}

func TestHeadlessReportKeepsRetryReserveSeparateFromObservedEstimate(t *testing.T) {
	cat, target := pricedTarget(t)
	rep := buildHeadlessReport(session.State{
		ID:                   "sess",
		Target:               string(target.ID()),
		CostMicroUSD:         400_000,
		RetryReserveMicroUSD: 100_000,
		CatalogRevision:      "rev",
	}, cat, config.Tier{ID: "t1", Target: target}, nil)
	if rep.Cost.EstimatedUSD == nil || *rep.Cost.EstimatedUSD != 400_000 {
		t.Fatalf("observed estimate = %+v", rep.Cost.EstimatedUSD)
	}
	if rep.Cost.RetryReserveUSD == nil || *rep.Cost.RetryReserveUSD != 100_000 {
		t.Fatalf("separate retry reserve = %+v", rep.Cost.RetryReserveUSD)
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, `"estimated_usd":"0.400000"`) || !strings.Contains(text, `"retry_reserve_usd":"0.100000"`) {
		t.Fatalf("headless JSON merged or omitted accounting: %s", raw)
	}
}
