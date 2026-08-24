package main

import (
	"context"
	"errors"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
)

type auxiliaryLimitProvider struct {
	ctx    context.Context
	stream *auxiliaryLimitStream
}

func (*auxiliaryLimitProvider) Name() string { return "auxiliary-limit" }

func (p *auxiliaryLimitProvider) Stream(ctx context.Context, _ provider.RouteTarget, _ provider.Request) (provider.EventStream, error) {
	p.ctx = ctx
	p.stream = &auxiliaryLimitStream{ctx: ctx}
	return p.stream, nil
}

func (*auxiliaryLimitProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}

func (*auxiliaryLimitProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{}, nil
}

type auxiliaryLimitStream struct {
	ctx              context.Context
	next             int
	closes           int
	closeSawCanceled bool
}

func (s *auxiliaryLimitStream) Next() (provider.Event, error) {
	s.next++
	return provider.Event{Type: provider.EventThinkingDelta}, nil
}

func (s *auxiliaryLimitStream) Close() error {
	s.closes++
	s.closeSawCanceled = s.ctx.Err() != nil
	return nil
}

func assertAuxiliaryStreamLimit(t *testing.T, p *auxiliaryLimitProvider, err error) {
	t.Helper()
	if !errors.Is(err, provider.ErrStreamLimit) {
		t.Fatalf("err = %v, want ErrStreamLimit", err)
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

func TestSummarizeRequestCallCancelsAndClosesEndlessIgnoredThinkingStream(t *testing.T) {
	p := &auxiliaryLimitProvider{}
	text, usage, done, err := summarizeRequestCall(context.Background(), p, provider.RouteTarget{}, provider.Request{})
	if text != "" || usage != (provider.Usage{}) || done {
		t.Fatalf("text = %q, usage = %+v, done = %v", text, usage, done)
	}
	assertAuxiliaryStreamLimit(t, p, err)
}

func TestDistillRequestCallCancelsAndClosesEndlessIgnoredThinkingStream(t *testing.T) {
	p := &auxiliaryLimitProvider{}
	text, usage, done, err := distillRequestCall(context.Background(), p, provider.RouteTarget{}, provider.Request{})
	if text != "" || usage != (provider.Usage{}) || done {
		t.Fatalf("text = %q, usage = %+v, done = %v", text, usage, done)
	}
	assertAuxiliaryStreamLimit(t, p, err)
}
