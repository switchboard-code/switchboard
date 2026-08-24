package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/continuity"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/provider/anthropic"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// scriptTurn is one canned model call.
type scriptTurn struct {
	startErr error
	events   []provider.Event
	endErr   error // returned in place of io.EOF once events run out
}

type scriptedProvider struct {
	turns        []scriptTurn
	calls        int
	requests     []provider.Request
	beforeStream func(int)
}

func (p *scriptedProvider) Name() string { return "scripted" }

func (p *scriptedProvider) Stream(_ context.Context, _ provider.RouteTarget, req provider.Request) (provider.EventStream, error) {
	p.requests = append(p.requests, req)
	if p.beforeStream != nil {
		p.beforeStream(p.calls)
	}
	if p.calls >= len(p.turns) {
		return nil, errors.New("scripted provider ran out of turns")
	}
	turn := p.turns[p.calls]
	p.calls++
	if turn.startErr != nil {
		return nil, turn.startErr
	}
	return &scriptedStream{turn: turn}, nil
}

func (p *scriptedProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}

func (p *scriptedProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true}, nil
}

type scriptedStream struct {
	turn scriptTurn
	i    int
}

func (s *scriptedStream) Next() (provider.Event, error) {
	if s.i < len(s.turn.events) {
		ev := s.turn.events[s.i]
		s.i++
		return ev, nil
	}
	if s.turn.endErr != nil {
		return provider.Event{}, s.turn.endErr
	}
	return provider.Event{}, io.EOF
}

func (s *scriptedStream) Close() error { return nil }

func textTurn(text string) scriptTurn {
	return scriptTurn{events: []provider.Event{
		{Type: provider.EventTextDelta, Index: 0, Text: text},
		{Type: provider.EventDone, StopReason: provider.StopEndTurn, Usage: provider.Usage{InputTokens: 10, OutputTokens: 5}},
	}}
}

func toolTurn(uses ...provider.ToolUse) scriptTurn {
	turn := scriptTurn{}
	for i, use := range uses {
		turn.events = append(turn.events, provider.Event{
			Type: provider.EventToolUse, Index: i, ToolUse: &use,
		})
	}
	turn.events = append(turn.events, provider.Event{
		Type: provider.EventDone, StopReason: provider.StopToolUse,
		Usage: provider.Usage{InputTokens: 20, OutputTokens: 8},
	})
	return turn
}

func use(id, name string, input string) provider.ToolUse {
	return provider.ToolUse{ID: id, Name: name, Input: json.RawMessage(input)}
}

func runRegistryTool(t *testing.T, registry *tools.Registry, name, input string) tools.Result {
	t.Helper()
	tool, ok := registry.Get(name)
	if !ok {
		t.Fatalf("tool %q is not registered", name)
	}
	plan, err := tool.Plan(json.RawMessage(input))
	if err != nil {
		t.Fatalf("plan %s: %v", name, err)
	}
	result, err := plan.Run(context.Background())
	if err != nil {
		t.Fatalf("run %s: %v", name, err)
	}
	return result
}

func TestMessageLabelExcludesContinuityWireBlock(t *testing.T) {
	message := provider.Message{
		Role:          provider.RoleUser,
		ContinuityRef: strings.Repeat("a", 32),
		Content: []provider.Block{
			provider.Text{Text: "[continuity]\n\n"},
			provider.Text{Text: "fix the retry path"},
		},
	}
	if got := messageLabel(message); got != "fix the retry path" {
		t.Fatalf("checkpoint label = %q", got)
	}
}

func TestTurnDefensivelyStampsPendingContinuity(t *testing.T) {
	h := newHarness(t, permission.ModeDefault, textTurn("done"))
	stored, err := h.sess.AppendContinuity(continuity.Capsule{
		Source: continuity.SourceManual,
		Tasks:  []continuity.Task{{Text: "deliver from core", Status: continuity.TaskActive}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.loop.Turn(context.Background(), "visible prompt"); err != nil {
		t.Fatal(err)
	}
	state := h.sess.State()
	if len(state.Messages) < 1 || state.Messages[0].ContinuityRef != stored.ID || state.Messages[0].AuthoredText() != "visible prompt" {
		t.Fatalf("durable opening was not defensively stamped: %+v", state.Messages)
	}
	if len(h.provider.requests) != 1 || len(h.provider.requests[0].Messages) < 1 ||
		h.provider.requests[0].Messages[0].ContinuityRef != stored.ID ||
		!strings.Contains(h.provider.requests[0].Messages[0].Text(), "[continuity ") {
		t.Fatalf("provider request missed continuity: %+v", h.provider.requests)
	}
}

type recordingObserver struct {
	text     strings.Builder
	thinking strings.Builder
	notices  []string
	toolEnds []string
	results  []tools.Result
	batches  int
	usages   int
	receipts []session.Usage

	// The loop runs a turn's tool calls in parallel, so an observer that
	// appends without guarding races. This one did, which is the same mistake
	// the production detector made and the reason -race is worth running.
	mu sync.Mutex
}

type closeSessionOnUsageObserver struct {
	*recordingObserver
	sess *session.Session
	once sync.Once
}

type closeSessionOnToolEndObserver struct {
	*recordingObserver
	sess *session.Session
	once sync.Once
}

type cancelOnToolEndObserver struct {
	*recordingObserver
	cancel context.CancelFunc
	once   sync.Once
}

func (o *cancelOnToolEndObserver) ToolEnd(call provider.ToolUse, request permission.Request, result tools.Result, elapsed time.Duration) {
	o.recordingObserver.ToolEnd(call, request, result, elapsed)
	o.once.Do(o.cancel)
}

func (o *closeSessionOnToolEndObserver) ToolEnd(call provider.ToolUse, request permission.Request, result tools.Result, elapsed time.Duration) {
	o.recordingObserver.ToolEnd(call, request, result, elapsed)
	o.once.Do(func() { _ = o.sess.Close() })
}

func (o *closeSessionOnUsageObserver) TurnUsage(u session.Usage) {
	o.recordingObserver.TurnUsage(u)
	o.once.Do(func() { _ = o.sess.Close() })
}

func (o *recordingObserver) ThinkingDelta(s string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.thinking.WriteString(s)
}

func (o *recordingObserver) TextDelta(s string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.text.WriteString(s)
}

func (o *recordingObserver) ToolStart(provider.ToolUse, permission.Request) {}

func (o *recordingObserver) ToolEnd(call provider.ToolUse, _ permission.Request, res tools.Result, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.toolEnds = append(o.toolEnds, call.Name)
	o.results = append(o.results, res)
}

func (o *recordingObserver) ToolBatchEnd(context.Context) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.batches++
}

func (o *recordingObserver) Notice(level, text string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.notices = append(o.notices, level+": "+text)
}
func (o *recordingObserver) TurnUsage(usage session.Usage) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.usages++
	o.receipts = append(o.receipts, usage)
}

type autoAsker struct {
	approve bool
	calls   int
}

func (a *autoAsker) Ask(context.Context, permission.Request, permission.Outcome) (permission.Response, error) {
	a.calls++
	return permission.Response{Approved: a.approve}, nil
}

type harness struct {
	loop     *Loop
	provider *scriptedProvider
	obs      *recordingObserver
	asker    *autoAsker
	sess     *session.Session
	root     string
}

func newHarness(t *testing.T, mode permission.Mode, turns ...scriptTurn) *harness {
	t.Helper()

	root := t.TempDir()
	registry, err := tools.NewRegistry(root, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(root, "scripted/local/test", "test-revision")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })

	p := &scriptedProvider{turns: turns}
	obs := &recordingObserver{}
	asker := &autoAsker{approve: true}

	return &harness{
		loop: &Loop{
			Provider: p,
			Target:   provider.RouteTarget{Provider: "scripted", Surface: "local", ModelID: "test"},
			Tools:    registry,
			Perms:    permission.NewEngine(mode, execution.Capability{}),
			Asker:    asker,
			Session:  sess,
			Observer: obs,
		},
		provider: p,
		obs:      obs,
		asker:    asker,
		sess:     sess,
		root:     registry.Root(),
	}
}

func (h *harness) messages() []provider.Message { return h.sess.State().Messages }

