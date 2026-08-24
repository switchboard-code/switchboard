package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

type delayedCancellationProvider struct {
	blocked   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
}

func (*delayedCancellationProvider) Name() string { return "delayed-cancellation" }

func (p *delayedCancellationProvider) Stream(ctx context.Context, _ provider.RouteTarget, _ provider.Request) (provider.EventStream, error) {
	return &delayedCancellationStream{ctx: ctx, owner: p}, nil
}

func (*delayedCancellationProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}

func (*delayedCancellationProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{}, nil
}

type delayedCancellationStream struct {
	ctx   context.Context
	owner *delayedCancellationProvider
	step  int
}

func (s *delayedCancellationStream) Next() (provider.Event, error) {
	if s.step == 0 {
		s.step++
		return provider.Event{Type: provider.EventTextDelta, Text: "durable partial answer"}, nil
	}
	close(s.owner.blocked)
	<-s.ctx.Done()
	close(s.owner.cancelled)
	<-s.owner.release
	return provider.Event{}, s.ctx.Err()
}

func (*delayedCancellationStream) Close() error { return nil }

type completedTurnProvider struct {
	events []provider.Event
}

func (*completedTurnProvider) Name() string { return "completed-turn" }

func (p *completedTurnProvider) Stream(context.Context, provider.RouteTarget, provider.Request) (provider.EventStream, error) {
	return &completedTurnStream{events: append([]provider.Event(nil), p.events...)}, nil
}

func (*completedTurnProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}

func (*completedTurnProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{}, nil
}

type completedTurnStream struct {
	events []provider.Event
}

func (s *completedTurnStream) Next() (provider.Event, error) {
	if len(s.events) == 0 {
		return provider.Event{}, io.EOF
	}
	event := s.events[0]
	s.events = s.events[1:]
	return event, nil
}

func (*completedTurnStream) Close() error { return nil }

func bindExitTestProvider(m *tuiModel, p provider.Provider) {
	binding := m.app.loop.Binding()
	binding.Provider = p
	m.app.loop.Bind(binding)
}

func launchExitTestTurn(m *tuiModel, prompt string) {
	m.beginTurn(prompt)
	m.launchModelTurn(provider.UserText(prompt))
}

func TestAbnormalTUIExitWaitsForInterruptedDraftAndRoutePersistence(t *testing.T) {
	for attempt := 0; attempt < 3; attempt++ {
		m := testModel(t)
		p := &delayedCancellationProvider{
			blocked: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{}),
		}
		bindExitTestProvider(m, p)
		launchExitTestTurn(m, "stream until the terminal disappears")
		select {
		case <-p.blocked:
		case <-time.After(5 * time.Second):
			t.Fatal("turn did not reach its blocking provider read")
		}

		exited := make(chan error, 1)
		go func() {
			exited <- runTUIProgram(tuiProgramFunc(func() (tea.Model, error) { return m, nil }), m)
		}()
		select {
		case <-p.cancelled:
		case <-time.After(5 * time.Second):
			t.Fatal("TUI exit did not cancel the primary provider")
		}
		select {
		case err := <-exited:
			t.Fatalf("runTUIProgram returned before the cancelled provider released: %v", err)
		case <-time.After(25 * time.Millisecond):
		}

		// The first streamed event is checkpointed before observer delivery and
		// before the second Next call blocks. It is already durable, but the final
		// incomplete-message and route records still belong to the running turn.
		mid := m.app.loop.Session.State()
		if len(mid.Messages) != 2 || !mid.Messages[1].Incomplete || mid.Messages[1].Text() != "durable partial answer" {
			t.Fatalf("mid-cancellation draft = %#v", mid.Messages)
		}
		close(p.release)
		select {
		case err := <-exited:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("runTUIProgram did not join the released primary turn")
		}
		if m.primaryTurn != nil {
			t.Fatal("joined primary turn retained TUI ownership")
		}

		state := m.app.loop.Session.State()
		if state.Calls != 0 || len(state.Messages) != 2 || !state.Messages[1].Incomplete || state.Messages[1].Text() != "durable partial answer" {
			t.Fatalf("settled interrupted state = calls:%d messages:%#v", state.Calls, state.Messages)
		}
		timeline, err := session.ReadTimeline(m.app.loop.Session.Path())
		if err != nil {
			t.Fatal(err)
		}
		var recorded *session.Route
		for _, item := range timeline {
			if item.Route != nil {
				recorded = item.Route
			}
		}
		if recorded == nil || recorded.FailureKind != session.RouteFailureCancelled {
			t.Fatalf("interrupted turn route = %+v", recorded)
		}
	}
}

func TestAbnormalTUIExitWaitsForUsageSettlementBeforeRoute(t *testing.T) {
	m := testModel(t)
	wantUsage := provider.Usage{InputTokens: 23, OutputTokens: 7}
	p := &completedTurnProvider{events: []provider.Event{
		{Type: provider.EventTextDelta, Text: "completed answer"},
		{Type: provider.EventDone, StopReason: provider.StopEndTurn, Usage: wantUsage},
	}}
	bindExitTestProvider(m, p)
	settlementEntered := make(chan struct{})
	settlementRelease := make(chan struct{})
	settlementDone := make(chan struct{})
	m.app.loop.BudgetResult = func(_ int, _ int, got session.Usage, err error) error {
		if err != nil {
			return err
		}
		if got.Usage != wantUsage || got.CallID == "" {
			return errors.New("budget settlement did not receive the durable usage record")
		}
		close(settlementEntered)
		<-settlementRelease
		close(settlementDone)
		return nil
	}
	launchExitTestTurn(m, "finish while the terminal disappears")
	select {
	case <-settlementEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not reach its post-usage settlement")
	}
	if got := m.app.loop.Session.State(); got.Calls != 1 || got.Usage != wantUsage {
		t.Fatalf("usage before settlement = calls:%d usage:%+v", got.Calls, got.Usage)
	}
	timeline, err := session.ReadTimeline(m.app.loop.Session.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range timeline {
		if item.Route != nil {
			t.Fatal("route was appended before the admitted call settled")
		}
	}

	exited := make(chan error, 1)
	go func() {
		exited <- runTUIProgram(tuiProgramFunc(func() (tea.Model, error) { return m, nil }), m)
	}()
	select {
	case err := <-exited:
		t.Fatalf("runTUIProgram returned before usage settlement: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(settlementRelease)
	select {
	case err := <-exited:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runTUIProgram did not join the settled primary turn")
	}
	select {
	case <-settlementDone:
	default:
		t.Fatal("runTUIProgram returned before the budget callback finished")
	}

	timeline, err = session.ReadTimeline(m.app.loop.Session.Path())
	if err != nil {
		t.Fatal(err)
	}
	var recorded *session.Route
	for _, item := range timeline {
		if item.Route != nil {
			recorded = item.Route
		}
	}
	if recorded == nil || recorded.Usage != wantUsage || len(recorded.UsageCallIDs) != 1 || recorded.Outcome != "completed" {
		t.Fatalf("settled route = %+v", recorded)
	}
}
