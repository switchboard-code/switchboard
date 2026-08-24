package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// oneTurnProvider streams a single text answer and stops.
type oneTurnProvider struct{ text string }

func (p *oneTurnProvider) Name() string { return "scripted" }

func (p *oneTurnProvider) Stream(context.Context, provider.RouteTarget, provider.Request) (provider.EventStream, error) {
	return &oneTurnStream{events: []provider.Event{
		{Type: provider.EventTextDelta, Index: 0, Text: p.text},
		{Type: provider.EventDone, StopReason: provider.StopEndTurn, Usage: provider.Usage{InputTokens: 10, OutputTokens: 5}},
	}}, nil
}

func (p *oneTurnProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}

func (p *oneTurnProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true}, nil
}

type streamErrorProvider struct{}

func (*streamErrorProvider) Name() string { return "broken-stream" }

func (*streamErrorProvider) Stream(context.Context, provider.RouteTarget, provider.Request) (provider.EventStream, error) {
	return nil, errors.New("provider stream failed")
}

func (*streamErrorProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}

func (*streamErrorProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true}, nil
}

type twoRoundBlockingProvider struct {
	secondStarted chan struct{}
	releaseSecond chan struct{}
	calls         int
}

func (p *twoRoundBlockingProvider) Name() string { return "scripted-live-usage" }

func (p *twoRoundBlockingProvider) Stream(ctx context.Context, _ provider.RouteTarget, _ provider.Request) (provider.EventStream, error) {
	p.calls++
	switch p.calls {
	case 1:
		return &oneTurnStream{events: []provider.Event{
			{Type: provider.EventToolUse, Index: 0, ToolUse: &provider.ToolUse{
				ID: "todo-1", Name: "todo", Input: json.RawMessage(`{"items":[]}`),
			}},
			{Type: provider.EventDone, StopReason: provider.StopToolUse, Usage: provider.Usage{InputTokens: 1_000, OutputTokens: 100}},
		}}, nil
	case 2:
		close(p.secondStarted)
		select {
		case <-p.releaseSecond:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &oneTurnStream{events: []provider.Event{
			{Type: provider.EventTextDelta, Index: 0, Text: "Finished after the tool round."},
			{Type: provider.EventDone, StopReason: provider.StopEndTurn, Usage: provider.Usage{InputTokens: 1_000, OutputTokens: 100}},
		}}, nil
	default:
		return nil, errors.New("unexpected provider call")
	}
}

func (*twoRoundBlockingProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}

func (*twoRoundBlockingProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true}, nil
}

type oneTurnStream struct {
	events []provider.Event
	i      int
}

func (s *oneTurnStream) Next() (provider.Event, error) {
	if s.i < len(s.events) {
		ev := s.events[s.i]
		s.i++
		return ev, nil
	}
	return provider.Event{}, io.EOF
}

func (s *oneTurnStream) Close() error { return nil }

type fallbackNoticeObserver struct {
	agent.NopObserver
	want string
	seen atomic.Bool
}

func (o *fallbackNoticeObserver) Notice(level, text string) {
	if level == "warn" && strings.Contains(text, o.want) {
		o.seen.Store(true)
	}
}

type fallbackOrderedProvider struct {
	observer *fallbackNoticeObserver
	calls    atomic.Int32
	ordered  atomic.Bool
}

func (*fallbackOrderedProvider) Name() string { return "fallback-ordered" }

func (p *fallbackOrderedProvider) Stream(context.Context, provider.RouteTarget, provider.Request) (provider.EventStream, error) {
	p.calls.Add(1)
	p.ordered.Store(p.observer != nil && p.observer.seen.Load())
	return &oneTurnStream{events: []provider.Event{
		{Type: provider.EventTextDelta, Index: 0, Text: "done"},
		{Type: provider.EventDone, StopReason: provider.StopEndTurn},
	}}, nil
}

func (*fallbackOrderedProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}

func (*fallbackOrderedProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true}, nil
}

func ladder() []config.Tier {
	return []config.Tier{
		{ID: "t1", Label: "light", Target: provider.RouteTarget{Provider: "scripted", Surface: "local", ModelID: "small"}},
		{ID: "t2", Label: "deep", Target: provider.RouteTarget{Provider: "scripted", Surface: "local", ModelID: "big"}},
	}
}

