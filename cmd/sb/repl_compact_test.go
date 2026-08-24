package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/prefix"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

func discardRenderer(t *testing.T) (*renderer, *os.File) {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return newRenderer(f), f
}

func TestREPLCompactPublishesTheAdoptedSession(t *testing.T) {
	r, _, _ := newOverrideREPL(t, "small")
	source := r.loop.Session
	if err := source.AppendRuntimeBinding(r.tier.ID, r.tier.Target.ID(), true); err != nil {
		t.Fatal(err)
	}
	wantBinding := source.State().RuntimeBinding
	if err := source.AppendMessage(provider.UserText("keep going after compaction")); err != nil {
		t.Fatal(err)
	}
	if err := source.AppendMessage(provider.Message{Role: provider.RoleAssistant,
		Content: []provider.Block{provider.Text{Text: "working"}}}); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Dir(filepath.Dir(source.Path())))
	if err != nil {
		t.Fatal(err)
	}
	r.store = store
	binding := r.loop.Binding()
	binding.Provider = &racedProvider{turns: []racedTurn{racedText(testCompactHandoff("Continue the active objective."))}}
	r.loop.Bind(binding)

	r.compact(context.Background(), "")
	fresh := r.loop.Session
	if fresh == source || fresh.ID() == source.ID() {
		t.Fatal("successful REPL compact did not adopt a fresh session")
	}
	if fresh.PublicationPending() {
		t.Fatal("successful REPL compact left its fresh session unpublished")
	}
	if got := fresh.State().RuntimeBinding; got != wantBinding {
		t.Fatalf("compacted runtime binding = %+v, want source posture %+v", got, wantBinding)
	}
	freshID := fresh.ID()
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(freshID)
	if err != nil {
		t.Fatalf("published compact session was not discoverable: %v", err)
	}
	defer reopened.Close()
	if reopened.ID() != freshID {
		t.Fatalf("reopened session = %s, want compacted %s", reopened.ID(), freshID)
	}
	if got := reopened.State().RuntimeBinding; got != wantBinding {
		t.Fatalf("reopened compact binding = %+v, want source posture %+v", got, wantBinding)
	}
}

func TestREPLCompactPublicationFailureRollsBackAndRetainsHiddenStage(t *testing.T) {
	r, _, _ := newOverrideREPL(t, "small")
	source := r.loop.Session
	store, err := session.NewStore(filepath.Dir(filepath.Dir(source.Path())))
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := store.CreateStaged(r.workspace, r.tier.Target.ID(), r.catalog.Revision)
	if err != nil {
		t.Fatal(err)
	}
	stagedID := fresh.ID()
	stagedPath := fresh.Path()
	if err := os.WriteFile(stagedPath+".published", []byte("injected publication collision"), 0o600); err != nil {
		t.Fatal(err)
	}

	err = adoptREPLCompaction(r.loop, source, fresh)
	if err == nil || !strings.Contains(err.Error(), "could not publish") || !strings.Contains(err.Error(), "session unchanged") {
		t.Fatalf("publication failure = %v", err)
	}
	if r.loop.Session != source {
		t.Fatal("publication failure left the REPL bound to the staged session")
	}
	assertRetainedUnpublishedStage(t, store, r.workspace, stagedID, stagedPath)
	if err := source.AppendNote("info", "source remains writable after compact rollback"); err != nil {
		t.Fatalf("source was not usable after compact rollback: %v", err)
	}
}

