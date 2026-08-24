package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

type retryEntryProvider struct {
	sess     *session.Session
	delegate provider.Provider
	calls    int
	statuses []session.RetryIntentStatus
}

func (p *retryEntryProvider) Name() string { return "retry-entry" }

func (p *retryEntryProvider) Stream(ctx context.Context, target provider.RouteTarget, req provider.Request) (provider.EventStream, error) {
	p.calls++
	state := p.sess.State()
	if state.RetryIntent == nil {
		p.statuses = append(p.statuses, "")
	} else {
		p.statuses = append(p.statuses, state.RetryIntent.Status)
	}
	return p.delegate.Stream(ctx, target, req)
}

func (p *retryEntryProvider) CountTokens(ctx context.Context, target provider.RouteTarget, req provider.Request) (provider.TokenEstimate, error) {
	return p.delegate.CountTokens(ctx, target, req)
}

func (p *retryEntryProvider) Probe(ctx context.Context, target provider.RouteTarget) (provider.ProbeResult, error) {
	return p.delegate.Probe(ctx, target)
}

func prepareAgentRetry(t *testing.T, h *harness) (provider.Message, session.RetryIntent) {
	t.Helper()
	opening := provider.UserText("retry this exact opening")
	if err := h.sess.AppendMessage(opening); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Dir(filepath.Dir(h.sess.Path())))
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.ForkSessionForRetryStaged(h.sess, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Close() })
	intent, err := child.AppendRetryIntent(
		h.sess.ID(), 0, opening, "t1", string(h.loop.Target.ID()), strings.Repeat("a", 64),
	)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := child.PublishDurably()
	if err != nil || !outcome.Visible || !outcome.Durable {
		t.Fatalf("publishing retry child = %+v, %v", outcome, err)
	}
	if err := h.loop.BindSession(child); err != nil {
		t.Fatal(err)
	}
	h.sess = child
	return opening, intent
}

func TestRetryExecutionStartIsDurableAtProviderEntry(t *testing.T) {
	h := newHarness(t, permission.ModeDefault, textTurn("done"))
	opening, intent := prepareAgentRetry(t, h)
	entry := &retryEntryProvider{sess: h.sess, delegate: h.provider}
	h.loop.Bind(Binding{Provider: entry, Target: h.loop.Target})

	if err := h.loop.RetryTurnMessage(context.Background(), opening, intent.ID); err != nil {
		t.Fatal(err)
	}
	if entry.calls != 1 || len(entry.statuses) != 1 || entry.statuses[0] != session.RetryIntentStarted {
		t.Fatalf("provider entry = calls %d statuses %v, want one durable started boundary", entry.calls, entry.statuses)
	}
	state := h.sess.State()
	if state.RetryIntent == nil || state.RetryIntent.Status != session.RetryIntentStarted {
		t.Fatalf("retry state after provider return = %#v", state.RetryIntent)
	}
	if len(state.Messages) != 2 || state.Messages[0].RetryIntentID != intent.ID {
		t.Fatalf("retry messages = %#v", state.Messages)
	}
}

func TestRetryAdmissionFailureLeavesMarkedOpeningPendingForExactResume(t *testing.T) {
	h := newHarness(t, permission.ModeDefault, textTurn("done once"))
	opening, intent := prepareAgentRetry(t, h)
	entry := &retryEntryProvider{sess: h.sess, delegate: h.provider}
	h.loop.Bind(Binding{Provider: entry, Target: h.loop.Target})
	denied := errors.New("budget denied before provider entry")
	h.loop.Budget = func(int, int) error { return denied }

	if err := h.loop.RetryTurnMessage(context.Background(), opening, intent.ID); !errors.Is(err, denied) {
		t.Fatalf("retry admission error = %v", err)
	}
	state := h.sess.State()
	if entry.calls != 0 || state.RetryIntent == nil || state.RetryIntent.Status != session.RetryIntentPending ||
		len(state.Messages) != 1 || state.Messages[0].RetryIntentID != intent.ID {
		t.Fatalf("pre-provider failure state = calls %d intent %#v messages %#v", entry.calls, state.RetryIntent, state.Messages)
	}

	h.loop.Budget = nil
	if err := h.loop.ResumeRetryTurn(context.Background(), intent.ID); err != nil {
		t.Fatal(err)
	}
	state = h.sess.State()
	if entry.calls != 1 || len(state.Messages) != 2 || len(h.provider.requests) != 1 || len(h.provider.requests[0].Messages) != 1 {
		t.Fatalf("exact resume duplicated work: calls=%d messages=%d requests=%d request_messages=%d",
			entry.calls, len(state.Messages), len(h.provider.requests), len(h.provider.requests[0].Messages))
	}
}

func TestRetryStartFailureMakesZeroProviderCallsAndTransientRetriesStartOnce(t *testing.T) {
	t.Run("start append failure", func(t *testing.T) {
		h := newHarness(t, permission.ModeDefault, textTurn("must not run"))
		opening, intent := prepareAgentRetry(t, h)
		entry := &retryEntryProvider{sess: h.sess, delegate: h.provider}
		h.loop.Bind(Binding{Provider: entry, Target: h.loop.Target})
		h.loop.Budget = func(int, int) error {
			return h.sess.Close()
		}
		err := h.loop.RetryTurnMessage(context.Background(), opening, intent.ID)
		if err == nil || !strings.Contains(err.Error(), "saving retry execution-start boundary") {
			t.Fatalf("start failure = %v", err)
		}
		if entry.calls != 0 {
			t.Fatalf("provider called %d times after start WAL failure", entry.calls)
		}
	})

	t.Run("transient provider retry", func(t *testing.T) {
		h := newHarness(t, permission.ModeDefault,
			scriptTurn{startErr: &provider.APIError{StatusCode: 429, Body: "busy"}},
			textTurn("done"),
		)
		opening, intent := prepareAgentRetry(t, h)
		h.loop.MaxAttempts = 2
		if err := h.loop.RetryTurnMessage(context.Background(), opening, intent.ID); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(h.sess.Path())
		if err != nil {
			t.Fatal(err)
		}
		if h.provider.calls != 2 || strings.Count(string(raw), `"status":"started"`) != 1 {
			t.Fatalf("provider calls=%d started records=%d", h.provider.calls, strings.Count(string(raw), `"status":"started"`))
		}
	})
}