func TestTurnWithoutTools(t *testing.T) {
	h := newHarness(t, permission.ModeDefault, textTurn("all done"))

	if err := h.loop.Turn(context.Background(), "say something"); err != nil {
		t.Fatal(err)
	}

	msgs := h.messages()
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want user + assistant", len(msgs))
	}
	if msgs[0].Role != provider.RoleUser || msgs[1].Role != provider.RoleAssistant {
		t.Errorf("roles = %s, %s", msgs[0].Role, msgs[1].Role)
	}
	if msgs[1].Text() != "all done" {
		t.Errorf("assistant text = %q", msgs[1].Text())
	}
	if h.obs.text.String() != "all done" {
		t.Errorf("observer saw %q", h.obs.text.String())
	}
	if got := h.sess.State().Usage; got.InputTokens != 10 || got.OutputTokens != 5 {
		t.Errorf("usage = %+v", got)
	}
	if len(h.obs.receipts) != 1 || h.obs.receipts[0].CallID == "" || h.obs.receipts[0].Purpose != session.UsagePurposeTurn {
		t.Fatalf("observer receipt = %#v, want stored durable turn CallID", h.obs.receipts)
	}
	durable, err := session.ReadUsages(h.sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(durable) != 1 || durable[0].CallID != h.obs.receipts[0].CallID {
		t.Fatalf("durable usage %#v does not resolve observer receipt %#v", durable, h.obs.receipts)
	}
}

func TestSuccessfulTodoBatchPersistsOneCapsuleAfterItsResult(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		toolTurn(
			use("todo-1", "todo", `{"items":[{"text":"first version","status":"active"}]}`),
			use("todo-2", "todo", `{"items":[{"text":"finished setup","status":"done"},{"text":"verify restart","status":"active"}]}`),
		),
		textTurn("done"),
	)
	if err := h.loop.Turn(context.Background(), "work through the plan"); err != nil {
		t.Fatal(err)
	}
	state := h.sess.State()
	if state.Continuity == nil {
		t.Fatal("successful todo batch did not persist continuity")
	}
	if state.Continuity.Source != continuity.SourceTodo || state.Continuity.BasisMessages != 3 {
		t.Fatalf("capsule was not ordered immediately after the durable result: %+v", state.Continuity)
	}
	if len(state.Continuity.Tasks) != 2 || state.Continuity.Tasks[1].Text != "verify restart" ||
		state.Continuity.Tasks[1].Status != continuity.TaskActive || state.Continuity.NextAction != "verify restart" {
		t.Fatalf("capsule did not capture the final batch state: %+v", state.Continuity)
	}
	raw, err := os.ReadFile(h.sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(raw), `"type":"message_continuity"`); got != 1 {
		t.Fatalf("todo batch wrote %d atomic result-continuity records, want one", got)
	}
	if strings.Contains(string(raw), `"type":"continuity"`) {
		t.Fatal("todo batch used a separate continuity frame and reopened a crash gap")
	}
}