func TestREPLCompactRollbackFailureStopsBeforeAnyContinuationOrInput(t *testing.T) {
	r, _, _ := newOverrideREPL(t, "small")
	source := r.loop.Session
	if err := source.AppendMessage(provider.UserText("keep working after compaction")); err != nil {
		t.Fatal(err)
	}
	if err := source.AppendMessage(provider.Message{Role: provider.RoleAssistant,
		Content: []provider.Block{provider.Text{Text: "work is still in progress"}}}); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Dir(filepath.Dir(source.Path())))
	if err != nil {
		t.Fatal(err)
	}
	r.store = store
	binding := r.loop.Binding()
	binding.Provider = &racedProvider{turns: []racedTurn{racedText(testCompactHandoff("Continue the active objective."))}}
	r.loop.Bind(binding)
	r.publishDurably = func(*session.Session) (session.PublicationOutcome, error) {
		return session.PublicationOutcome{}, errors.Join(
			errors.New("injected invisible compact publication failure"),
			poisonSessionRollback(source),
		)
	}
	r.in = bufio.NewReader(strings.NewReader("user input that must remain unread\n"))

	r.compact(context.Background(), "")
	fresh := r.loop.Session
	if fresh == source {
		t.Fatal("double-fault fixture did not leave the loop on the failed compact child")
	}
	var restart *publicationRestartRequiredError
	if !errors.As(r.restartRequired, &restart) ||
		!strings.Contains(r.restartRequired.Error(), "source-session rollback also failed") {
		t.Fatalf("REPL double-fault restart state = %v", r.restartRequired)
	}
	for _, message := range fresh.State().Messages {
		if strings.Contains(message.Text(), compactContinuePrompt) {
			t.Fatal("REPL sent its automatic continuation after failed rollback")
		}
	}
	if err := fresh.AppendNote("info", "must not append"); err == nil {
		t.Fatal("failed compact child remained writable")
	}
	if err := source.AppendNote("info", "must not append"); err == nil {
		t.Fatal("failed compact source remained writable")
	}
	assertRetainedUnpublishedStage(t, r.store, r.workspace, fresh.ID(), fresh.Path())

	interactiveErr := r.interactive(context.Background())
	if !errors.As(interactiveErr, &restart) {
		t.Fatalf("interactive loop did not stop on restart-required error: %v", interactiveErr)
	}
	remaining, err := r.in.ReadString('\n')
	if err != nil || remaining != "user input that must remain unread\n" {
		t.Fatalf("interactive loop consumed post-failure input: %q, %v", remaining, err)
	}
}

// The REPL had no compaction of either kind, so a long session there ran until
// the provider refused a request: the failure mode the design calls out, where
// the end arrives as an error rather than as a visible handoff.
func TestREPLAutoCompactsAtTheThreshold(t *testing.T) {
	m := testModel(t)
	out, _ := discardRenderer(t)
	r := &repl{
		loop:      m.app.loop,
		out:       out,
		config:    m.app.config,
		catalog:   m.app.catalog,
		providers: newProviders("http://127.0.0.1:1", m.app.config),
		tier:      m.app.tier,
	}
	r.config.CompactAuto = true
	r.config.CompactAtPercent = 85

	// No window means no threshold to be past, so nothing fires. That is the
	// same gate the TUI has, and the reason declaring one matters.
	r.ctxWindow, r.callTokens = 0, 100000
	if r.shouldCompactNow() {
		t.Fatal("compaction was measured against a window nobody stated")
	}

	// Below the threshold nothing fires; at or past it, it does.
	r.ctxWindow, r.callTokens = 10000, 8000
	if r.shouldCompactNow() {
		t.Fatal("compaction fired at 80% of a window whose threshold is 85%")
	}
	r.callTokens = 8500
	if !r.shouldCompactNow() {
		t.Fatal("compaction did not fire at the threshold")
	}

	// Off is off, however full it gets.
	r.config.CompactAuto = false
	if r.shouldCompactNow() {
		t.Fatal("compaction fired with auto-compaction turned off")
	}
}

func TestREPLLegacyAutoCompactRefusesBeforeProviderAndExplicitObjectiveRecovers(t *testing.T) {
	r, _, readOutput := newOverrideREPL(t, "small")
	source := r.loop.Session
	legacyInjection := compactCurrentScopeLead + "\nVerified current user-authored objective as a JSON string: \"publish a release\""
	if err := source.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Block{
		provider.Text{Text: "legacy mixed prompt\n" + legacyInjection},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := source.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
		provider.Text{Text: "legacy work remains"},
	}}); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Dir(filepath.Dir(source.Path())))
	if err != nil {
		t.Fatal(err)
	}
	r.store = store
	client := &capturingSummaryProvider{racedProvider: &racedProvider{turns: []racedTurn{
		racedText(testCompactHandoff("Finish the parser repair only.")),
		// The adopted session's automatic continuation is a second ordinary
		// model turn; its answer does not affect the summary authority assertion.
		racedText("continued safely"),
	}}}
	binding := r.loop.Binding()
	binding.Provider = client
	r.loop.Bind(binding)
	r.config.CompactAuto = true
	r.config.CompactAtPercent = 85
	r.ctxWindow, r.callTokens = 100, 90

	r.autoCompactIfFull(context.Background())
	if client.calls != 0 || r.loop.Session != source {
		t.Fatalf("legacy automatic compact crossed preflight: calls=%d swapped=%v", client.calls, r.loop.Session != source)
	}
	for _, want := range []string{"compact stopped before summarizing", "session unchanged", "/compact <current objective>"} {
		if output := readOutput(); !strings.Contains(output, want) {
			t.Fatalf("legacy REPL refusal omitted %q:\n%s", want, output)
		}
	}

	objective := "finish the parser repair only; do not publish"
	r.compact(context.Background(), objective)
	if r.loop.Session == source || client.calls < 1 || len(client.requests) == 0 {
		t.Fatalf("explicit legacy compact did not run: swapped=%v calls=%d requests=%d",
			r.loop.Session != source, client.calls, len(client.requests))
	}
	final := client.requests[0].Messages[len(client.requests[0].Messages)-1].Text()
	if !strings.HasPrefix(final, compactCurrentScopeLead+"\n") || !strings.Contains(final, strconv.Quote(objective)) ||
		strings.Contains(final, strconv.Quote("publish a release")) {
		t.Fatalf("REPL explicit current scope envelope:\n%s", final)
	}
}

