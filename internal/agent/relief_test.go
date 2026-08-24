package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
)

// busyProvider fails every attempt with a status the loop is willing to retry,
// which is the shape another target might not share.
type busyProvider struct{ calls int }

func (p *busyProvider) Name() string { return "busy" }

func (p *busyProvider) Stream(context.Context, provider.RouteTarget, provider.Request) (provider.EventStream, error) {
	p.calls++
	return nil, &provider.APIError{StatusCode: 429, Body: "slow down"}
}

func (p *busyProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}

func (p *busyProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true}, nil
}

// Retryable to the last attempt is typed rather than flattened into the
// generic provider failure, because only a typed error lets a surface tell
// "this one is busy" from "this request is wrong".
func TestExhaustedRetriesAreTypedAsAvailability(t *testing.T) {
	h := newHarness(t, permission.ModeDefault)
	busy := &busyProvider{}
	h.loop.Provider = busy
	h.loop.MaxAttempts = 2

	err := h.loop.Turn(context.Background(), "hello")

	var availability *AvailabilityError
	if !errors.As(err, &availability) {
		t.Fatalf("err = %v, want an AvailabilityError", err)
	}
	if !errors.Is(err, ErrProviderCall) {
		t.Error("the typed error stopped reading as a provider call failure")
	}
	if availability.Attempts != 2 {
		t.Errorf("attempts = %d, want the attempts actually spent", availability.Attempts)
	}
}

// A failure no other target would answer differently stays exactly what it was.
func TestANonRetryableFailureIsNotAnAvailabilityError(t *testing.T) {
	h := newHarness(t, permission.ModeDefault)
	h.loop.Provider = &refusingProvider{}

	err := h.loop.Turn(context.Background(), "hello")

	var availability *AvailabilityError
	if errors.As(err, &availability) {
		t.Errorf("err = %v, which invites a substitution that cannot help", err)
	}
}

// refusingProvider fails with a status the loop will not retry.
type refusingProvider struct{}

func (p *refusingProvider) Name() string { return "refusing" }

func (p *refusingProvider) Stream(context.Context, provider.RouteTarget, provider.Request) (provider.EventStream, error) {
	return nil, &provider.APIError{StatusCode: 400, Body: "malformed"}
}

func (p *refusingProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}

func (p *refusingProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true}, nil
}

// The two refusals a different target might not make are recognized, and
// nothing else is: everything else is the request being wrong, which no rung
// fixes.
func TestOnlyTheTwoAnswerableRefusalsAskForRelief(t *testing.T) {
	if _, ok := reliefReasonFor(&ContextWindowError{}); !ok {
		t.Error("a context refusal did not ask for relief")
	}
	if reason, _ := reliefReasonFor(&ContextWindowError{}); reason != ReliefContext {
		t.Error("a context refusal asked for the wrong kind of relief")
	}
	if _, ok := reliefReasonFor(&AvailabilityError{}); !ok {
		t.Error("an availability failure did not ask for relief")
	}
	if reason, _ := reliefReasonFor(&AvailabilityError{}); reason != ReliefAvailability {
		t.Error("an availability failure asked for the wrong kind of relief")
	}
	if _, ok := reliefReasonFor(errors.New("the tool schema is wrong")); ok {
		t.Error("an ordinary failure asked for a rung that cannot help it")
	}
	// Wrapped the way the loop wraps them, since that is how a surface meets them.
	if _, ok := reliefReasonFor(providerCallError(&AvailabilityError{})); !ok {
		t.Error("a wrapped availability failure was not recognized")
	}
}