func TestFailedTodoDoesNotPersistContinuity(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		toolTurn(use("todo-bad", "todo", `{"items":[{"text":"cannot persist","status":"blocked"}]}`)),
		textTurn("recovered"),
	)
	if err := h.loop.Turn(context.Background(), "try a bad plan"); err != nil {
		t.Fatal(err)
	}
	if got := h.sess.State().Continuity; got != nil {
		t.Fatalf("failed todo persisted continuity: %+v", got)
	}
	raw, err := os.ReadFile(h.sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"type":"continuity"`) || strings.Contains(string(raw), `"type":"message_continuity"`) {
		t.Fatal("failed todo wrote continuity state")
	}
}

func TestUnstorableTodoIsAToolErrorNotAPersistenceFailure(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		toolTurn(use("todo-control", "todo", `{"items":[{"text":"ok\u0000still","status":"active"}]}`)),
		textTurn("recovered"),
	)
	if err := h.loop.Turn(context.Background(), "try an invalid task"); err != nil {
		t.Fatalf("turn failed after an invalid todo instead of returning a tool error: %v", err)
	}
	state := h.sess.State()
	if state.Continuity != nil || len(h.loop.Tools.Todos()) != 0 {
		t.Fatalf("invalid todo changed live or durable state: continuity=%+v todos=%+v", state.Continuity, h.loop.Tools.Todos())
	}
	if len(state.Messages) < 3 || state.Messages[2].Role != provider.RoleTool {
		t.Fatalf("missing durable tool error: %+v", state.Messages)
	}
	result, ok := state.Messages[2].Content[0].(provider.ToolResult)
	if !ok || !result.IsError || !strings.Contains(result.Content, "control character") {
		t.Fatalf("todo result = %#v", state.Messages[2].Content[0])
	}
}

func TestSuccessfulTodoPersistsAtomicallyWhenBatchEndsCancelled(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		toolTurn(use("todo-cancel", "todo", `{"items":[{"text":"survive cancellation","status":"active"}]}`)),
	)
	ctx, cancel := context.WithCancel(context.Background())
	h.loop.SetObserver(&cancelOnToolEndObserver{recordingObserver: h.obs, cancel: cancel})
	err := h.loop.Turn(ctx, "plan then cancel")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("turn error = %v, want cancellation", err)
	}
	state := h.sess.State()
	if len(state.Messages) != 3 || state.Continuity == nil || len(state.Continuity.Tasks) != 1 ||
		state.Continuity.Tasks[0].Text != "survive cancellation" {
		t.Fatalf("successful todo was lost at cancelled batch boundary: %+v", state)
	}
	raw, readErr := os.ReadFile(h.sess.Path())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Count(string(raw), `"type":"message_continuity"`) != 1 {
		t.Fatal("cancelled batch did not use the atomic todo frame")
	}
	id, path := h.sess.ID(), h.sess.Path()
	if err := h.sess.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Dir(filepath.Dir(path)))
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	registry, err := tools.NewRegistry(h.root, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	loop := &Loop{Tools: registry}
	if err := loop.BindSession(reopened); err != nil {
		t.Fatal(err)
	}
	if got := registry.Todos(); len(got) != 1 || got[0].Text != "survive cancellation" {
		t.Fatalf("restart hydration after cancellation = %+v", got)
	}
}

func TestTodoResultAndCapsuleBothFailWhenAtomicAppendFails(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		toolTurn(use("todo-1", "todo", `{"items":[{"text":"not durable","status":"active"}]}`)),
	)
	observer := &closeSessionOnToolEndObserver{recordingObserver: h.obs, sess: h.sess}
	h.loop.SetObserver(observer)
	err := h.loop.Turn(context.Background(), "close before result append")
	if !errors.Is(err, session.ErrSessionPoisoned) {
		t.Fatalf("turn error = %v, want poisoned result append", err)
	}
	if got := h.sess.State(); got.Continuity != nil || len(got.Messages) != 2 {
		t.Fatalf("failed atomic append published one half: %+v", got)
	}
	raw, readErr := os.ReadFile(h.sess.Path())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(raw), `"type":"message_continuity"`) || strings.Contains(string(raw), `"type":"continuity"`) {
		t.Fatal("failed atomic append reached the WAL")
	}
}

func TestBindSessionHydratesRestartAndClearsOtherSessions(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		toolTurn(use("todo-1", "todo", `{"items":[{"text":"survive restart","status":"active"}]}`)),
		textTurn("done"),
	)
	if err := h.loop.Turn(context.Background(), "persist a plan"); err != nil {
		t.Fatal(err)
	}
	id, path := h.sess.ID(), h.sess.Path()
	if err := h.sess.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Dir(filepath.Dir(path)))
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	fresh, err := tools.NewRegistry(h.root, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	loop := &Loop{Tools: fresh}
	if err := loop.BindSession(reopened); err != nil {
		t.Fatal(err)
	}
	if got := fresh.Todos(); len(got) != 1 || got[0].Text != "survive restart" || got[0].Status != tools.TodoActive {
		t.Fatalf("restart hydration = %+v", got)
	}
	if err := loop.BindSession(nil); err == nil {
		t.Fatal("nil session binding was accepted")
	}
	if loop.Session != reopened || len(fresh.Todos()) != 1 {
		t.Fatal("failed binding partially changed session or todos")
	}

	blank, err := store.Create(h.root, "scripted/local/blank", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer blank.Close()
	if err := loop.BindSession(blank); err != nil {
		t.Fatal(err)
	}
	if got := fresh.Todos(); len(got) != 0 {
		t.Fatalf("binding a session without continuity kept stale todos: %+v", got)
	}

	if err := fresh.RestoreTodos([]tools.TodoItem{{Text: "stale again", Status: tools.TodoActive}}); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.ClearContinuity(continuity.SourceManual); err != nil {
		t.Fatal(err)
	}
	if err := loop.BindSession(reopened); err != nil {
		t.Fatal(err)
	}
	if got := fresh.Todos(); len(got) != 0 {
		t.Fatalf("binding a tombstoned session kept stale todos: %+v", got)
	}
}

func TestBindSessionSuppressesCapsuleStaleAfterUserCancellation(t *testing.T) {
	h := newHarness(t, permission.ModeDefault)
	stored, err := h.sess.AppendContinuity(continuity.Capsule{
		Source:        continuity.SourceManual,
		Objective:     "publish the release",
		NextAction:    "cut the release",
		StopCondition: "release is public",
		Tasks: []continuity.Task{{
			Text: "cut the release", Status: continuity.TaskActive,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cancel, included, err := h.sess.StampContinuityOpening(provider.UserText("cancel that release; do not publish"))
	if err != nil || !included {
		t.Fatalf("stamp cancellation: included=%v err=%v", included, err)
	}
	if err := h.sess.AppendMessage(cancel); err != nil {
		t.Fatal(err)
	}
	if got := h.sess.CurrentContinuity(); got == nil || got.ID != stored.ID {
		t.Fatalf("fixture lost stale capsule evidence: %+v", got)
	}

	registry, err := tools.NewRegistry(h.root, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	loop := &Loop{Tools: registry}
	if err := loop.BindSession(h.sess); err != nil {
		t.Fatal(err)
	}
	if got := registry.Todos(); len(got) != 0 {
		t.Fatalf("generic resume re-promoted cancelled tasks: %+v", got)
	}
	if got := registry.Working(); got != (continuity.Working{}) {
		t.Fatalf("generic resume re-promoted cancelled working context: %+v", got)
	}

	// A capsule recorded after the cancellation has seen the new authority and
	// remains legitimate resumable state.
	updated, err := h.sess.AppendContinuity(continuity.Capsule{
		Source:    continuity.SourceManual,
		Objective: "prepare release notes only",
		Tasks: []continuity.Task{{
			Text: "draft release notes", Status: continuity.TaskActive,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.BindSession(h.sess); err != nil {
		t.Fatal(err)
	}
	if got := registry.Todos(); len(got) != 1 || got[0].Text != "draft release notes" {
		t.Fatalf("post-cancellation capsule did not hydrate: %+v", got)
	}
	if got := h.sess.ResumableContinuity(); got == nil || got.ID != updated.ID {
		t.Fatalf("post-cancellation capsule is not resumable: %+v", got)
	}
}

func TestBindSessionReplacesWorkingContextAcrossSessions(t *testing.T) {
	h := newHarness(t, permission.ModeDefault)
	store, err := session.NewStore(filepath.Dir(filepath.Dir(h.sess.Path())))
	if err != nil {
		t.Fatal(err)
	}

	workingA := continuity.Working{
		Objective:     "finish session A",
		NextAction:    "session A task",
		StopCondition: "session A tests pass",
	}
	if _, err := h.sess.AppendContinuity(continuity.WithWorking(nil, []continuity.Task{{
		Text: "session A task", Status: continuity.TaskActive,
	}}, workingA)); err != nil {
		t.Fatal(err)
	}
	if err := h.loop.BindSession(h.sess); err != nil {
		t.Fatal(err)
	}
	if got := h.loop.Tools.Working(); got != workingA {
		t.Fatalf("session A working context = %+v, want %+v", got, workingA)
	}

	sessionB, err := store.Create(h.root, "scripted/local/session-b-working", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sessionB.Close()
	workingB := continuity.Working{
		Objective:     "finish session B",
		NextAction:    "session B task",
		StopCondition: "session B tests pass",
	}
	if _, err := sessionB.AppendContinuity(continuity.WithWorking(nil, []continuity.Task{{
		Text: "session B task", Status: continuity.TaskActive,
	}}, workingB)); err != nil {
		t.Fatal(err)
	}
	if err := h.loop.BindSession(sessionB); err != nil {
		t.Fatal(err)
	}
	if got := h.loop.Tools.Working(); got != workingB {
		t.Fatalf("session B inherited session A working context: got %+v, want %+v", got, workingB)
	}

	runRegistryTool(t, h.loop.Tools, "todo", `{"items":[{"text":"updated session B task","status":"active"}]}`)
	if got := h.loop.Tools.Working(); got != (continuity.Working{
		Objective:     workingB.Objective,
		StopCondition: workingB.StopCondition,
	}) {
		t.Fatalf("session B todo omission resurrected session A context: %+v", got)
	}

	fresh, err := store.Create(h.root, "scripted/local/fresh-working", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err := h.loop.BindSession(fresh); err != nil {
		t.Fatal(err)
	}
	if got := h.loop.Tools.Working(); got != (continuity.Working{}) {
		t.Fatalf("fresh session inherited prior working context: %+v", got)
	}
	runRegistryTool(t, h.loop.Tools, "todo", `{"items":[{"text":"fresh task","status":"active"}]}`)
	if got := h.loop.Tools.Working(); got != (continuity.Working{}) {
		t.Fatalf("fresh-session todo omission resurrected prior context: %+v", got)
	}

	// A tombstone is another explicit empty state, not permission to keep the
	// registry contents that happened to precede the bind.
	if _, err := sessionB.ClearContinuity(continuity.SourceManual); err != nil {
		t.Fatal(err)
	}
	if err := h.loop.Tools.RestoreContinuity(
		[]tools.TodoItem{{Text: "stale", Status: tools.TodoActive}}, workingA,
	); err != nil {
		t.Fatal(err)
	}
	if err := h.loop.BindSession(sessionB); err != nil {
		t.Fatal(err)
	}
	if got := h.loop.Tools.Todos(); len(got) != 0 {
		t.Fatalf("tombstoned session retained prior todos: %+v", got)
	}
	if got := h.loop.Tools.Working(); got != (continuity.Working{}) {
		t.Fatalf("tombstoned session retained prior working context: %+v", got)
	}
}

func TestBindSessionAcceptsSameProcessCapsuleAfterPayloadFitting(t *testing.T) {
	h := newHarness(t, permission.ModeDefault)
	large := continuity.Capsule{
		Source:    continuity.SourceManual,
		Narrative: strings.Repeat("context ", 2_000),
		Tasks:     make([]continuity.Task, continuity.MaxTasks),
		Files:     make([]continuity.File, continuity.MaxFiles),
		Facts:     make([]string, continuity.MaxFacts),
	}
	for i := range large.Tasks {
		large.Tasks[i] = continuity.Task{Text: strings.Repeat("task ", 100), Status: continuity.TaskDone}
	}
	for i := range large.Files {
		large.Files[i] = continuity.File{Path: strings.Repeat("path/", 120), State: "unverified"}
	}
	for i := range large.Facts {
		large.Facts[i] = strings.Repeat("fact ", 100)
	}
	stored, err := h.sess.AppendContinuity(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := continuity.ValidateStored(stored); err != nil {
		t.Fatalf("same-process stored capsule is not canonical: %v", err)
	}
	fresh, err := tools.NewRegistry(h.root, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	loop := &Loop{Tools: fresh}
	if err := loop.BindSession(h.sess); err != nil {
		t.Fatalf("same-process binding rejected fitted capsule: %v", err)
	}
	if len(fresh.Todos()) != len(stored.Tasks) {
		t.Fatalf("hydrated %d tasks, stored %d", len(fresh.Todos()), len(stored.Tasks))
	}
}

func TestBindSessionDropsPriorSessionReadAuthorityUntilReread(t *testing.T) {
	h := newHarness(t, permission.ModeDefault)
	recorder := checkpoint.NewRecorder()
	if err := recorder.ConfigureRestoreCleanup(filepath.Dir(h.sess.Path()), h.root); err != nil {
		t.Fatal(err)
	}
	recorder.Begin("session-bound read authority")
	h.loop.Tools.SetCheckpoints(recorder)
	path := filepath.Join(h.root, "session-bound.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if result := runRegistryTool(t, h.loop.Tools, "read", `{"path":"session-bound.txt"}`); result.IsError {
		t.Fatalf("session A read failed: %s", result.Content)
	}

	store, err := session.NewStore(filepath.Dir(filepath.Dir(h.sess.Path())))
	if err != nil {
		t.Fatal(err)
	}
	sessionB, err := store.Create(h.root, "scripted/local/session-b", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sessionB.Close()
	if err := h.loop.BindSession(sessionB); err != nil {
		t.Fatal(err)
	}

	write := runRegistryTool(t, h.loop.Tools, "write", `{"path":"session-bound.txt","content":"written\n"}`)
	if !write.IsError || !strings.Contains(write.Content, "not been read") {
		t.Fatalf("session B inherited A's write authority: %+v", write)
	}
	edit := runRegistryTool(t, h.loop.Tools, "edit", `{"path":"session-bound.txt","old_string":"before","new_string":"after"}`)
	if !edit.IsError || !strings.Contains(edit.Content, "not been read") {
		t.Fatalf("session B inherited A's edit authority: %+v", edit)
	}
	if result := runRegistryTool(t, h.loop.Tools, "read", `{"path":"session-bound.txt"}`); result.IsError {
		t.Fatalf("session B reread failed: %s", result.Content)
	}
	if result := runRegistryTool(t, h.loop.Tools, "edit", `{"path":"session-bound.txt","old_string":"before","new_string":"after"}`); result.IsError {
		t.Fatalf("edit after session B reread failed: %s", result.Content)
	}
}

func TestBindSessionWaitsForActiveTurnAndPublishesContextTogether(t *testing.T) {
	h := newHarness(t, permission.ModeDefault, textTurn("done"))
	gate := &gatedProvider{started: make(chan struct{}), release: make(chan struct{})}
	h.loop.Provider = gate
	store, err := session.NewStore(filepath.Dir(filepath.Dir(h.sess.Path())))
	if err != nil {
		t.Fatal(err)
	}
	next, err := store.Create(h.root, "scripted/local/next", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()

	turnDone := make(chan error, 1)
	go func() { turnDone <- h.loop.Turn(context.Background(), "finish on the old session") }()
	<-gate.started
	bindDone := make(chan error, 1)
	go func() { bindDone <- h.loop.BindSession(next) }()
	select {
	case err := <-bindDone:
		t.Fatalf("bind crossed an active turn: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(gate.release)
	if err := <-turnDone; err != nil {
		t.Fatal(err)
	}
	if err := <-bindDone; err != nil {
		t.Fatal(err)
	}
	if h.loop.Session != next || len(next.State().Messages) != 0 || len(h.sess.State().Messages) != 2 {
		t.Fatalf("session swap mixed turn state: old=%d new=%d bound=%p want=%p",
			len(h.sess.State().Messages), len(next.State().Messages), h.loop.Session, next)
	}
}

func TestToolRoundTrip(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		toolTurn(use("call_1", "read", `{"path":"hello.txt"}`)),
		textTurn("the file says hi"),
	)
	if err := os.WriteFile(filepath.Join(h.root, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := h.loop.Turn(context.Background(), "read hello.txt"); err != nil {
		t.Fatal(err)
	}

	msgs := h.messages()
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want user, assistant, tool, assistant", len(msgs))
	}
	if msgs[2].Role != provider.RoleTool {
		t.Fatalf("message 2 role = %s, want tool", msgs[2].Role)
	}
	result, ok := msgs[2].Content[0].(provider.ToolResult)
	if !ok {
		t.Fatalf("tool message holds a %s block", msgs[2].Content[0].Kind())
	}
	if result.ToolUseID != "call_1" || result.Content != "hi" || result.IsError {
		t.Errorf("result = %+v", result)
	}
	if h.obs.batches != 1 {
		t.Fatalf("tool batch callbacks = %d, want exactly one after results were committed", h.obs.batches)
	}

	// The second request must carry the whole conversation so far.
	if len(h.provider.requests) != 2 {
		t.Fatalf("provider called %d times, want 2", len(h.provider.requests))
	}
	if n := len(h.provider.requests[1].Messages); n != 3 {
		t.Errorf("second request carried %d messages, want 3", n)
	}
}

func TestToolDoesNotRunWhenPermissionDecisionCannotBeJournaled(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		toolTurn(use("call_1", "write", `{"path":"must-not-exist.txt","content":"side effect"}`)),
	)
	observer := &closeSessionOnUsageObserver{recordingObserver: h.obs, sess: h.sess}
	h.loop.SetObserver(observer)

	err := h.loop.Turn(context.Background(), "write the file")
	if !errors.Is(err, session.ErrSessionPoisoned) {
		t.Fatalf("Turn err = %v, want ErrSessionPoisoned from the permission journal", err)
	}
	if _, statErr := os.Stat(filepath.Join(h.root, "must-not-exist.txt")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("approved side effect ran after the journal failed: stat err = %v", statErr)
	}
	observer.mu.Lock()
	toolEnds := len(observer.toolEnds)
	observer.mu.Unlock()
	if toolEnds != 0 {
		t.Fatalf("observer saw %d completed tools, want none", toolEnds)
	}
	if h.provider.calls != 1 {
		t.Fatalf("provider calls = %d, want no follow-up after journal failure", h.provider.calls)
	}
}

// Every tool call needs exactly one result, in call order. A conversation where
// they do not line up is malformed, and every later request inherits it.
func TestEveryCallGetsExactlyOneResultInOrder(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		toolTurn(
			use("call_1", "read", `{"path":"a.txt"}`),
			use("call_2", "read", `{"path":"missing.txt"}`),
			use("call_3", "nosuchtool", `{}`),
			use("call_4", "read", `{"path":"b.txt"}`),
		),
		textTurn("done"),
	)
	os.WriteFile(filepath.Join(h.root, "a.txt"), []byte("alpha"), 0o644)
	os.WriteFile(filepath.Join(h.root, "b.txt"), []byte("bravo"), 0o644)

	if err := h.loop.Turn(context.Background(), "read some files"); err != nil {
		t.Fatal(err)
	}

	toolMsg := h.messages()[2]
	if len(toolMsg.Content) != 4 {
		t.Fatalf("got %d results for 4 calls", len(toolMsg.Content))
	}
	for i, want := range []string{"call_1", "call_2", "call_3", "call_4"} {
		got := toolMsg.Content[i].(provider.ToolResult)
		if got.ToolUseID != want {
			t.Errorf("result %d is for %s, want %s", i, got.ToolUseID, want)
		}
	}
	if r := toolMsg.Content[0].(provider.ToolResult); r.Content != "alpha" || r.IsError {
		t.Errorf("call_1 = %+v", r)
	}
	if r := toolMsg.Content[1].(provider.ToolResult); !r.IsError {
		t.Error("a missing file should come back as a tool error")
	}
	if r := toolMsg.Content[2].(provider.ToolResult); !r.IsError || !strings.Contains(r.Content, "nosuchtool") {
		t.Errorf("unknown tool = %+v", r)
	}
}

func TestToolResultCredentialIsRedactedAtProviderAndSessionBoundary(t *testing.T) {
	token := "ghp_" + strings.Repeat("A", 36)
	localResult := strings.Repeat("x", 4_090) + token + "\n"
	h := newHarness(t, permission.ModeDefault,
		toolTurn(use("call_1", "read", `{"path":"credential.txt"}`)),
		textTurn("done"),
	)
	if err := os.WriteFile(filepath.Join(h.root, "credential.txt"), []byte(localResult), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := h.loop.Turn(context.Background(), "read the file"); err != nil {
		t.Fatal(err)
	}

	// Hooks and the local observer consume the exact tool result before the
	// provider/session boundary owns and redacts its copy.
	h.obs.mu.Lock()
	observed := append([]tools.Result(nil), h.obs.results...)
	h.obs.mu.Unlock()
	if len(observed) != 1 || !strings.Contains(observed[0].Content, token) {
		t.Fatalf("local observer result was changed before the egress boundary: %#v", observed)
	}

	if len(h.provider.requests) != 2 {
		t.Fatalf("provider requests = %d, want tool call and follow-up", len(h.provider.requests))
	}
	requestWire, err := json.Marshal(h.provider.requests[1])
	if err != nil {
		t.Fatal(err)
	}
	marker := "[redacted: a GitHub token]"
	if bytes := string(requestWire); strings.Contains(bytes, token) || !strings.Contains(bytes, marker) {
		t.Fatalf("second provider request did not contain only the redaction marker: %s", bytes)
	}

	state := h.sess.State()
	result, ok := state.Messages[2].Content[0].(provider.ToolResult)
	if !ok || strings.Contains(result.Content, token) || !strings.Contains(result.Content, marker) {
		t.Fatalf("durable result did not contain only the redaction marker: %#v", state.Messages[2].Content)
	}
	durable, err := os.ReadFile(h.sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(durable), token) || !strings.Contains(string(durable), marker) {
		t.Fatal("session record retained the credential or omitted the redaction marker")
	}
}

func TestResultBlocksRedactExecutedAndFallbackCredentials(t *testing.T) {
	token := "ghp_" + strings.Repeat("B", 36)
	jobs := []*toolJob{
		{use: use("run", "test", `{}`), result: &tools.Result{Content: "tool result: " + token}},
		{use: use("fallback", "test", `{}`)},
	}
	blocks := resultBlocks(jobs, "fallback error: "+token)
	if len(blocks) != len(jobs) {
		t.Fatalf("result blocks = %d, want %d", len(blocks), len(jobs))
	}
	for i, block := range blocks {
		result := block.(provider.ToolResult)
		if strings.Contains(result.Content, token) || !strings.Contains(result.Content, "[redacted: a GitHub token]") {
			t.Errorf("result %d was not redacted at the common boundary: %q", i, result.Content)
		}
	}
}

func TestResumedLegacyToolResultIsRedactedOnlyInProviderReplay(t *testing.T) {
	userToken := "ghp_" + strings.Repeat("U", 36)
	toolToken := "ghp_" + strings.Repeat("T", 36)
	h := newHarness(t, permission.ModeDefault, textTurn("continued"))
	legacy := []provider.Message{
		provider.UserText("explicitly send as typed: " + userToken),
		{Role: provider.RoleAssistant, Content: []provider.Block{provider.ToolUse{
			ID: "legacy-call", Name: "read", Input: json.RawMessage(`{"path":".env"}`),
		}}},
		{Role: provider.RoleTool, Content: []provider.Block{provider.ToolResult{
			ToolUseID: "legacy-call", Name: "read", Content: "TOKEN=" + toolToken,
		}}},
		{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "legacy turn complete"}}},
	}
	for _, message := range legacy {
		if err := h.sess.AppendMessage(message); err != nil {
			t.Fatal(err)
		}
	}

	id, logPath := h.sess.ID(), h.sess.Path()
	if err := h.sess.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Dir(filepath.Dir(logPath)))
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := store.OpenInWorkspace(id, h.root)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if err := h.loop.BindSession(resumed); err != nil {
		t.Fatal(err)
	}
	h.sess = resumed

	if err := h.loop.Turn(context.Background(), "continue the resumed conversation"); err != nil {
		t.Fatal(err)
	}
	if len(h.provider.requests) != 1 {
		t.Fatalf("provider requests = %d, want one resumed turn", len(h.provider.requests))
	}
	wire, err := json.Marshal(h.provider.requests[0])
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(wire)
	if strings.Contains(rendered, toolToken) || !strings.Contains(rendered, "[redacted: a GitHub token]") {
		t.Fatalf("legacy tool result crossed resumed provider replay: %s", rendered)
	}
	if !strings.Contains(rendered, userToken) {
		t.Fatal("defensive replay redaction narrowed an explicit send-as-typed user message")
	}

	state := resumed.State()
	legacyResult := state.Messages[2].Content[0].(provider.ToolResult)
	if !strings.Contains(legacyResult.Content, toolToken) {
		t.Fatalf("provider replay mutated the durable legacy record: %#v", legacyResult)
	}
	durable, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(durable), toolToken) {
		t.Fatal("resume migration rewrote the append-only legacy log")
	}
}

func TestMalformedToolArgumentsGoBackToTheModel(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		toolTurn(use("call_1", "read", `{"path": 12345}`)),
		textTurn("let me try again"),
	)

	// A bad argument blob is the model's mistake to fix, not a reason to throw
	// away the turn (§10.3).
	if err := h.loop.Turn(context.Background(), "read something"); err != nil {
		t.Fatalf("a malformed argument must not abort the turn: %v", err)
	}
	res := h.messages()[2].Content[0].(provider.ToolResult)
	if !res.IsError {
		t.Error("the model should have been told its arguments were wrong")
	}
	if h.provider.calls != 2 {
		t.Errorf("provider called %d times; the model never got a chance to correct itself", h.provider.calls)
	}
}

func TestDeniedCallIsReportedNotFatal(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		toolTurn(use("call_1", "exec", `{"command":["rm","-rf","/"]}`)),
		textTurn("understood, I will not"),
	)
	h.asker.approve = false

	if err := h.loop.Turn(context.Background(), "clean up"); err != nil {
		t.Fatal(err)
	}
	res := h.messages()[2].Content[0].(provider.ToolResult)
	if !res.IsError || !strings.Contains(res.Content, "not approved") {
		t.Errorf("result = %+v", res)
	}
	if h.asker.calls != 1 {
		t.Errorf("asker called %d times, want 1", h.asker.calls)
	}
}

func TestPlanModeRefusesWithoutPrompting(t *testing.T) {
	h := newHarness(t, permission.ModePlan,
		toolTurn(use("call_1", "write", `{"path":"new.txt","content":"x"}`)),
		textTurn("I cannot write in plan mode"),
	)

	if err := h.loop.Turn(context.Background(), "make a file"); err != nil {
		t.Fatal(err)
	}
	if h.asker.calls != 0 {
		t.Error("plan mode denies outright; it must not prompt")
	}
	if _, err := os.Stat(filepath.Join(h.root, "new.txt")); err == nil {
		t.Error("plan mode wrote a file")
	}
}

func TestRetryOnTransientFailure(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		scriptTurn{startErr: &provider.APIError{Provider: "scripted", StatusCode: 503}},
		textTurn("recovered"),
	)
	h.loop.MaxAttempts = 3

	if err := h.loop.Turn(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if h.provider.calls != 2 {
		t.Errorf("provider called %d times, want a retry", h.provider.calls)
	}
	if len(h.obs.notices) == 0 {
		t.Error("a retry must be visible to the user, not silent")
	}
}

func TestBudgetIsRecheckedBeforeEveryProviderAttempt(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		scriptTurn{startErr: &provider.APIError{Provider: "scripted", StatusCode: 503}},
		textTurn("must not be sent"),
	)
	h.loop.MaxAttempts = 3
	var attempts []int
	var outcomes []error
	h.loop.Budget = func(_ int, attempt int) error {
		attempts = append(attempts, attempt)
		if attempt > 1 {
			return errors.New("retry reservation crosses the ceiling")
		}
		return nil
	}
	h.loop.BudgetResult = func(_ int, _ int, _ session.Usage, err error) error {
		outcomes = append(outcomes, err)
		return nil
	}

	err := h.loop.Turn(context.Background(), "hello")
	if err == nil || !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("err = %v, want the retry budget refusal", err)
	}
	if h.provider.calls != 1 {
		t.Fatalf("provider calls = %d, want the second attempt stopped before send", h.provider.calls)
	}
	if fmt.Sprint(attempts) != "[1 2]" {
		t.Fatalf("budget attempts = %v", attempts)
	}
	if len(outcomes) != 1 || outcomes[0] == nil {
		t.Fatalf("attempt outcomes = %v, want the one issued provider attempt reported as failed", outcomes)
	}
}

func TestSuccessfulBudgetReservationReleasesAfterUsageIsDurable(t *testing.T) {
	h := newHarness(t, permission.ModeDefault, textTurn("done"))
	h.loop.Budget = func(_ int, _ int) error { return nil }
	called := false
	h.loop.BudgetResult = func(_ int, _ int, _ session.Usage, err error) error {
		called = true
		if err != nil {
			t.Fatalf("successful attempt reported %v", err)
		}
		if state := h.sess.State(); state.Calls != 1 {
			t.Fatalf("reservation released before usage was durable: calls=%d", state.Calls)
		}
		return nil
	}
	if err := h.loop.Turn(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("successful attempt did not release its budget reservation")
	}
}

func TestPermanentFailureIsNotRetried(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		scriptTurn{startErr: &provider.APIError{Provider: "scripted", StatusCode: 404, Body: "model not found"}},
	)
	h.loop.MaxAttempts = 3

	err := h.loop.Turn(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected the turn to fail")
	}
	if h.provider.calls != 1 {
		t.Errorf("provider called %d times; a 404 will not fix itself", h.provider.calls)
	}
}

func TestMalformedStreamAbortsWithoutRetrying(t *testing.T) {
	h := newHarness(t, permission.ModeDefault, scriptTurn{
		events: []provider.Event{{Type: provider.EventTextDelta, Text: "partial"}},
		endErr: &provider.ProtocolError{Provider: "scripted", Detail: "unknown block"},
	})
	h.loop.MaxAttempts = 3

	err := h.loop.Turn(context.Background(), "hello")
	var protoErr *provider.ProtocolError
	if !errors.As(err, &protoErr) {
		t.Fatalf("err = %v, want a ProtocolError", err)
	}
	if h.provider.calls != 1 {
		t.Errorf("provider called %d times; re-issuing produces the same malformed shape", h.provider.calls)
	}
}

// A stream that drops leaves real output. It is kept, marked incomplete, and
// never replayed as a finished turn.
func TestExhaustedRetriesRecordAnIncompleteMessage(t *testing.T) {
	dropped := scriptTurn{
		events: []provider.Event{{Type: provider.EventTextDelta, Text: "half a thou"}},
		endErr: provider.ErrStreamIncomplete,
	}
	h := newHarness(t, permission.ModeDefault, dropped, dropped)
	h.loop.MaxAttempts = 2

	err := h.loop.Turn(context.Background(), "hello")
	if !errors.Is(err, provider.ErrStreamIncomplete) {
		t.Fatalf("err = %v", err)
	}
	if !errors.Is(err, ErrProviderCall) {
		t.Fatalf("err = %v, want provider-call provenance", err)
	}
	if h.provider.calls != 2 {
		t.Errorf("provider called %d times, want 2 attempts", h.provider.calls)
	}

	msgs := h.messages()
	last := msgs[len(msgs)-1]
	if !last.Incomplete {
		t.Fatalf("the partial turn was not marked incomplete: %+v", last)
	}
	if last.Text() != "half a thou" {
		t.Errorf("partial content = %q, want what actually arrived", last.Text())
	}
}

func TestResumeWithholdsIncompleteAssistantButKeepsItDurable(t *testing.T) {
	workspace := t.TempDir()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(workspace, "scripted/local/test", "test-revision")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("first request")); err != nil {
		t.Fatal(err)
	}
	const partial = "PARTIAL MUST STAY DURABLE"
	if err := sess.AppendMessage(provider.Message{
		Role:       provider.RoleAssistant,
		Incomplete: true,
		Content:    []provider.Block{provider.Text{Text: partial}},
	}); err != nil {
		t.Fatal(err)
	}
	id := sess.ID()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	resumed, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(workspace, execution.Capability{})
	if err != nil {
		resumed.Close()
		t.Fatal(err)
	}
	scripted := &scriptedProvider{turns: []scriptTurn{textTurn("done")}}
	loop := &Loop{
		Provider: scripted,
		Target: provider.RouteTarget{
			Provider: "scripted",
			Surface:  "local",
			ModelID:  "test",
		},
		Tools:    registry,
		Perms:    permission.NewEngine(permission.ModeDefault, execution.Capability{}),
		Session:  resumed,
		Observer: &recordingObserver{},
	}
	if err := loop.Turn(context.Background(), "continue"); err != nil {
		resumed.Close()
		t.Fatal(err)
	}
	if len(scripted.requests) != 1 {
		resumed.Close()
		t.Fatalf("provider requests = %d, want 1", len(scripted.requests))
	}
	wireMessages := scripted.requests[0].Messages
	if len(wireMessages) != 2 || wireMessages[0].Text() != "first request" || wireMessages[1].Text() != "continue" {
		resumed.Close()
		t.Fatalf("resumed provider request = %+v, want the two user messages", wireMessages)
	}
	for _, message := range wireMessages {
		if message.Incomplete || strings.Contains(message.Text(), partial) {
			resumed.Close()
			t.Fatalf("incomplete assistant reached resumed provider request: %+v", wireMessages)
		}
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}

	// Close and replay again to prove the provider projection did not rewrite
	// the append-only diagnostic record while serving the resumed turn.
	persisted, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer persisted.Close()
	state := persisted.State()
	if len(state.Messages) != 4 {
		t.Fatalf("durable messages = %+v, want original user, partial, resumed user, answer", state.Messages)
	}
	if !state.Messages[1].Incomplete || state.Messages[1].Role != provider.RoleAssistant || state.Messages[1].Text() != partial {
		t.Fatalf("durable partial was lost or rewritten: %+v", state.Messages[1])
	}
	if state.Messages[3].Incomplete || state.Messages[3].Text() != "done" {
		t.Fatalf("resumed completion was not recorded normally: %+v", state.Messages[3])
	}
}

func TestRoundLimit(t *testing.T) {
	var turns []scriptTurn
	for range 5 {
		turns = append(turns, toolTurn(use("call_x", "read", `{"path":"loop.txt"}`)))
	}
	h := newHarness(t, permission.ModeDefault, turns...)
	os.WriteFile(filepath.Join(h.root, "loop.txt"), []byte("x"), 0o644)
	h.loop.MaxToolRounds = 3

	err := h.loop.Turn(context.Background(), "go forever")
	if !errors.Is(err, ErrRoundLimit) {
		t.Fatalf("err = %v, want ErrRoundLimit", err)
	}
	if h.provider.calls != 3 {
		t.Errorf("provider called %d times, want the 3 round limit", h.provider.calls)
	}
}

// With no cap set a turn runs until the model is done. The watcher and the
// budget are the brakes, not a round count.
func TestNoDefaultRoundLimit(t *testing.T) {
	var turns []scriptTurn
	for range 45 {
		turns = append(turns, toolTurn(use("call_x", "read", `{"path":"loop.txt"}`)))
	}
	turns = append(turns, textTurn("done"))
	h := newHarness(t, permission.ModeDefault, turns...)
	os.WriteFile(filepath.Join(h.root, "loop.txt"), []byte("x"), 0o644)

	if err := h.loop.Turn(context.Background(), "keep going"); err != nil {
		t.Fatal(err)
	}
	if h.provider.calls != 46 {
		t.Errorf("provider called %d times, want 45 tool rounds plus the closing call", h.provider.calls)
	}
}

type cancellationBlockingTool struct {
	started chan struct{}
	once    sync.Once
}

func (*cancellationBlockingTool) Name() string        { return "cancel-block" }
func (*cancellationBlockingTool) Description() string { return "wait for cancellation" }
func (*cancellationBlockingTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false}`)
}
func (*cancellationBlockingTool) ParallelSafe() bool { return false }
func (t *cancellationBlockingTool) Plan(json.RawMessage) (tools.Plan, error) {
	return tools.Plan{
		Request: permission.Request{Tool: "cancel-block", Effect: permission.EffectRead},
		Run: func(ctx context.Context) (tools.Result, error) {
			t.once.Do(func() { close(t.started) })
			<-ctx.Done()
			return tools.Result{}, ctx.Err()
		},
	}, nil
}