func TestREPLPostCompactContinuationDoesNotRecursivelyCompact(t *testing.T) {
	r, _, readOutput := newOverrideREPL(t, "small")
	source := r.loop.Session
	if err := source.AppendMessage(provider.UserText("continue after the handoff")); err != nil {
		t.Fatal(err)
	}
	if err := source.AppendMessage(provider.Message{Role: provider.RoleAssistant,
		Content: []provider.Block{provider.Text{Text: "work is in progress"}}}); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Dir(filepath.Dir(source.Path())))
	if err != nil {
		t.Fatal(err)
	}
	r.store = store
	r.config.CompactAuto = true
	r.config.CompactAtPercent = 85
	// The frozen zone survives compaction. A provider can therefore honestly
	// report the fresh continuation above the same threshold even when the
	// conversation handoff is small.
	r.loop.System = []provider.Block{provider.Text{Text: strings.Repeat("frozen policy ", 2_000)}}

	chatCalls, summaryCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{{"name": "small"}}})
		case "/api/show":
			_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": []string{"tools", "vision"}})
		case "/api/chat":
			chatCalls++
			if chatCalls%2 == 1 {
				summaryCalls++
			}
			// The third call is the recursive summary attempt the regression
			// used to make. Stop it deterministically instead of letting a broken
			// implementation recurse until the test process exhausts its stack.
			if chatCalls > 2 {
				http.Error(w, "unexpected recursive compaction", http.StatusInternalServerError)
				return
			}
			content := "continued in the fresh session"
			if chatCalls == 1 {
				content = testCompactHandoff("Continue the active objective in the fresh session.")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message":           map[string]string{"role": "assistant", "content": content},
				"done":              true,
				"done_reason":       "stop",
				"prompt_eval_count": 90_000,
				"eval_count":        1,
			})
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(server.Close)
	r.providers = newProviders(server.URL, r.config)
	tier, client, err := r.providers.probeTier(context.Background(), r.config.Tiers[0])
	if err != nil {
		t.Fatal(err)
	}
	r.tier = tier
	binding := r.loop.Binding()
	binding.Provider, binding.Target = client, tier.Target
	r.loop.Bind(binding)
	r.watcher = newWatcher(r.out, r.sticky, len(r.config.Tiers)-1, nil)
	r.loop.SetObserver(r.watcher)

	r.compact(context.Background(), "")
	if chatCalls != 2 || summaryCalls != 1 {
		t.Fatalf("post-compact calls = %d total / %d summaries, want one summary plus one continuation; output:\n%s",
			chatCalls, summaryCalls, readOutput())
	}
	if r.callTokens != 90_000 {
		t.Fatalf("fresh continuation occupancy = %d, want provider-reported 90000", r.callTokens)
	}
	if !r.shouldCompactNow() {
		t.Fatal("post-compact guard mutated the normal threshold decision for the next turn")
	}
	infos, err := store.List(r.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("sessions after one compact = %d, want source plus one handoff", len(infos))
	}
}