// The turn survives a refusal the ladder can answer, on the binding the
// surface hands back.
func TestAReliefBindingTakesOverTheTurn(t *testing.T) {
	h := newHarness(t, permission.ModeDefault, textTurn("finished elsewhere"))
	busy := &busyProvider{}
	h.loop.Provider = busy
	h.loop.MaxAttempts = 1
	noteBeforeCall := false
	h.provider.beforeStream = func(_ int) {
		h.obs.mu.Lock()
		defer h.obs.mu.Unlock()
		noteBeforeCall = len(h.obs.notices) == 1 && h.obs.notices[0] == "warn: exact fallback substitution"
	}

	roomy := provider.RouteTarget{Provider: "scripted", Surface: "local", ModelID: "roomy"}
	asked := 0
	h.loop.Relief = func(_ context.Context, reason ReliefReason, _ error) (Binding, string, error) {
		asked++
		if reason != ReliefAvailability {
			t.Errorf("reason = %s, want availability", reason)
		}
		return Binding{Provider: h.provider, Target: roomy}, "exact fallback substitution", nil
	}

	if err := h.loop.Turn(context.Background(), "hello"); err != nil {
		t.Fatalf("the turn did not survive a substitution: %v", err)
	}
	if asked != 1 {
		t.Errorf("relief was asked %d times, want once", asked)
	}
	if h.loop.Binding().Target.ID() != roomy.ID() {
		t.Errorf("bound target = %s, want the rung that took over", h.loop.Binding().Target.ID())
	}
	if !noteBeforeCall {
		t.Fatal("the relieved provider was called before its exact substitution note was rendered")
	}
}

// A surface that cannot find a rung leaves the original failure standing
// rather than replacing it with a less useful one.
func TestARefusedReliefKeepsTheOriginalFailure(t *testing.T) {
	h := newHarness(t, permission.ModeDefault)
	h.loop.Provider = &busyProvider{}
	h.loop.MaxAttempts = 1
	h.loop.Relief = func(context.Context, ReliefReason, error) (Binding, string, error) {
		return Binding{}, "", errors.New("no rung could take this over")
	}

	err := h.loop.Turn(context.Background(), "hello")
	var availability *AvailabilityError
	if !errors.As(err, &availability) {
		t.Fatalf("err = %v, want the original availability failure", err)
	}
	if strings.Contains(err.Error(), "no rung could take this over") {
		t.Error("the surface's own refusal replaced the failure it was asked about")
	}
}

// Relief is bounded, or an unanswerable ladder is walked at a probe and a
// budget check per rung until the round budget runs out.
func TestReliefIsBoundedPerTurn(t *testing.T) {
	h := newHarness(t, permission.ModeDefault)
	h.loop.Provider = &busyProvider{}
	h.loop.MaxAttempts = 1

	calls := 0
	h.loop.Relief = func(context.Context, ReliefReason, error) (Binding, string, error) {
		calls++
		return Binding{Provider: &busyProvider{}, Target: h.loop.Target}, "", nil
	}

	if err := h.loop.Turn(context.Background(), "hello"); err == nil {
		t.Fatal("a ladder that never answers reported success")
	}
	if calls != maxReliefsPerTurn {
		t.Errorf("relief ran %d times, want the %d bound", calls, maxReliefsPerTurn)
	}
}

// A surface with no relief configured behaves exactly as it did before the
// hook existed.
func TestNoReliefLeavesTheFailureUnchanged(t *testing.T) {
	h := newHarness(t, permission.ModeDefault)
	h.loop.Provider = &busyProvider{}
	h.loop.MaxAttempts = 1

	err := h.loop.Turn(context.Background(), "hello")
	var availability *AvailabilityError
	if !errors.As(err, &availability) {
		t.Fatalf("err = %v, want the plain availability failure", err)
	}
}

// checkContext is what raises the refusal a roomier rung answers, so the two
// have to agree about which target is too small.
func TestContextRefusalNamesTheTargetAndTheNumbers(t *testing.T) {
	h := newHarness(t, permission.ModeDefault)
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	h.loop.Catalog = cat
	h.loop.Target = provider.RouteTarget{
		Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5",
	}

	refusal := h.loop.checkContext(h.loop.Binding(), 1<<20)
	var window *ContextWindowError
	if !errors.As(refusal, &window) {
		t.Fatalf("refusal = %v, want a ContextWindowError", refusal)
	}
	if window.Window == 0 || window.ReservedOutput == 0 {
		t.Errorf("refusal = %+v, which does not carry the numbers a surface picks a rung with", window)
	}
	if reason, ok := reliefReasonFor(refusal); !ok || reason != ReliefContext {
		t.Errorf("the refusal did not ask for context relief")
	}
}
