package approval

import (
	"context"
	"errors"
	"testing"

	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
)

type reviewerLimitProvider struct {
	ctx    context.Context
	stream *reviewerLimitStream
}

func (*reviewerLimitProvider) Name() string { return "reviewer-limit" }

func (p *reviewerLimitProvider) Stream(ctx context.Context, _ provider.RouteTarget, _ provider.Request) (provider.EventStream, error) {
	p.ctx = ctx
	p.stream = &reviewerLimitStream{ctx: ctx}
	return p.stream, nil
}

func (*reviewerLimitProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}

func (*reviewerLimitProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{}, nil
}

type reviewerLimitStream struct {
	ctx              context.Context
	next             int
	closes           int
	closeSawCanceled bool
}

func (s *reviewerLimitStream) Next() (provider.Event, error) {
	s.next++
	return provider.Event{Type: provider.EventThinkingDelta}, nil
}

func (s *reviewerLimitStream) Close() error {
	s.closes++
	s.closeSawCanceled = s.ctx.Err() != nil
	return nil
}

func TestReviewerCancelsAndClosesEndlessIgnoredThinkingStream(t *testing.T) {
	p := &reviewerLimitProvider{}
	m := &meterState{}
	r := &ModelReviewer{Provider: p, Target: target(), Identity: "t1", Meter: m.meter}

	result, err := r.Review(context.Background(), command())
	if !errors.Is(err, provider.ErrStreamLimit) {
		t.Fatalf("err = %v, want ErrStreamLimit", err)
	}
	if result.Decision == permission.ReviewAllow {
		t.Fatalf("limit refusal returned allow: %+v", result)
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
	if m.finish != 1 || !errors.Is(m.callErr, provider.ErrStreamLimit) {
		t.Fatalf("meter = %+v, want one limit-error settlement", m)
	}
}