func TestREPLOccupancyUsesTheCurrentTurnsLastReceipt(t *testing.T) {
	r, _, _ := newOverrideREPL(t, "small")
	r.watcher = newWatcher(r.out, r.sticky, len(r.config.Tiers)-1, nil)

	r.watcher.StartTurn()
	first, err := r.loop.Session.AppendUsageRecord(session.Usage{
		Target: string(r.tier.Target.ID()), Usage: provider.Usage{InputTokens: 7_000}, Attempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.watcher.TurnUsage(first)
	r.noteOccupancy()
	if r.callTokens != 7_000 {
		t.Fatalf("first-turn occupancy = %d, want 7000", r.callTokens)
	}

	// The durable ledger now totals 8,000, but the last request occupied only
	// 1,000 tokens. Comparing the cumulative value to a context window made
	// automatic compaction fire earlier after every completed turn.
	r.watcher.StartTurn()
	second, err := r.loop.Session.AppendUsageRecord(session.Usage{
		Target: string(r.tier.Target.ID()), Usage: provider.Usage{InputTokens: 1_000}, Attempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.watcher.TurnUsage(second)
	r.noteOccupancy()
	if got := r.loop.Session.State().Usage.InputTokens; got != 8_000 {
		t.Fatalf("fixture cumulative usage = %d, want 8000", got)
	}
	if r.callTokens != 1_000 {
		t.Fatalf("second-turn occupancy = %d, want last receipt 1000", r.callTokens)
	}
	r.ctxWindow = 10_000
	r.config.CompactAuto = true
	r.config.CompactAtPercent = 85
	if r.shouldCompactNow() {
		t.Fatal("cumulative historical usage triggered current-request compaction")
	}
}

func TestREPLOccupancyEstimatesWhenTheLatestReceiptReportsZero(t *testing.T) {
	r, _, _ := newOverrideREPL(t, "small")
	r.watcher = newWatcher(r.out, r.sticky, len(r.config.Tiers)-1, nil)
	if err := r.loop.Session.AppendMessage(provider.UserText(strings.Repeat("context ", 300))); err != nil {
		t.Fatal(err)
	}
	r.watcher.StartTurn()
	r.watcher.TurnUsage(session.Usage{Target: string(r.tier.Target.ID()), Attempts: 1})

	want := prefix.RequestTokens(r.loop.Request(r.loop.Session.State().Messages))
	r.noteOccupancy()
	if want <= 0 || r.callTokens != want {
		t.Fatalf("zero-usage fallback = %d, want local request estimate %d", r.callTokens, want)
	}
}

func TestHeadlessPromptNeverAutoCompactsOrContinues(t *testing.T) {
	r, capture, readOutput := newOverrideREPL(t, "small")
	store, err := session.NewStore(filepath.Dir(filepath.Dir(r.loop.Session.Path())))
	if err != nil {
		t.Fatal(err)
	}
	r.store = store
	r.config.CompactAuto = true
	r.config.CompactAtPercent = 85
	capture.mu.Lock()
	capture.promptEval = 90_000 // 90% of the fixture target's 100k window.
	capture.mu.Unlock()

	if err := r.onceAuthored(context.Background(), "answer exactly once", "answer exactly once"); err != nil {
		t.Fatal(err)
	}
	capture.mu.Lock()
	calls := len(capture.bodies)
	capture.mu.Unlock()
	if calls != 1 {
		t.Fatalf("headless -p made %d provider calls, want its single no-tool result; output:\n%s", calls, readOutput())
	}
	if strings.Contains(readOutput(), "compacting") || strings.Contains(readOutput(), "automatic continuation") {
		t.Fatalf("headless -p entered an interactive continuation path:\n%s", readOutput())
	}
}

func TestREPLCompactionUsesConfiguredSummarizerSlot(t *testing.T) {
	r, _, readOutput := newOverrideREPL(t, "small")
	source := r.loop.Session
	if err := source.AppendMessage(provider.UserText("preserve this active objective")); err != nil {
		t.Fatal(err)
	}
	if err := source.AppendMessage(provider.Message{Role: provider.RoleAssistant,
		Content: []provider.Block{provider.Text{Text: "working"}}}); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Dir(filepath.Dir(source.Path())))
	if err != nil {
		t.Fatal(err)
	}
	r.store = store

	summarizer := ollamaTier("summary", "summary-only")
	r.config.Tiers = append(r.config.Tiers, summarizer)
	r.config.Slots = map[string]string{"summarizer": summarizer.ID}
	active := r.loop.Binding().Target
	capture := &replRequestCapture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{
				{"name": summarizer.Target.ModelID}, {"name": active.ModelID},
			}})
		case "/api/show":
			_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": []string{"tools"}})
		case "/api/chat":
			body, _ := io.ReadAll(req.Body)
			capture.add(string(body))
			content := "done"
			if strings.Contains(string(body), `"model":"summary-only"`) {
				content = testCompactHandoff("Continue the active objective.")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message":           map[string]string{"role": "assistant", "content": content},
				"done":              true,
				"done_reason":       "stop",
				"prompt_eval_count": 8,
				"eval_count":        1,
			})
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(server.Close)
	r.providers = newProviders(server.URL, r.config)

	r.compact(context.Background(), "")
	if r.loop.Session == source {
		t.Fatalf("slot compaction was not adopted:\n%s", readOutput())
	}
	t.Cleanup(func() { _ = r.loop.Session.Close() })
	if got := r.loop.Session.State().Target; got != string(active.ID()) {
		t.Fatalf("fresh session target = %q, want active %q", got, active.ID())
	}
	if got := source.State().UsageTargets; len(got) != 1 || got[0] != string(summarizer.Target.ID()) {
		t.Fatalf("summary usage targets = %v, want only %q", got, summarizer.Target.ID())
	}
	capture.mu.Lock()
	bodies := append([]string(nil), capture.bodies...)
	capture.mu.Unlock()
	summaryCalls := 0
	for _, body := range bodies {
		if strings.Contains(body, `"model":"summary-only"`) {
			summaryCalls++
		}
	}
	if summaryCalls != 1 {
		t.Fatalf("summarizer requests = %v", bodies)
	}
	if !strings.Contains(readOutput(), "the summarizer slot") {
		t.Fatalf("REPL did not disclose the summarizer target:\n%s", readOutput())
	}
}

