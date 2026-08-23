package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/continuity"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

func appendSurfaceContinuity(t *testing.T, sess *session.Session, task string) continuity.Capsule {
	t.Helper()
	stored, err := sess.AppendContinuity(continuity.Capsule{
		Source:    continuity.SourceManual,
		Objective: "finish the continuity integration",
		Tasks:     []continuity.Task{{Text: task, Status: continuity.TaskActive}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func continuityBlockCount(message provider.Message) int {
	count := 0
	for _, block := range message.Content {
		if text, ok := block.(provider.Text); ok && strings.Contains(text.Text, "[continuity ") {
			count++
		}
	}
	return count
}

func TestREPLRoutesAndSendsTheSameStampedOpeningOnce(t *testing.T) {
	r, capture, _ := newOverrideREPL(t, "small", "large")
	prompt := "continue the exact task"
	plain := prospectiveTurnPlan(r.loop, r.sticky, turnOpening(prompt, nil), r.workspace)
	stored := appendSurfaceContinuity(t, r.loop.Session, "verify the provider wire")

	if err := r.turnPrepared(context.Background(), prompt, nil, false); err != nil {
		t.Fatal(err)
	}
	state := r.loop.Session.State()
	if len(state.Messages) < 1 {
		t.Fatal("turn recorded no opening")
	}
	opening := state.Messages[0]
	if opening.ContinuityRef != stored.ID || opening.AuthoredText() != prompt || continuityBlockCount(opening) != 1 {
		t.Fatalf("recorded opening ref=%q authored=%q capsules=%d", opening.ContinuityRef, opening.AuthoredText(), continuityBlockCount(opening))
	}
	if r.routeFeatures.PromptTokens <= plain.PromptTokens || r.routeFeatures.ContextTokens <= plain.ContextTokens {
		t.Fatalf("route estimate ignored capsule: routed=%d/%d plain=%d/%d",
			r.routeFeatures.PromptTokens, r.routeFeatures.ContextTokens, plain.PromptTokens, plain.ContextTokens)
	}
	if len(capture.bodies) != 1 || strings.Count(capture.bodies[0], "[continuity ") != 1 {
		t.Fatalf("provider wire calls=%d capsule count=%d\n%s", len(capture.bodies), func() int {
			if len(capture.bodies) == 0 {
				return 0
			}
			return strings.Count(capture.bodies[0], "[continuity ")
		}(), strings.Join(capture.bodies, "\n"))
	}
	if strings.Contains(opening.AuthoredText(), "[continuity ") {
		t.Fatal("visible authored text exposed the hidden capsule")
	}
}

func TestTUIPlansNormalAndOverrideTurnsWithStampedOpening(t *testing.T) {
	r, _, _ := newOverrideREPL(t, "small", "large")
	stored := appendSurfaceContinuity(t, r.loop.Session, "route both TUI paths")
	app := &tuiApp{
		loop: r.loop, config: r.config, catalog: r.catalog, tier: r.tier,
		providers: r.providers, sticky: r.sticky, budget: r.budget, caches: r.caches,
		workspace: r.workspace,
	}
	m := newTUIModel(app, darkTheme(), newMarkdown(80, true), newTextarea())
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	normal, ok := m.launchTurn("normal visible prompt", nil)().(turnPlanMsg)
	if !ok || normal.err != nil {
		t.Fatalf("normal plan = %#v", normal)
	}
	if normal.opening.ContinuityRef != stored.ID || normal.opening.AuthoredText() != "normal visible prompt" || continuityBlockCount(normal.opening) != 1 {
		t.Fatalf("normal opening = %#v", normal.opening)
	}
	m.finishPlanning()

	override, ok := m.launchOverrideTurn("override visible prompt", nil, r.config.Tiers[1])().(overrideProbeMsg)
	if !ok || override.err != nil {
		t.Fatalf("override plan = %#v", override)
	}
	if override.opening.ContinuityRef != stored.ID || override.opening.AuthoredText() != "override visible prompt" || continuityBlockCount(override.opening) != 1 {
		t.Fatalf("override opening = %#v", override.opening)
	}
	m.finishPlanning()
}

func TestCancelledTUIPlanDoesNotDeliverContinuity(t *testing.T) {
	m := testModel(t)
	stored := appendSurfaceContinuity(t, m.app.loop.Session, "remain pending after cancellation")
	cmd := m.launchTurn("cancel before routing", nil)
	m.interrupt()
	msg, ok := cmd().(turnPlanMsg)
	if !ok || msg.err == nil {
		t.Fatalf("cancelled plan = %#v", msg)
	}
	state := m.app.loop.Session.State()
	if len(state.Messages) != 0 || state.ContinuityRef != "" || state.Continuity == nil || state.Continuity.ID != stored.ID {
		t.Fatalf("cancelled planning delivered or lost continuity: %+v", state)
	}
}

func TestRetryReplaysTheExactRecordedOpeningAndCapsuleOnce(t *testing.T) {
	m := testModel(t)
	appendTurn(t, m, "first question", "first answer")
	stored := appendSurfaceContinuity(t, m.app.loop.Session, "retry the second turn")
	recorded := provider.Message{Role: provider.RoleUser, Content: []provider.Block{
		provider.Text{Text: "second question"},
		provider.Image{MediaType: "image/png", Data: []byte{0x89, 0x50, 0x4e, 0x47}},
		provider.Text{Text: " with exact trailing detail"},
	}}
	var err error
	recorded, err = stampTurnOpening(m.app.loop.Session, recorded)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.app.loop.Session.AppendMessage(recorded); err != nil {
		t.Fatal(err)
	}
	if err := m.app.loop.Session.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "weak answer"}}}); err != nil {
		t.Fatal(err)
	}

	swap, ok := cmdRetry(m, "")().(sessionSwapMsg)
	if !ok || swap.err != nil {
		t.Fatalf("retry swap = %#v", swap)
	}
	defer swap.sess.Close()
	start, ok := swap.andThen().(retryStartMsg)
	if !ok {
		t.Fatalf("retry continuation = %#v", swap.andThen())
	}
	if !reflect.DeepEqual(start.opening, recorded) {
		t.Fatalf("retry flattened or changed the opening:\n got %#v\nwant %#v", start.opening, recorded)
	}
	if start.opening.ContinuityRef != stored.ID || continuityBlockCount(start.opening) != 1 ||
		start.opening.AuthoredText() != "second question with exact trailing detail" {
		t.Fatalf("retry opening ref=%q authored=%q capsules=%d", start.opening.ContinuityRef, start.opening.AuthoredText(), continuityBlockCount(start.opening))
	}
	validated, err := stampRecordedTurnOpening(swap.sess, start.opening)
	if err != nil {
		t.Fatal(err)
	}
	validatedAgain, _, err := swap.sess.StampContinuityOpening(validated)
	if err != nil {
		t.Fatalf("loop idempotent validation failed: %v", err)
	}
	if !reflect.DeepEqual(validatedAgain, recorded) || continuityBlockCount(validatedAgain) != 1 {
		t.Fatal("surface plus loop stamping changed or duplicated the recorded opening")
	}
	if err := swap.sess.AppendMessage(validatedAgain); err != nil {
		t.Fatal(err)
	}
	if got := swap.sess.State().ContinuityRef; got != stored.ID {
		t.Fatalf("retry delivered ref %q, want %q", got, stored.ID)
	}
}