func testConfig(t *testing.T, answer string) Config {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()

	return Config{
		Tiers: ladder(),
		Probe: func(_ context.Context, tierID string) (config.Tier, provider.Provider, string, error) {
			for _, tier := range ladder() {
				if tier.ID == tierID {
					return tier, &oneTurnProvider{text: answer}, "", nil
				}
			}
			t.Fatalf("probe asked for unknown tier %s", tierID)
			return config.Tier{}, nil, "", nil
		},
		NewSession: func(target provider.RouteTargetID) (*session.Session, error) {
			return store.Create(workspace, target, "test-revision")
		},
		NewLoop: func(tier config.Tier, client provider.Provider, sess *session.Session, obs agent.Observer, named *Agent, _ TaskRef) (*agent.Loop, error) {
			registry, err := tools.NewRegistry(workspace, execution.Capability{})
			if err != nil {
				return nil, err
			}
			if named != nil && len(named.Tools) > 0 {
				if err := registry.Restrict(named.Tools); err != nil {
					return nil, err
				}
			}
			return &agent.Loop{
				Provider:      client,
				Target:        tier.Target,
				Tools:         registry,
				Perms:         permission.NewEngine(permission.ModeDefault, execution.Capability{}),
				Session:       sess,
				System:        []provider.Block{provider.Text{Text: Preamble}},
				Observer:      obs,
				MaxToolRounds: MaxRounds,
			}, nil
		},
	}
}