func TestREPLManualCompactRefusesUnreachableSummarizerSlot(t *testing.T) {
	r, _, readOutput := newOverrideREPL(t, "small")
	source := r.loop.Session
	if err := source.AppendMessage(provider.UserText("do not replace this session")); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Dir(filepath.Dir(source.Path())))
	if err != nil {
		t.Fatal(err)
	}
	r.store = store
	r.config.Slots = map[string]string{"summarizer": "ollama/missing-summary-model"}
	r.providers = newProviders("http://127.0.0.1:1", r.config)

	r.compact(context.Background(), "")
	if r.loop.Session != source {
		t.Fatal("an unreachable manual summarizer replaced the active session")
	}
	output := readOutput()
	if !strings.Contains(output, "summarizer slot") || !strings.Contains(output, "session unchanged") {
		t.Fatalf("unreachable slot refusal = %q", output)
	}
}

// The window resolves from the same three sources, in the same order, as the
// TUI's: the server, then the user, then the catalog.
func TestREPLResolvesTheContextWindowLikeTheTUI(t *testing.T) {
	m := testModel(t)
	cfg := m.app.config
	out, _ := discardRenderer(t)
	r := &repl{
		loop:      m.app.loop,
		out:       out,
		config:    cfg,
		catalog:   m.app.catalog,
		providers: newProviders("http://127.0.0.1:1", cfg),
		tier:      m.app.tier,
	}
	target := provider.RouteTarget{Provider: "openaicompat", Surface: "generic", ModelID: "local"}
	r.loop.Target = target

	r.refreshCtxWindow()
	if r.ctxWindow != 0 {
		t.Fatalf("nothing knows this window, got %d", r.ctxWindow)
	}
	cfg.SetProviderContextWindow(config.ProviderSurfaceKey("openaicompat", "generic"), 32768)
	r.refreshCtxWindow()
	if r.ctxWindow != 32768 {
		t.Fatalf("a declared window resolved to %d", r.ctxWindow)
	}
}

func TestDiscardRendererDoesNotOwnStandardInput(t *testing.T) {
	if _, err := os.Stdin.Stat(); err != nil {
		t.Skipf("standard input is not open in this test environment: %v", err)
	}
	_, f := discardRenderer(t)
	if f.Fd() == os.Stdin.Fd() {
		t.Fatal("discard renderer reused the standard-input descriptor")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stdin.Stat(); err != nil {
		t.Fatalf("closing the discard renderer closed standard input: %v", err)
	}
}
