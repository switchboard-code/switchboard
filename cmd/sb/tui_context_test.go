package main

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/provider/anthropic"
	"github.com/switchboard-code/switchboard/internal/session"
)

func sessionUsageFor(in, out int) session.Usage {
	return session.Usage{Usage: provider.Usage{InputTokens: in, OutputTokens: out}}
}

func TestOneShotContextGateExplainsUnknownOutputWithoutSentinel(t *testing.T) {
	target := provider.RouteTarget{Provider: "openaicompat", Surface: "generic", ModelID: "custom"}
	err := checkRequestContext(nil, target, provider.Request{
		Messages: []provider.Message{provider.UserText("summarize")},
	}, nil, 32_768)
	if err == nil {
		t.Fatal("one-shot request with unknown output bound passed a finite window")
	}
	text := err.Error()
	for _, want := range []string{"no finite output bound", "positive tier max_output", "/models", "config"} {
		if !strings.Contains(text, want) {
			t.Fatalf("one-shot refusal omitted %q: %s", want, text)
		}
	}
	if strings.Contains(text, strconv.Itoa(math.MaxInt)) {
		t.Fatalf("one-shot refusal printed its implementation sentinel: %s", text)
	}
}

func TestOneShotContextGateExplainsExplicitCapReasoningConflict(t *testing.T) {
	target := anthropic.Target("claude-haiku-4-5")
	target.Params.MaxOutputTokens = 4096
	target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}
	err := checkRequestContext(anthropic.New(), target, provider.Request{
		Messages: []provider.Message{provider.UserText("summarize")},
	}, nil, 200_000)
	if err == nil {
		t.Fatal("one-shot request with conflicting explicit cap passed")
	}
	var capErr *provider.CapabilityError
	if !errors.As(err, &capErr) || capErr.Target != target.ID() || capErr.Capability != "max_output with token-budget reasoning" {
		t.Fatalf("one-shot conflict = %v, want typed CapabilityError for the exact target", err)
	}
	text := err.Error()
	for _, want := range []string{"does not support max_output with token-budget reasoning", "explicit max_output 4096", "16384-token reasoning budget", "raise max_output", "lower or disable reasoning"} {
		if !strings.Contains(text, want) {
			t.Fatalf("one-shot explicit-cap refusal omitted %q: %s", want, text)
		}
	}
	if strings.Contains(text, "set a positive tier max_output") || strings.Contains(text, strconv.Itoa(math.MaxInt)) {
		t.Fatalf("one-shot explicit-cap refusal gave omitted-cap/sentinel advice: %s", text)
	}
}

// A compatible endpoint has no catalog entry and the format has no field for
// its window, so the number was zero, the meter drew nothing, and
// auto-compaction never fired. The session ran until the server refused.
func TestAnUnknownWindowIsSaidRatherThanShownAsEmpty(t *testing.T) {
	m := testModel(t)
	m.app.config.Path = t.TempDir() + "/" + config.FileName
	// The "?" and the compaction gate are both about auto-compaction, so the
	// setting has to be the default-on it is for a real config.
	m.app.config.CompactAuto = true
	m.app.loop.Target = provider.RouteTarget{Provider: "openaicompat", Surface: "generic", ModelID: "local"}
	m.app.loop.Bind(m.app.loop.Binding())

	m.refreshCtxWindow()
	if m.ctxWindow != 0 {
		t.Fatalf("nothing knows this window yet, got %d", m.ctxWindow)
	}
	m.callTokens = 5000
	if _, ok := m.ctxPercent(); ok {
		t.Fatal("a percentage of an unknown window is not a number")
	}
	if !strings.Contains(m.ctxPct(), "?") {
		t.Errorf("an unknown window has to say so: %q", m.ctxPct())
	}
	if m.shouldAutoCompact() {
		t.Fatal("compaction cannot be measured against a window nobody stated")
	}

	// Stating it is what turns the meter and auto-compaction back on.
	if cmd := setContextWindowCmd(m, "32768"); cmd == nil {
		t.Fatal("/context produced nothing")
	} else if n, ok := cmd().(noticeMsg); !ok || n.level == "error" {
		t.Fatalf("/context failed: %#v", cmd())
	}
	if m.ctxWindow != 32768 {
		t.Fatalf("the declared window is %d, want 32768", m.ctxWindow)
	}
	pct, ok := m.ctxPercent()
	if !ok || pct != 15 {
		t.Fatalf("ctxPercent = %d,%v; want 15%% of 32768", pct, ok)
	}
	m.callTokens = 30000
	if !m.shouldAutoCompact() {
		t.Fatal("past the threshold of a stated window, auto-compaction has to fire")
	}

	// It survives the session that set it.
	saved, err := config.LoadFile(m.app.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if got := saved.ProviderForTarget("openaicompat", "generic").ContextWindow; got != 32768 {
		t.Fatalf("the window saved as %d", got)
	}
}

func TestContextWindowSaveFailureLeavesLiveAndRuntimeAllowanceUnchanged(t *testing.T) {
	m := testModel(t)
	m.app.config.Path = t.TempDir() // a directory cannot be replaced by the config file
	m.app.loop.Target = provider.RouteTarget{Provider: "openaicompat", Surface: "generic", ModelID: "local"}
	m.app.loop.Bind(m.app.loop.Binding())
	key := config.ProviderSurfaceKey("openaicompat", "generic")
	m.app.config.SetProviderContextWindow(key, 16_384)
	m.refreshCtxWindow()
	if m.ctxWindow != 16_384 {
		t.Fatalf("fixture runtime window = %d", m.ctxWindow)
	}

	cmd := setContextWindowCmd(m, "32768")
	msg, ok := cmd().(noticeMsg)
	if !ok || msg.level != "error" || !strings.Contains(msg.text, "nothing changed") {
		t.Fatalf("failed /context notice = %#v", msg)
	}
	if got := m.app.config.ProviderForTarget("openaicompat", "generic").ContextWindow; got != 16_384 {
		t.Fatalf("failed save changed live config window to %d", got)
	}
	if m.ctxWindow != 16_384 {
		t.Fatalf("failed save changed runtime window to %d", m.ctxWindow)
	}
}

// An effort change rebinds the target under a parameterized identity, and
// the window the server stated for the model does not move with it. Losing
// the number at that seam is what disarmed the meter and auto-compaction
// after a /think on a target the catalog cannot window, local servers
// included.
func TestProbedWindowSurvivesAnEffortChange(t *testing.T) {
	base := provider.RouteTarget{Provider: "openaicompat", Surface: "generic", ModelID: "local"}
	p := &providers{windows: map[string]probedWindow{bareTargetKey(base): {tokens: 128000}}}

	moved := base
	moved.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}
	if moved.ID() == base.ID() {
		t.Fatal("an effort change did not change the target identity; the test proves nothing")
	}
	if got, _ := p.probedContextWindow(moved); got != 128000 {
		t.Fatalf("window under the rebound identity = %d, want the 128000 the server stated", got)
	}
}