func plan(t *testing.T, tool tools.Tool, input string) tools.Plan {
	t.Helper()
	p, err := tool.Plan(json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewRequiresALadderAndWiring(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("an empty ladder must be an error")
	}
	if _, err := New(Config{Tiers: ladder()}); err == nil {
		t.Error("missing wiring must be an error")
	}
}

func TestPlanValidatesTaskAndTier(t *testing.T) {
	tool, err := New(testConfig(t, "ok"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := tool.Plan(json.RawMessage(`{"task":"  "}`)); err == nil {
		t.Error("an empty task must fail at Plan time")
	}
	if _, err := tool.Plan(json.RawMessage(`{"task":"x","tier":"t9"}`)); err == nil {
		t.Error("a tier outside the ladder must fail at Plan time")
	}

	p := plan(t, tool, `{"task":"find the flaky test"}`)
	if p.Request.Effect != permission.EffectRead {
		t.Errorf("spawning carries effect %q, want read: each sub call is gated on its own", p.Request.Effect)
	}
	if !strings.Contains(p.Request.Detail, "] t1 → ") {
		t.Errorf("Detail = %q, want the default bottom rung named", p.Request.Detail)
	}
}

func TestRunReturnsTheFinalAnswerWithATrailer(t *testing.T) {
	tool, err := New(testConfig(t, "The flaky test is TestRetry; it races on port 8080."))
	if err != nil {
		t.Fatal(err)
	}

	p := plan(t, tool, `{"task":"find the flaky test","tier":"t2"}`)
	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("run failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "TestRetry") {
		t.Errorf("content = %q, want the subagent's answer", res.Content)
	}
	if !strings.Contains(res.Content, "[delegate on t2:") {
		t.Errorf("content = %q, want the trailer naming the rung it ran on", res.Content)
	}
}

func TestRunnerFallbackNoteIsDurableAndObservedBeforeContent(t *testing.T) {
	cfg := testConfig(t, "unused")
	const note = "t2 is served by its fallback scripted/local/backup: primary unavailable"
	observer := &fallbackNoticeObserver{want: note}
	client := &fallbackOrderedProvider{observer: observer}
	fallbackTier := cfg.Tiers[1]
	fallbackTier.Target.ModelID = "backup"
	cfg.Probe = func(context.Context, string) (config.Tier, provider.Provider, string, error) {
		return fallbackTier, client, note, nil
	}
	cfg.Forward = func() agent.Observer { return observer }
	newSession := cfg.NewSession
	var child *session.Session
	cfg.NewSession = func(target provider.RouteTargetID) (*session.Session, error) {
		var err error
		child, err = newSession(target)
		return child, err
	}

	tool, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := plan(t, tool, `{"task":"inspect","tier":"t2"}`)
	res, err := p.Run(context.Background())
	if err != nil || res.IsError {
		t.Fatalf("fallback delegate failed: result=%+v err=%v", res, err)
	}
	if client.calls.Load() != 1 || !client.ordered.Load() {
		t.Fatalf("provider order = calls:%d notice-before-call:%v", client.calls.Load(), client.ordered.Load())
	}
	if child == nil {
		t.Fatal("delegate created no child session")
	}
	timeline, err := session.ReadTimeline(child.Path())
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, item := range timeline {
		if item.Note != nil && item.Note.Text == note {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("delegate fallback timeline count = %d, want one", count)
	}
}

func TestRunnerFallbackAppendFailureStopsDelegateAndWorkflowContent(t *testing.T) {
	cfg := testConfig(t, "unused")
	const note = "t2 is served by its fallback scripted/local/backup: primary unavailable"
	observer := &fallbackNoticeObserver{want: note}
	client := &fallbackOrderedProvider{observer: observer}
	fallbackTier := cfg.Tiers[1]
	fallbackTier.Target.ModelID = "backup"
	cfg.Probe = func(context.Context, string) (config.Tier, provider.Provider, string, error) {
		return fallbackTier, client, note, nil
	}
	cfg.Forward = func() agent.Observer { return observer }
	newSession := cfg.NewSession
	var child *session.Session
	cfg.NewSession = func(target provider.RouteTargetID) (*session.Session, error) {
		var err error
		child, err = newSession(target)
		if err == nil {
			err = child.Close()
		}
		return child, err
	}

	tool, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := plan(t, tool, `{"task":"must not leave","tier":"t2"}`)
	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "could not record the fallback note") {
		t.Fatalf("fallback append refusal = %+v", res)
	}
	if client.calls.Load() != 0 || observer.seen.Load() {
		t.Fatalf("failed append leaked content or notice: calls=%d notice=%v", client.calls.Load(), observer.seen.Load())
	}
	if child == nil {
		t.Fatal("delegate created no child session")
	}
	timeline, err := session.ReadTimeline(child.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range timeline {
		if item.Note != nil && item.Note.Text == note {
			t.Fatal("failed delegate append left a fallback note")
		}
	}
}

func TestTaskUsageIsLiveBetweenProviderCallsAndFinishesExact(t *testing.T) {
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	target := provider.RouteTarget{
		Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5",
	}
	info, _, ok := cat.Lookup(target)
	if !ok {
		t.Fatalf("priced test target %s is missing from the bundled catalog", target.ID())
	}
	perCall, _, ok := info.Cost(provider.Usage{InputTokens: 1_000, OutputTokens: 100})
	if !ok || perCall <= 0 {
		t.Fatalf("priced test usage = %s, priced %v", perCall, ok)
	}

	manager := NewTaskManager(1)
	blocked := &twoRoundBlockingProvider{
		secondStarted: make(chan struct{}),
		releaseSecond: make(chan struct{}),
	}
	c := testConfig(t, "unused")
	c.Tiers = []config.Tier{{ID: "t1", Label: "priced", Target: target}}
	c.Tasks = manager
	c.ParentSession = func() string { return "primary-live" }
	c.Probe = func(context.Context, string) (config.Tier, provider.Provider, string, error) {
		return c.Tiers[0], blocked, "", nil
	}
	originalNewLoop := c.NewLoop
	c.NewLoop = func(tier config.Tier, client provider.Provider, sess *session.Session, obs agent.Observer, named *Agent, task TaskRef) (*agent.Loop, error) {
		loop, err := originalNewLoop(tier, client, sess, obs, named, task)
		if err != nil {
			return nil, err
		}
		loop.Catalog = cat
		return loop, nil
	}

	tool, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	runPlan := plan(t, tool, `{"task":"use a tool, then finish"}`)
	type outcome struct {
		result tools.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := runPlan.Run(context.Background())
		done <- outcome{result: result, err: err}
	}()

	select {
	case <-blocked.secondStarted:
	case <-time.After(time.Second):
		t.Fatal("delegate did not reach its second provider call")
	}
	live := manager.List()
	if len(live) != 1 {
		t.Fatalf("live task snapshot = %+v", live)
	}
	if live[0].Status != TaskRunning || live[0].Calls != 1 || live[0].CostMicroUSD != int64(perCall) {
		t.Fatalf("live task metrics = %+v, want one durable call costing %s", live[0], perCall)
	}

	close(blocked.releaseSecond)
	var finished outcome
	select {
	case finished = <-done:
	case <-time.After(time.Second):
		t.Fatal("delegate did not finish after the second call was released")
	}
	if finished.err != nil || finished.result.IsError {
		t.Fatalf("delegate result = %+v, error %v", finished.result, finished.err)
	}
	final := manager.List()
	if len(final) != 1 || final[0].Status != TaskSucceeded || final[0].Calls != 2 || final[0].CostMicroUSD != int64(perCall)*2 {
		t.Fatalf("final task metrics = %+v, want two exact provider receipts", final)
	}
}

func TestPartialAnswerRemainsUsableButTaskReportsFailure(t *testing.T) {
	c := testConfig(t, "unused")
	manager := NewTaskManager(1)
	c.Tasks = manager
	c.ParentSession = func() string { return "primary" }
	c.Probe = func(_ context.Context, tierID string) (config.Tier, provider.Provider, string, error) {
		for _, tier := range ladder() {
			if tier.ID == tierID {
				return tier, &streamErrorProvider{}, "", nil
			}
		}
		return config.Tier{}, nil, "", errors.New("unknown tier")
	}
	originalNewLoop := c.NewLoop
	c.NewLoop = func(tier config.Tier, client provider.Provider, sess *session.Session, obs agent.Observer, named *Agent, task TaskRef) (*agent.Loop, error) {
		if err := sess.AppendMessage(provider.UserText("earlier step")); err != nil {
			return nil, err
		}
		if err := sess.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
			provider.Text{Text: "Useful findings collected before the retry."},
		}}); err != nil {
			return nil, err
		}
		return originalNewLoop(tier, client, sess, obs, named, task)
	}

	tool, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	res, err := plan(t, tool, `{"task":"finish the investigation"}`).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(res.Content, "Useful findings") || !strings.Contains(res.Content, "stopped early") {
		t.Fatalf("partial result should remain usable and disclose failure: %+v", res)
	}
	tasks := manager.List()
	if len(tasks) != 1 || tasks[0].Status != TaskFailed || !strings.Contains(tasks[0].Error, "provider stream failed") {
		t.Fatalf("partial task status = %+v, want failed with provider reason", tasks)
	}
}

func TestAssemblyFailureStillClosesTaskAccounting(t *testing.T) {
	c := testConfig(t, "unused")
	manager := NewTaskManager(1)
	c.Tasks = manager
	c.ParentSession = func() string { return "primary" }
	c.NewLoop = func(config.Tier, provider.Provider, *session.Session, agent.Observer, *Agent, TaskRef) (*agent.Loop, error) {
		return nil, errors.New("broken assembly")
	}
	finished := 0
	c.Finish = func(*session.Session) error {
		finished++
		return nil
	}
	tool, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	res, err := plan(t, tool, `{"task":"inspect"}`).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "broken assembly") {
		t.Fatalf("result = %+v", res)
	}
	if finished != 1 {
		t.Fatalf("Finish called %d times, want once", finished)
	}
	tasks := manager.List()
	if len(tasks) != 1 || tasks[0].Status != TaskFailed || tasks[0].DelegateSessionID == "" {
		t.Fatalf("failed task attribution = %+v", tasks)
	}
}