func TestCancellationStillPairsResultsWithCalls(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		toolTurn(
			use("call_1", "cancel-block", `{}`),
			use("call_2", "cancel-block", `{}`),
		),
	)
	blocking := &cancellationBlockingTool{started: make(chan struct{})}
	if err := h.loop.Tools.AddExternal(blocking); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- h.loop.Turn(ctx, "run things")
	}()
	select {
	case <-blocking.started:
		cancel()
	case <-time.After(5 * time.Second):
		t.Fatal("blocking tool did not start")
	}
	var err error
	select {
	case err = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled tool batch did not settle")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	msgs := h.messages()
	toolMsg := msgs[len(msgs)-1]
	if toolMsg.Role != provider.RoleTool {
		t.Fatalf("last message role = %s, want the tool results", toolMsg.Role)
	}
	if len(toolMsg.Content) != 2 {
		t.Fatalf("got %d results for 2 calls; a cancelled turn must still pair them", len(toolMsg.Content))
	}
	second := toolMsg.Content[1].(provider.ToolResult)
	if !second.IsError || !strings.Contains(second.Content, "cancelled") {
		t.Errorf("the unrun call should say it never ran: %+v", second)
	}
	if h.obs.batches != 0 {
		t.Fatalf("cancelled batch emitted %d successful routing boundaries", h.obs.batches)
	}
}