func TestRetryStopsAfterPartialUndo(t *testing.T) {
	m := testModel(t)
	appendTurn(t, m, "edit both files", "done")
	dir := t.TempDir()
	good := filepath.Join(dir, "a-good.txt")
	stale := filepath.Join(dir, "z-stale.txt")
	for _, path := range []string{good, stale} {
		if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rec := checkpoint.NewRecorder()
	rec.Begin("edit both files")
	rec.Record(good)
	rec.Record(stale)
	if err := os.WriteFile(good, []byte("agent edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("agent edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Close the checkpoint scope so its committed post-image is the agent edit;
	// the write below is then correctly recognized as newer external state.
	rec.Begin("later empty turn")
	if err := os.WriteFile(stale, []byte("newer external edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.app.undo = rec
	sourceID := m.app.loop.Session.ID()
	messages := len(m.app.loop.Session.State().Messages)
	semantic := &lspView{}
	m.full = semantic
	workspaceEpoch := m.workspaceRuntime.epoch.Load()

	completion, ok := cmdRetry(m, "")().(noticeMsg)
	if !ok || completion.level != "error" || !strings.Contains(completion.text, "retry stopped before another model ran") ||
		!strings.Contains(completion.text, "not restored") {
		t.Fatalf("partial undo result = %#v", completion)
	}
	if data, _ := os.ReadFile(good); string(data) != "before" {
		t.Fatalf("successful restore was not reported/applied: %q", data)
	}
	if data, _ := os.ReadFile(stale); string(data) != "newer external edit" {
		t.Fatalf("stale file was overwritten: %q", data)
	}
	if m.app.loop.Session.ID() != sourceID || len(m.app.loop.Session.State().Messages) != messages {
		t.Fatal("partial undo still forked or launched a retry")
	}
	if got := m.workspaceRuntime.epoch.Load(); got != workspaceEpoch+1 || !semantic.stale {
		t.Fatalf("partial retry restore left final-code caches live: epoch=%d want=%d lsp-stale=%v", got, workspaceEpoch+1, semantic.stale)
	}
	m.Update(completion)
	if m.operationActive || m.busy {
		t.Fatal("partial undo refusal did not release retry ownership")
	}
}

func TestRetryRefusesPartialCheckpointRepeatedlyWithoutConsumingIt(t *testing.T) {
	m := testModel(t)
	appendTurn(t, m, "edit the oversized file", "done")
	path := filepath.Join(t.TempDir(), "oversized.bin")
	before := []byte(strings.Repeat("a", (4<<20)+1))
	if err := os.WriteFile(path, before, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := checkpoint.NewRecorder()
	rec.Begin("edit the oversized file")
	rec.Record(path)
	if err := os.WriteFile(path, []byte("new bytes that cannot be restored from the bounded checkpoint"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec.Begin("later empty turn")
	m.app.undo = rec
	sourceID := m.app.loop.Session.ID()

	for attempt := 1; attempt <= 2; attempt++ {
		completion, ok := cmdRetry(m, "")().(noticeMsg)
		if !ok || completion.level != "error" || !strings.Contains(completion.text, "checkpoint is partial") ||
			!strings.Contains(completion.text, path) {
			t.Fatalf("retry attempt %d = %#v", attempt, completion)
		}
		m.Update(completion)
		if m.operationActive || m.busy || m.app.loop.Session.ID() != sourceID {
			t.Fatalf("retry attempt %d crossed the partial-checkpoint refusal", attempt)
		}
		details := rec.Details()
		if len(details) == 0 || len(details[len(details)-1].Skipped) != 1 || details[len(details)-1].Skipped[0] != path {
			t.Fatalf("retry attempt %d consumed partial evidence: %+v", attempt, details)
		}
	}
	if data, _ := os.ReadFile(path); string(data) != "new bytes that cannot be restored from the bounded checkpoint" {
		t.Fatalf("partial retry changed the oversized file: %q", data)
	}
}

func runSurfaceTool(t *testing.T, registry *tools.Registry, name string, input any) tools.Result {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := registry.Get(name)
	if !ok {
		t.Fatalf("tool %s is missing", name)
	}
	plan, err := tool.Plan(raw)
	if err != nil {
		t.Fatal(err)
	}
	result, err := plan.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSessionSwapHydratesTodosAndDropsReadAuthority(t *testing.T) {
	m := testModel(t)
	root := m.app.loop.Tools.Root()
	path := filepath.Join(root, "kept.txt")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if result := runSurfaceTool(t, m.app.loop.Tools, "read", map[string]any{"path": "kept.txt"}); result.IsError {
		t.Fatalf("fixture read failed: %s", result.Content)
	}
	if err := m.app.loop.Tools.RestoreTodos([]tools.TodoItem{{Text: "old-session task", Status: tools.TodoActive}}); err != nil {
		t.Fatal(err)
	}

	newSession, err := m.app.store.Create(m.app.workspace, m.app.tier.Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	stored := appendSurfaceContinuity(t, newSession, "new-session task")
	binding := m.app.loop.Binding()
	if cmd := m.onSessionSwap(sessionSwapMsg{sess: newSession, tier: m.app.tier, client: binding.Provider, fresh: false}); cmd != nil {
		t.Fatal("idle swap unexpectedly returned a continuation")
	}
	t.Cleanup(func() { _ = m.app.loop.Session.Close() })
	if m.app.loop.Session != newSession {
		t.Fatal("swap did not publish the new session")
	}
	todos := m.app.loop.Tools.Todos()
	if len(todos) != 1 || todos[0].Text != "new-session task" || newSession.CurrentContinuity().ID != stored.ID {
		t.Fatalf("swap todos=%+v continuity=%+v", todos, newSession.CurrentContinuity())
	}
	write := runSurfaceTool(t, m.app.loop.Tools, "write", map[string]any{"path": "kept.txt", "content": "must reread"})
	if !write.IsError || !strings.Contains(write.Content, "not been read") {
		t.Fatalf("old session read authority survived swap: %+v", write)
	}
}

func TestSessionSwapReplayHidesContinuityAndKeepsOneAuthoredTurn(t *testing.T) {
	m := testModel(t)
	newSession, err := m.app.store.Create(m.app.workspace, m.app.tier.Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	appendSurfaceContinuity(t, newSession, "hide the capsule")
	opening := provider.Message{Role: provider.RoleUser, Content: []provider.Block{
		provider.Text{Text: "visible prompt"},
		provider.Text{Text: " plus detail"},
	}}
	opening, err = stampTurnOpening(newSession, opening)
	if err != nil {
		t.Fatal(err)
	}
	if err := newSession.AppendMessage(opening); err != nil {
		t.Fatal(err)
	}
	if err := newSession.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "answer"}}}); err != nil {
		t.Fatal(err)
	}
	binding := m.app.loop.Binding()
	if cmd := m.onSessionSwap(sessionSwapMsg{sess: newSession, tier: m.app.tier, client: binding.Provider, fresh: false}); cmd != nil {
		t.Fatal("history swap returned a continuation")
	}
	t.Cleanup(func() { _ = m.app.loop.Session.Close() })
	rendered := strings.Join(m.tr.flat, "\n")
	if strings.Contains(rendered, "[continuity ") {
		t.Fatalf("replayed transcript leaked the hidden capsule:\n%s", rendered)
	}
	if strings.Count(rendered, "visible prompt plus detail") != 1 {
		t.Fatalf("replayed multi-text opening did not render once:\n%s", rendered)
	}
}

func TestRaceArmsReceiveOneStampedOpeningAndHydratedTodos(t *testing.T) {
	m := raceModel(t)
	stored := appendSurfaceContinuity(t, m.app.loop.Session, "compare both arms")
	_, generation, sourceID, err := m.startOperation("race setup")
	if err != nil {
		t.Fatal(err)
	}
	probe := raceProbeMsg{
		operation: generation, sourceID: sourceID, prompt: "compare",
		a: m.app.config.Tiers[0], b: m.app.config.Tiers[1],
		ca: &racedProvider{turns: []racedTurn{racedText("answer a")}},
		cb: &racedProvider{turns: []racedTurn{racedText("answer b")}},
	}
	setup, ok := m.startRaceArms(probe, "compare exactly", nil)().(raceSetupMsg)
	if !ok || setup.err != nil {
		t.Fatalf("race setup = %#v", setup)
	}
	defer func() {
		if setup.release != nil {
			setup.release()
		}
		for _, arm := range setup.arms {
			if arm != nil {
				_ = arm.sess.Close()
			}
		}
	}()
	if setup.opening.ContinuityRef != stored.ID || setup.opening.AuthoredText() != "compare exactly" || continuityBlockCount(setup.opening) != 1 {
		t.Fatalf("race opening = %#v", setup.opening)
	}
	for i, arm := range setup.arms {
		if todos := arm.loop.Tools.Todos(); len(todos) != 1 || todos[0].Text != "compare both arms" {
			t.Fatalf("arm %d todos = %+v", i, todos)
		}
		if err := arm.loop.TurnMessage(context.Background(), setup.opening); err != nil {
			t.Fatalf("arm %d turn: %v", i, err)
		}
		messages := arm.sess.State().Messages
		opening := messages[len(m.app.loop.Session.State().Messages)]
		if opening.ContinuityRef != stored.ID || continuityBlockCount(opening) != 1 {
			t.Fatalf("arm %d delivered opening = %#v", i, opening)
		}
	}
	if got := m.app.loop.Session.State().ContinuityRef; got != "" {
		t.Fatalf("race setup delivered capsule to the source: %q", got)
	}
}

func compactWithSummary(t *testing.T, m *tuiModel, summary string) sessionSwapMsg {
	t.Helper()
	client := &racedProvider{turns: []racedTurn{racedText(summary)}}
	binding := m.app.loop.Binding()
	binding.Provider = client
	m.app.loop.Bind(binding)
	m.app.budget = &budgetState{}
	msg := compactCmd(m, "", false)()
	swap, ok := msg.(sessionSwapMsg)
	if !ok || swap.err != nil {
		t.Fatalf("compact result = %#v", msg)
	}
	return swap
}

func TestCompactSwapAuthorsLineageAndDeliversContinuityOnce(t *testing.T) {
	m := testModel(t)
	appendTurn(t, m, "what remains", "keep working")
	source := m.app.loop.Session
	stored := appendSurfaceContinuity(t, source, "finish after compaction")
	if err := m.app.loop.BindSession(source); err != nil {
		t.Fatal(err)
	}

	swap := compactWithSummary(t, m, "Line one.\n\nLine two.\tNext action.")
	freshState := swap.sess.State()
	if freshState.Continuity == nil {
		t.Fatal("compaction authored no continuity capsule")
	}
	capsule := freshState.Continuity
	if capsule.Source != continuity.SourceCompact || capsule.BasisMessages != 0 || capsule.ParentSession != source.ID() ||
		capsule.ParentMessages != 2 || capsule.ParentCapsule != stored.ID || capsule.Narrative != "" {
		t.Fatalf("compacted capsule = %+v", capsule)
	}
	seedOpening := freshState.Messages[0]
	if seedOpening.ContinuityRef != capsule.ID || continuityBlockCount(seedOpening) != 1 ||
		!strings.Contains(seedOpening.AuthoredText(), "Line one.\n\nLine two.\tNext action.") {
		t.Fatalf("compact seed did not carry one hidden capsule beside the unmodified summary: %#v", seedOpening)
	}
	// A compacted session continues on its own now: the swap hands back the
	// continuation's launcher. It is not run here — the harness has no live
	// provider — and planning is closed so the typed first turn below plans
	// cleanly.
	cmd := m.onSessionSwap(swap)
	if cmd == nil {
		t.Fatal("a compacted session did not continue on its own")
	}
	m.finishPlanning()
	t.Cleanup(func() { _ = m.app.loop.Session.Close() })
	if todos := m.app.loop.Tools.Todos(); len(todos) != 1 || todos[0].Text != "finish after compaction" {
		t.Fatalf("compaction did not hydrate todos: %+v", todos)
	}

	// Exercise the actual TUI planning seam for the first post-compact turn.
	m.app.tier.ID = "-resumed"
	planned, ok := m.launchTurn("continue visibly", nil)().(turnPlanMsg)
	if !ok || planned.err != nil {
		t.Fatalf("post-compact plan = %#v", planned)
	}
	m.finishPlanning()
	if planned.opening.ContinuityRef != "" || planned.opening.AuthoredText() != "continue visibly" || continuityBlockCount(planned.opening) != 0 {
		t.Fatalf("post-compact opening = %#v", planned.opening)
	}
	loopValidated, _, err := m.app.loop.Session.StampContinuityOpening(planned.opening)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.app.loop.Session.AppendMessage(loopValidated); err != nil {
		t.Fatal(err)
	}
	requestMessages := append(provider.CloneMessages(freshState.Messages), loopValidated)
	capsulesOnWire := 0
	for _, message := range requestMessages {
		capsulesOnWire += continuityBlockCount(message)
	}
	if capsulesOnWire != 1 {
		t.Fatalf("first post-compact provider request would carry %d capsules, want one", capsulesOnWire)
	}
	next, included, err := m.app.loop.Session.StampContinuityOpening(provider.UserText("next turn"))
	if err != nil || included || next.ContinuityRef != "" || continuityBlockCount(next) != 0 {
		t.Fatalf("compacted capsule redelivered: included=%v next=%#v err=%v", included, next, err)
	}
}

// Queued prompts are the continuation when they exist: they were typed after
// the work the summary describes, so they run first and the synthetic
// "continue" never fires. Only an empty queue gets it.
func TestCompactContinueYieldsToQueuedPrompts(t *testing.T) {
	m := testModel(t)
	appendTurn(t, m, "do the work", "done")
	swap := compactWithSummary(t, m, "A summary of the work so far.")
	m.queue = []string{"typed while the summary ran"}

	cmd := m.onSessionSwap(swap)
	if cmd == nil {
		t.Fatal("the queued prompt did not start after the swap")
	}
	defer func() { _ = m.app.loop.Session.Close() }()
	if len(m.queue) != 0 {
		t.Fatalf("the queue survived its own drain: %v", m.queue)
	}
	flat := strings.Join(m.tr.flat, "\n")
	if !strings.Contains(flat, "typed while the summary ran") {
		t.Errorf("the queued prompt never rendered:\n%s", flat)
	}
	if strings.Contains(flat, compactContinuePrompt) {
		t.Errorf("the synthetic continuation fired over a queued prompt:\n%s", flat)
	}
	m.finishPlanning()
}

func TestCompactRespectsContinuityTombstone(t *testing.T) {
	m := testModel(t)
	appendTurn(t, m, "clear carried state", "done")
	appendSurfaceContinuity(t, m.app.loop.Session, "must not return")
	if _, err := m.app.loop.Session.ClearContinuity(continuity.SourceManual); err != nil {
		t.Fatal(err)
	}
	if err := m.app.loop.Tools.RestoreTodos([]tools.TodoItem{{Text: "stale live todo", Status: tools.TodoActive}}); err != nil {
		t.Fatal(err)
	}
	swap := compactWithSummary(t, m, "A summary still seeds the fresh transcript.")
	if got := swap.sess.State().Continuity; got != nil {
		swap.sess.Close()
		t.Fatalf("tombstoned source produced a new capsule: %+v", got)
	}
	// Same continuation contract as any compact swap: the launcher comes
	// back, is not run (no live provider), and planning is closed.
	if cmd := m.onSessionSwap(swap); cmd == nil {
		t.Fatal("a compacted session did not continue on its own")
	}
	m.finishPlanning()
	t.Cleanup(func() { _ = m.app.loop.Session.Close() })
	if todos := m.app.loop.Tools.Todos(); len(todos) != 0 {
		t.Fatalf("tombstone did not clear live todos: %+v", todos)
	}
	opening, err := stampTurnOpening(m.app.loop.Session, provider.UserText("continue without capsule"))
	if err != nil || opening.ContinuityRef != "" || continuityBlockCount(opening) != 0 {
		t.Fatalf("tombstoned compact stamped an opening: %#v err=%v", opening, err)
	}
}

func TestCompactContinuityDoesNotCreateAHeaderOnlyCapsule(t *testing.T) {
	for name, state := range map[string]session.State{
		"none":           {ID: "source"},
		"empty":          {ID: "source", Continuity: &continuity.Capsule{Source: continuity.SourceManual}},
		"narrative only": {ID: "source", Continuity: &continuity.Capsule{Source: continuity.SourceManual, Narrative: "old prose summary", Omitted: []string{"narrative"}}},
		"tombstone":      {ID: "source", Continuity: &continuity.Capsule{Source: continuity.SourceManual, Cleared: true}},
	} {
		if capsule, ok := compactContinuity(state); ok {
			t.Errorf("%s source produced header-only capsule %+v", name, capsule)
		}
	}
}

func TestFailedCompactLeavesConversationAndContinuityInSource(t *testing.T) {
	m := testModel(t)
	appendTurn(t, m, "keep this source", "intact")
	stored := appendSurfaceContinuity(t, m.app.loop.Session, "survive compact failure")
	source := m.app.loop.Session
	before := source.State()
	binding := m.app.loop.Binding()
	binding.Provider = &racedProvider{}
	m.app.loop.Bind(binding)
	m.app.budget = &budgetState{}

	completion, ok := compactCmd(m, "", false)().(noticeMsg)
	if !ok || completion.level != "error" || !strings.Contains(completion.text, "session unchanged") {
		t.Fatalf("failed compact result = %#v", completion)
	}
	after := source.State()
	if m.app.loop.Session != source || !reflect.DeepEqual(after.Messages, before.Messages) || after.Continuity == nil || after.Continuity.ID != stored.ID {
		t.Fatalf("failed compact changed source conversation/continuity: before=%+v after=%+v", before, after)
	}
	m.Update(completion)
	if m.operationActive || m.busy {
		t.Fatal("failed compact did not release ownership")
	}
}