func TestForwardingFiltersWhatWouldMislead(t *testing.T) {
	rec := &recorder{}
	f := &forwarding{parent: rec, task: TaskRef{ID: "task-004", Name: "scan api"}}

	f.TextDelta("streamed text")
	f.ThinkingDelta("thoughts")
	f.TurnUsage(session.Usage{})
	todo := provider.ToolUse{ID: "1", Name: "todo"}
	grep := provider.ToolUse{ID: "2", Name: "grep"}
	f.ToolStart(todo, permission.Request{Tool: "todo"})
	f.ToolEnd(todo, permission.Request{Tool: "todo"}, tools.Result{}, time.Millisecond)
	f.ToolStart(grep, permission.Request{Tool: "grep"})
	f.ToolEnd(grep, permission.Request{Tool: "grep"}, tools.Result{Content: "hit"}, time.Millisecond)
	f.Notice("warn", "retrying")

	if len(rec.starts) != 1 || rec.starts[0] != "grep" {
		t.Errorf("starts = %v, want grep only: a sub todo would collide with the primary's", rec.starts)
	}
	if len(rec.ends) != 1 || rec.ends[0] != "grep" {
		t.Errorf("ends = %v", rec.ends)
	}
	if len(rec.notices) != 1 {
		t.Errorf("notices = %v, want the retry surfaced", rec.notices)
	}
	if len(rec.ids) != 1 || rec.ids[0] != "task-004/2" || !strings.Contains(rec.details[0], "task-004 scan api") {
		t.Errorf("forwarded attribution ids=%v details=%v", rec.ids, rec.details)
	}
	if !strings.Contains(rec.notices[0], "task-004 scan api") {
		t.Errorf("notice lost task attribution: %v", rec.notices)
	}
}