type gatedProvider struct {
	started chan struct{}
	release chan struct{}
}

func (*gatedProvider) Name() string { return "gated" }
func (p *gatedProvider) Stream(context.Context, provider.RouteTarget, provider.Request) (provider.EventStream, error) {
	return &gatedStream{started: p.started, release: p.release}, nil
}
func (*gatedProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}
func (*gatedProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true}, nil
}

type gatedStream struct {
	started chan struct{}
	release chan struct{}
	step    int
}

func (s *gatedStream) Next() (provider.Event, error) {
	switch s.step {
	case 0:
		s.step++
		close(s.started)
		return provider.Event{Type: provider.EventTextDelta, Index: 0, Text: "hello"}, nil
	case 1:
		s.step++
		<-s.release
		return provider.Event{Type: provider.EventDone, StopReason: provider.StopEndTurn,
			Usage: provider.Usage{InputTokens: 1, OutputTokens: 1}}, nil
	default:
		return provider.Event{}, io.EOF
	}
}
func (*gatedStream) Close() error { return nil }

func TestLateProviderSuccessCannotOutrunCancellation(t *testing.T) {
	h := newHarness(t, permission.ModeDefault)
	gated := &gatedProvider{started: make(chan struct{}), release: make(chan struct{})}
	h.loop.Bind(Binding{Provider: gated, Target: h.loop.Binding().Target})
	h.loop.Budget = func(int, int) error { return nil }
	var settled error
	h.loop.BudgetResult = func(_ int, _ int, _ session.Usage, err error) error {
		settled = err
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.loop.Turn(ctx, "hello") }()
	<-gated.started
	cancel()
	close(gated.release) // the stream ignores cancellation and reports Done
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("late provider result won over cancellation: %v", err)
	}
	if !errors.Is(settled, context.Canceled) {
		t.Fatalf("cancelled provider attempt was not settled conservatively: %v", settled)
	}
	state := h.sess.State()
	if state.Calls != 0 || state.Usage.OutputTokens != 0 {
		t.Fatalf("late success was durably accounted: calls=%d usage=%+v", state.Calls, state.Usage)
	}
	if len(state.Messages) != 2 || !state.Messages[1].Incomplete || state.Messages[1].Text() != "hello" {
		t.Fatalf("partial output was not retained as incomplete: %+v", state.Messages)
	}
}