// The user is the better witness when the server's own fields contradict
// each other: a declared window outranks a metadata-inferred probe, and only
// an enforced window outranks the declaration.
func TestDeclaredWindowOutranksAnInferredProbe(t *testing.T) {
	m := testModel(t)
	target := provider.RouteTarget{Provider: "openaicompat", Surface: "generic", ModelID: "local"}
	m.app.loop.Bind(agent.Binding{Target: target})

	m.app.providers = &providers{windows: map[string]probedWindow{
		bareTargetKey(target): {tokens: 103168, enforced: false},
	}}
	m.app.config.Providers = map[string]config.ProviderSettings{
		"openaicompat/generic": {ContextWindow: 262144},
	}
	m.refreshCtxWindow()
	if m.ctxWindow != 262144 {
		t.Fatalf("an inferred probe beat the declared window: %d", m.ctxWindow)
	}

	m.app.providers.windows[bareTargetKey(target)] = probedWindow{tokens: 103168, enforced: true}
	m.refreshCtxWindow()
	if m.ctxWindow != 103168 {
		t.Fatalf("an enforced probe lost to the declaration: %d", m.ctxWindow)
	}
}

// A server that answers with no usage block left occupancy at zero, which the
// meter read as an empty window and auto-compaction read as nothing to do.
func TestOccupancyFallsBackToTheEstimatorWhenNothingIsReported(t *testing.T) {
	m := testModel(t)
	m.ctxWindow = 10000

	m.Update(usageMsg{u: sessionUsageFor(0, 0)})
	if m.callTokens != 0 || m.callEstimated {
		// An empty session estimates to nearly nothing; the point is only
		// that the path does not panic and does not invent a number.
		t.Logf("empty session estimated %d", m.callTokens)
	}

	// With a conversation in the session, the estimate stands in.
	m.app.loop.System = []provider.Block{provider.Text{Text: strings.Repeat("system ", 500)}}
	m.Update(usageMsg{u: sessionUsageFor(0, 0)})
	if m.callTokens <= 0 {
		t.Fatal("an unreported turn should fall back to a local count, not stay at zero")
	}
	if !m.callEstimated {
		t.Fatal("an estimated occupancy has to be marked as one")
	}
	if !strings.Contains(m.ctxPct(), "~") {
		t.Errorf("an estimate should be shown as approximate: %q", m.ctxPct())
	}

	// A provider that does report wins, and drops the marker.
	m.Update(usageMsg{u: sessionUsageFor(1234, 10)})
	if m.callTokens != 1234 || m.callEstimated {
		t.Fatalf("reported usage should win: tokens=%d estimated=%v", m.callTokens, m.callEstimated)
	}
}