func TestDelegateUsesAnExclusiveParallelBatchGroup(t *testing.T) {
	tool, err := New(testConfig(t, "ok"))
	if err != nil {
		t.Fatal(err)
	}
	if tool.ParallelSafe() {
		t.Fatal("delegate was marked generally parallel-safe despite opaque writes")
	}
	grouped, ok := tool.(tools.ParallelBatchTool)
	if !ok || grouped.ParallelBatchKey() != "delegate" {
		t.Fatalf("delegate parallel group = %T %#v", tool, grouped)
	}
}

func TestFinalTextSkipsIncompleteMessages(t *testing.T) {
	state := session.State{Messages: []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "the real answer"}}},
		{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "interrupted"}}, Incomplete: true},
	}}
	if got := finalText(state); got != "the real answer" {
		t.Errorf("finalText = %q", got)
	}
}

// TestNoAgentsLeavesTheToolByteIdentical guards the frozen zone: a session
// with no definitions must render the same schema and description it always
// has, or every existing session's cached prefix breaks on upgrade.
func TestNoAgentsLeavesTheToolByteIdentical(t *testing.T) {
	tool, err := New(testConfig(t, "ok"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(tool.Schema()), `"agent"`) {
		t.Error("the bare schema must not carry an agent property")
	}
	if strings.Contains(tool.Description(), "Named agents") {
		t.Error("the bare description must not enumerate agents")
	}
}

func agentConfig(t *testing.T, answer string) Config {
	t.Helper()
	c := testConfig(t, answer)
	c.Agents = []Agent{
		{Name: "reviewer", Description: "reviews a diff", Tier: "t2", Tools: []string{"read", "grep"}, Prompt: "You review changes."},
		{Name: "scout", Description: "finds things", Prompt: "You search."},
	}
	return c
}

func TestAgentsAppearInSchemaAndDescription(t *testing.T) {
	tool, err := New(agentConfig(t, "ok"))
	if err != nil {
		t.Fatal(err)
	}
	schema := string(tool.Schema())
	if !strings.Contains(schema, `"enum": ["reviewer","scout"]`) {
		t.Errorf("schema = %s, want the agent names enumerated", schema)
	}
	desc := tool.Description()
	if !strings.Contains(desc, "reviewer: reviews a diff (runs on t2)") {
		t.Errorf("description = %q, want each agent's charter and rung", desc)
	}
}

func TestPlanResolvesTheAgentAndItsDefaultRung(t *testing.T) {
	tool, err := New(agentConfig(t, "ok"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := tool.Plan(json.RawMessage(`{"task":"x","agent":"nobody"}`)); err == nil {
		t.Error("an undefined agent must fail at Plan time")
	}

	p := plan(t, tool, `{"task":"check the diff","agent":"reviewer"}`)
	if !strings.Contains(p.Request.Detail, "] reviewer on t2 → ") {
		t.Errorf("Detail = %q, want the agent's default rung", p.Request.Detail)
	}

	p = plan(t, tool, `{"task":"check the diff","agent":"reviewer","tier":"t1"}`)
	if !strings.Contains(p.Request.Detail, "] reviewer on t1 → ") {
		t.Errorf("Detail = %q, want the explicit tier to win", p.Request.Detail)
	}

	p = plan(t, tool, `{"task":"look around","agent":"scout"}`)
	if !strings.Contains(p.Request.Detail, "] scout on t1 → ") {
		t.Errorf("Detail = %q, want a rungless agent on the bottom", p.Request.Detail)
	}
}

func TestRunNamesTheAgentInTheTrailer(t *testing.T) {
	tool, err := New(agentConfig(t, "Looks correct."))
	if err != nil {
		t.Fatal(err)
	}
	p := plan(t, tool, `{"task":"check the diff","agent":"reviewer"}`)
	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("run failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "[delegate reviewer on t2:") {
		t.Errorf("content = %q, want the trailer naming who ran", res.Content)
	}
}

type recorder struct {
	starts, ends, notices []string
	ids, details          []string
}

func (r *recorder) ThinkingDelta(string) {}
func (r *recorder) TextDelta(string)     {}
func (r *recorder) ToolStart(call provider.ToolUse, req permission.Request) {
	r.starts = append(r.starts, call.Name)
	r.ids = append(r.ids, call.ID)
	r.details = append(r.details, req.Detail)
}
func (r *recorder) ToolEnd(call provider.ToolUse, _ permission.Request, _ tools.Result, _ time.Duration) {
	r.ends = append(r.ends, call.Name)
}
func (r *recorder) ToolBatchEnd(context.Context) {}
func (r *recorder) Notice(_, text string)        { r.notices = append(r.notices, text) }
func (r *recorder) TurnUsage(session.Usage)      {}