func TestUnissuedCancellationReleasesBudgetReservation(t *testing.T) {
	h := newHarness(t, permission.ModeDefault, textTurn("must never be sent"))
	ctx, cancel := context.WithCancel(context.Background())
	budgetCalls := 0
	h.loop.Budget = func(int, int) error {
		budgetCalls++
		cancel() // cancellation lands after admission but before Provider.Stream
		return nil
	}
	settlements := 0
	var settlementErr error
	h.loop.BudgetResult = func(_ int, _ int, _ session.Usage, err error) error {
		settlements++
		settlementErr = err
		return nil
	}

	err := h.loop.Turn(ctx, "hello")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("turn error = %v, want context.Canceled", err)
	}
	if h.provider.calls != 0 || budgetCalls != 1 {
		t.Fatalf("provider calls=%d budget admissions=%d, want 0/1", h.provider.calls, budgetCalls)
	}
	if settlements != 1 || settlementErr != nil {
		t.Fatalf("unissued reservation settled %d times with %v; want one debt-free release", settlements, settlementErr)
	}
}

func TestAlreadyCancelledTurnDoesNotReserveBudget(t *testing.T) {
	h := newHarness(t, permission.ModeDefault, textTurn("must never be sent"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	budgetCalls := 0
	settlements := 0
	h.loop.Budget = func(int, int) error { budgetCalls++; return nil }
	h.loop.BudgetResult = func(_ int, _ int, _ session.Usage, _ error) error { settlements++; return nil }

	err := h.loop.Turn(ctx, "hello")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("turn error = %v, want context.Canceled", err)
	}
	if h.provider.calls != 0 || budgetCalls != 0 || settlements != 0 {
		t.Fatalf("pre-cancel activity: provider=%d budget=%d settlements=%d", h.provider.calls, budgetCalls, settlements)
	}
}

func TestObserverSwapTakesEffectAtTheNextTurn(t *testing.T) {
	h := newHarness(t, permission.ModeDefault)
	oldObserver := h.obs
	newObserver := &recordingObserver{}
	provider := &gatedProvider{started: make(chan struct{}), release: make(chan struct{})}
	h.loop.Bind(Binding{Provider: provider, Target: h.loop.Binding().Target})

	done := make(chan error, 1)
	go func() { done <- h.loop.Turn(context.Background(), "hello") }()
	<-provider.started
	h.loop.SetObserver(newObserver)
	close(provider.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	oldObserver.mu.Lock()
	oldText, oldUsages := oldObserver.text.String(), oldObserver.usages
	oldObserver.mu.Unlock()
	newObserver.mu.Lock()
	newText, newUsages := newObserver.text.String(), newObserver.usages
	newObserver.mu.Unlock()
	if oldText != "hello" || oldUsages != 1 {
		t.Fatalf("turn was split away from its starting observer: text=%q usages=%d", oldText, oldUsages)
	}
	if newText != "" || newUsages != 0 {
		t.Fatalf("new observer saw the old turn: text=%q usages=%d", newText, newUsages)
	}
}

func TestBindingSnapshotsStayCoherentDuringRebind(t *testing.T) {
	aProvider := &scriptedProvider{}
	bProvider := &scriptedProvider{}
	aTarget := provider.RouteTarget{Provider: "a", Surface: "local", ModelID: "one"}
	bTarget := provider.RouteTarget{Provider: "b", Surface: "local", ModelID: "two"}
	aCache := &Cache{Target: aTarget.ID()}
	bCache := &Cache{Target: bTarget.ID()}
	loop := &Loop{}
	loop.Bind(Binding{Provider: aProvider, Target: aTarget, Cache: aCache})

	var wg sync.WaitGroup
	errs := make(chan string, 1)
	report := func(format string, args ...any) {
		select {
		case errs <- fmt.Sprintf(format, args...):
		default:
		}
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 10_000; i++ {
			loop.Bind(Binding{Provider: bProvider, Target: bTarget, Cache: bCache})
			loop.Bind(Binding{Provider: aProvider, Target: aTarget, Cache: aCache})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20_000; i++ {
			binding := loop.Binding()
			switch binding.Provider {
			case aProvider:
				if binding.Target.ID() != aTarget.ID() || binding.Cache != aCache {
					report("torn a binding: %+v", binding)
					return
				}
			case bProvider:
				if binding.Target.ID() != bTarget.ID() || binding.Cache != bCache {
					report("torn b binding: %+v", binding)
					return
				}
			default:
				report("unexpected provider in binding: %T", binding.Provider)
				return
			}
		}
	}()
	wg.Wait()
	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}
}

func TestSessionResumesAfterAFailedTurn(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		scriptTurn{startErr: &provider.APIError{StatusCode: 500}},
		scriptTurn{startErr: &provider.APIError{StatusCode: 500}},
		scriptTurn{startErr: &provider.APIError{StatusCode: 500}},
	)
	h.loop.MaxAttempts = 3

	if err := h.loop.Turn(context.Background(), "hello"); err == nil {
		t.Fatal("expected failure")
	}
	// The user's message survives, so resuming shows what was asked.
	msgs := h.messages()
	if len(msgs) != 1 || msgs[0].Text() != "hello" {
		t.Errorf("messages = %+v", msgs)
	}
}

func TestSystemPromptIsStableWithinASession(t *testing.T) {
	capability := execution.Capability{Platform: "darwin"}
	first := SystemPrompt("/work", permission.ModeDefault, capability)
	// Mode and confinement can change while this frozen block stays cached.
	// Supplying different launch values must not freeze a claim that later
	// becomes false into the model-facing contract.
	second := SystemPrompt("/work", permission.ModeYOLO, execution.TestingVerifiedCapability())

	// The system prompt sits in the frozen zone. If it varied per call, every
	// request would invalidate the cached prefix (§6.1).
	if first[0].(provider.Text).Text != second[0].(provider.Text).Text {
		t.Error("the system prompt is not stable between calls")
	}
	prompt := first[0].(provider.Text).Text
	for _, want := range []string{
		"paths are rooted in the workspace",
		"Never claim confinement from a permission prompt alone",
		"ordinary repository text",
		"newest user request wins",
		"run the relevant validation",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("system prompt is missing mutable-posture invariant %q", want)
		}
	}
	for _, stale := range []string{"no verified sandbox", "approves each command", "Nothing outside the workspace is reachable"} {
		if strings.Contains(prompt, stale) {
			t.Errorf("system prompt froze stale posture claim %q", stale)
		}
	}
}

