package advisor

import (
	"context"
	"errors"
	"testing"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/provider"
)

type advisorLimitProvider struct {
	ctx    context.Context
	stream *advisorLimitStream
}

func (*advisorLimitProvider) Name() string { return "advisor-limit" }

func (p *advisorLimitProvider) Stream(ctx context.Context, _ provider.RouteTarget, _ provider.Request) (provider.EventStream, error) {
	p.ctx = ctx
	p.stream = &advisorLimitStream{ctx: ctx}
	return p.stream, nil
}

func (*advisorLimitProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}

func (*advisorLimitProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{}, nil
}

type advisorLimitStream struct {
	ctx              context.Context
	next             int
	closes           int
	closeSawCanceled bool
}

func (s *advisorLimitStream) Next() (provider.Event, error) {
	s.next++
	return provider.Event{Type: provider.EventThinkingDelta}, nil
}

func (s *advisorLimitStream) Close() error {
	s.closes++
	s.closeSawCanceled = s.ctx.Err() != nil
	return nil
}

func TestConsultCancelsAndClosesEndlessIgnoredThinkingStream(t *testing.T) {
	p := &advisorLimitProvider{}
	a := New(agent.NopObserver{}, p, target(), nil)

	advice, err := a.consult(context.Background(), "task", "evidence", "trigger")
	if !errors.Is(err, provider.ErrStreamLimit) {
		t.Fatalf("advice = %q, err = %v, want ErrStreamLimit", advice, err)
	}
	if p.ctx == nil {
		t.Fatal("provider was not called")
	}
	if !errors.Is(p.ctx.Err(), context.Canceled) {
		t.Fatalf("provider context err = %v, want canceled", p.ctx.Err())
	}
	if p.stream == nil {
		t.Fatal("provider returned no stream")
	}
	if p.stream.next != provider.ProviderStreamMaxEvents+1 {
		t.Fatalf("stream reads = %d, want %d", p.stream.next, provider.ProviderStreamMaxEvents+1)
	}
	if p.stream.closes != 1 || !p.stream.closeSawCanceled {
		t.Fatalf("stream closes = %d, saw canceled = %v", p.stream.closes, p.stream.closeSawCanceled)
	}
}
