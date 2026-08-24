package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/advisor"
	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/tools"
)

type exitBlockingAdvisorProvider struct {
	started chan struct{}
	exited  chan struct{}
}

func (*exitBlockingAdvisorProvider) Name() string { return "exit-blocking-advisor" }

func (p *exitBlockingAdvisorProvider) Stream(ctx context.Context, _ provider.RouteTarget, _ provider.Request) (provider.EventStream, error) {
	close(p.started)
	<-ctx.Done()
	close(p.exited)
	return nil, ctx.Err()
}

func (*exitBlockingAdvisorProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}

func (*exitBlockingAdvisorProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{}, nil
}

func TestRunTUIStopsAdvisorBeforeReturningSessionOwnership(t *testing.T) {
	m := testModel(t)
	p := &exitBlockingAdvisorProvider{started: make(chan struct{}), exited: make(chan struct{})}
	settled := make(chan error, 1)
	adv := advisor.New(agent.NopObserver{}, p, m.app.tier.Target, nil,
		advisor.WithBounds(1, time.Nanosecond),
		advisor.WithMeter(func(provider.Request) (advisor.AttemptFinish, error) {
			return func(_ provider.Usage, err error) error {
				settled <- err
				return nil
			}, nil
		}))
	m.app.setAdvisor(adv)
	adv.StartTurn("task")
	req := permission.Request{Tool: "exec", Argv: []string{"go", "test", "./..."}}
	for i := 0; i < 4; i++ {
		call := provider.ToolUse{
			ID:    fmt.Sprintf("call-%d", i),
			Name:  "exec",
			Input: json.RawMessage(`{"argv":["go","test","./..."]}`),
		}
		adv.ToolStart(call, req)
		adv.ToolEnd(call, req, tools.Result{Content: "FAIL: TestX", IsError: true}, time.Second)
	}
	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		t.Fatal("advisor consult did not start")
	}

	terminalErr := errors.New("terminal disconnected")
	err := runTUIProgram(tuiProgramFunc(func() (tea.Model, error) { return m, terminalErr }), m)
	if !errors.Is(err, terminalErr) {
		t.Fatalf("runTUIProgram error = %v", err)
	}
	select {
	case <-p.exited:
	default:
		t.Fatal("runTUIProgram returned before the advisor provider exited")
	}
	select {
	case err := <-settled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("advisor settlement error = %v, want cancellation", err)
		}
	default:
		t.Fatal("runTUIProgram returned before the advisor meter settled")
	}
}