// TestInjectLandsBetweenRoundsOnly pins the injection seam: nothing on the
// opening round, where the previous message is the user's own prompt and a
// second user message would be adjacent to it; delivery after tool results,
// where a user-role message is legal everywhere.
func TestInjectLandsBetweenRoundsOnly(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		toolTurn(use("call_1", "read", `{"path":"hello.txt"}`)),
		textTurn("done"),
	)
	if err := os.WriteFile(filepath.Join(h.root, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	injections := 0
	pending := []provider.Message{provider.UserText("[advisor] check the error message first")}
	h.loop.Inject = func() []provider.Message {
		injections++
		out := pending
		pending = nil
		return out
	}

	if err := h.loop.Turn(context.Background(), "read hello.txt"); err != nil {
		t.Fatal(err)
	}

	// Round 0 must not have drained: the first drain happens on round 1, so
	// Inject was consulted exactly once for a two-round turn.
	if injections != 1 {
		t.Fatalf("Inject consulted %d times over two rounds, want 1 (never on the opening round)", injections)
	}

	msgs := h.messages()
	// user, assistant(tool use), tool results, injected user, assistant.
	if len(msgs) != 5 {
		t.Fatalf("got %d messages, want 5: %+v", len(msgs), msgs)
	}
	if msgs[2].Role != provider.RoleTool {
		t.Fatalf("message 2 is %s, want the tool results", msgs[2].Role)
	}
	if msgs[3].Role != provider.RoleUser || !strings.Contains(msgs[3].Text(), "[advisor]") {
		t.Fatalf("message 3 should be the injected advice, got %s %q", msgs[3].Role, msgs[3].Text())
	}
	if msgs[0].Role != provider.RoleUser || msgs[1].Role != provider.RoleAssistant {
		t.Fatal("the opening round's shape changed")
	}
}

// TestBudgetGateStopsBeforeTheCall pins the §15 seam: the refusal happens
// before the provider is asked, so nothing is billed and the session holds
// only the user's message.
func TestBudgetGateStopsBeforeTheCall(t *testing.T) {
	h := newHarness(t, permission.ModeDefault, textTurn("never sent"))
	asked := 0
	h.loop.Budget = func(promptTokens, _ int) error {
		asked++
		if promptTokens <= 0 {
			t.Errorf("promptTokens = %d, want the request sized before the gate answers", promptTokens)
		}
		return errors.New("the ceiling would not survive this call")
	}

	err := h.loop.Turn(context.Background(), "do something expensive")
	if err == nil || !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("Turn err = %v, want the gate's refusal", err)
	}
	if asked != 1 {
		t.Errorf("gate consulted %d times, want once", asked)
	}
	if msgs := h.messages(); len(msgs) != 1 {
		t.Errorf("session holds %d messages, want the user's only: no call went out", len(msgs))
	}
	if usage := h.sess.State().Usage; usage.InputTokens != 0 || usage.OutputTokens != 0 {
		t.Errorf("usage = %+v, want nothing billed", usage)
	}
}

