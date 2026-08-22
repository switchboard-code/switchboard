package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/continuity"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
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
	turns    []scriptTurn
	calls    int
	requests []provider.Request
}

func (p *scriptedProvider) Name() string { return "scripted" }

func (p *scriptedProvider) Stream(_ context.Context, _ provider.RouteTarget, req provider.Request) (provider.EventStream, error) {
	p.requests = append(p.requests, req)
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

func TestCancellationStillPairsResultsWithCalls(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		toolTurn(
			use("call_1", "exec", `{"command":["sleep","10"],"timeout_seconds":30}`),
			use("call_2", "exec", `{"command":["echo","never"]}`),
		),
	)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	err := h.loop.Turn(ctx, "run things")
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