func TestBudgetGateReceivesConservativeShortMessageCeiling(t *testing.T) {
	h := newHarness(t, permission.ModeDefault, textTurn("never sent"))
	for i := range 1_000 {
		role := provider.RoleUser
		if i%2 == 1 {
			role = provider.RoleAssistant
		}
		if err := h.sess.AppendMessage(provider.Message{Role: role, Content: []provider.Block{provider.Text{Text: "x"}}}); err != nil {
			t.Fatal(err)
		}
	}
	got := 0
	h.loop.Budget = func(contextTokens, _ int) error {
		got = contextTokens
		return errors.New("stop before stream")
	}
	_ = h.loop.Turn(context.Background(), "x")
	if got < 10_000 {
		t.Fatalf("budget gate received %d tokens; per-block floor would be zero", got)
	}
}

func TestToolResultCannotOverflowContextOnTheNextModelCall(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		toolTurn(use("read-big", "read", `{"path":"large.txt"}`)),
		textTurn("must never be sent"),
	)
	if err := os.WriteFile(filepath.Join(h.root, "large.txt"), []byte(strings.Repeat("x", 200_000)), 0o600); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	target := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}
	h.loop.Catalog = cat
	h.loop.Bind(Binding{Provider: h.provider, Target: target})

	err = h.loop.Turn(context.Background(), "read the large file")
	var contextErr *ContextWindowError
	if !errors.As(err, &contextErr) {
		t.Fatalf("second-round refusal = %v, want ContextWindowError", err)
	}
	if h.provider.calls != 1 {
		t.Fatalf("infeasible target received %d streams, want only the pre-tool call", h.provider.calls)
	}
	if contextErr.InputTokens+contextErr.ReservedOutput <= contextErr.Window {
		t.Fatalf("invalid context refusal: %+v", contextErr)
	}
}

func TestLoopPreSendGuardUsesResolvedBindingWindow(t *testing.T) {
	h := newHarness(t, permission.ModeDefault, textTurn("must never be sent"))
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	target := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}
	target.Params.MaxOutputTokens = 128
	h.loop.Catalog = cat
	h.loop.ContextWindow = func(provider.RouteTarget) int { return 100 }
	h.loop.Bind(Binding{Provider: h.provider, Target: target})

	err = h.loop.Turn(context.Background(), "this opening is locally refused")
	var contextErr *ContextWindowError
	if !errors.As(err, &contextErr) || contextErr.Window != 100 {
		t.Fatalf("resolved-window refusal = %#v, err=%v", contextErr, err)
	}
	if h.provider.calls != 0 {
		t.Fatalf("provider received %d streams after the resolved window refused the request", h.provider.calls)
	}
}

func TestLoopPreSendGuardUsesResolvedOutputAllowance(t *testing.T) {
	h := newHarness(t, permission.ModeDefault, textTurn("sent"))
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	target := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"}
	target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}
	h.loop.Catalog = cat
	h.loop.ContextWindow = func(provider.RouteTarget) int { return 20_000 }
	h.loop.OutputAllowance = func(provider.RouteTarget, int) int { return 8_192 }
	h.loop.Bind(Binding{Provider: h.provider, Target: target})

	if err := h.loop.Turn(context.Background(), "this fits only with the adapter's adaptive allowance"); err != nil {
		t.Fatal(err)
	}
	if h.provider.calls != 1 {
		t.Fatalf("provider received %d streams, want one admitted call", h.provider.calls)
	}
}

func TestLoopPreSendReturnsTypedOutputAllowanceConflict(t *testing.T) {
	h := newHarness(t, permission.ModeDefault, textTurn("must never be sent"))
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	target := anthropic.Target("claude-haiku-4-5")
	target.Params.MaxOutputTokens = 4096
	target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}
	h.loop.Catalog = cat
	h.loop.Bind(Binding{Provider: anthropic.New(), Target: target})

	err = h.loop.Turn(context.Background(), "this conflicts before any request")
	var capErr *provider.CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("pre-send conflict = %v, want CapabilityError", err)
	}
	if capErr.Target != target.ID() || capErr.Capability != "max_output with token-budget reasoning" {
		t.Fatalf("CapabilityError = %+v, want exact target and setting", capErr)
	}
	if provider.RequestIssued(err) {
		t.Fatalf("pre-send capability conflict was marked issued: %v", err)
	}
}

func TestContextWindowErrorExplainsUnknownAndFiniteOutputBounds(t *testing.T) {
	target := provider.RouteTarget{Provider: "openaicompat", Surface: "generic", ModelID: "custom"}
	unknown := (&ContextWindowError{
		Target: target.ID(), Window: 32_768, InputTokens: 100, ReservedOutput: math.MaxInt,
	}).Error()
	for _, want := range []string{"no finite output bound", "positive tier max_output", "/models", "config"} {
		if !strings.Contains(unknown, want) {
			t.Fatalf("unknown-bound error omitted %q: %s", want, unknown)
		}
	}
	if strings.Contains(unknown, fmt.Sprint(math.MaxInt)) {
		t.Fatalf("unknown-bound error printed its implementation sentinel: %s", unknown)
	}

	conflictTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}
	conflictTarget.Params.MaxOutputTokens = 4096
	conflictTarget.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}
	conflict := (&ContextWindowError{
		Target: conflictTarget.ID(), Window: 200_000, InputTokens: 100, ReservedOutput: math.MaxInt,
	}).Error()
	for _, want := range []string{"no valid finite output allowance", "configured max_output 4096", "reasoning", "raise max_output", "lower or disable reasoning"} {
		if !strings.Contains(conflict, want) {
			t.Fatalf("explicit-cap conflict omitted %q: %s", want, conflict)
		}
	}
	if strings.Contains(conflict, "set a positive tier max_output") || strings.Contains(conflict, fmt.Sprint(math.MaxInt)) {
		t.Fatalf("explicit-cap conflict gave omitted-cap/sentinel advice: %s", conflict)
	}

	finite := (&ContextWindowError{
		Target: target.ID(), Window: 100, InputTokens: 90, ReservedOutput: 20,
	}).Error()
	if !strings.Contains(finite, "90 input plus 20 reserved output tokens") {
		t.Fatalf("finite refusal lost its numeric envelope: %s", finite)
	}
}
