package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/prefix"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

type localCapabilityThenSuccessProvider struct {
	target provider.RouteTarget
	calls  int
}

func (*localCapabilityThenSuccessProvider) Name() string { return "local-capability" }
func (p *localCapabilityThenSuccessProvider) Stream(context.Context, provider.RouteTarget, provider.Request) (provider.EventStream, error) {
	p.calls++
	if p.calls == 1 {
		return nil, &provider.CapabilityError{Target: p.target.ID(), Capability: "local request", Detail: "rejected before transport"}
	}
	return &budgetTestStream{events: []provider.Event{
		{Type: provider.EventTextDelta, Text: "ok"},
		{Type: provider.EventDone, StopReason: provider.StopEndTurn, Usage: provider.Usage{InputTokens: 1, OutputTokens: 1}},
	}}, nil
}
func (*localCapabilityThenSuccessProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}
func (*localCapabilityThenSuccessProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true}, nil
}

type budgetTestStream struct {
	events []provider.Event
	index  int
}

func (s *budgetTestStream) Next() (provider.Event, error) {
	if s.index >= len(s.events) {
		return provider.Event{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}
func (*budgetTestStream) Close() error { return nil }

// The bundled catalog is the fixture: the gate's behavior is asserted, not
// its exact dollars, so a price revision does not break these.
func pricedTarget(t *testing.T) (*catalog.Catalog, provider.RouteTarget) {
	t.Helper()
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	target := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"}
	if _, _, ok := cat.Lookup(target); !ok {
		t.Fatal("the bundled catalog no longer prices claude-opus-5; pick another fixture target")
	}
	return cat, target
}

func TestPreflightBoundIsAWorstCase(t *testing.T) {
	cat, target := pricedTarget(t)
	info, _, _ := cat.Lookup(target)

	small := preflightBound(info, 1_000)
	large := preflightBound(info, 100_000)
	if small <= 0 {
		t.Fatal("a priced target must produce a positive bound")
	}
	if large <= small {
		t.Errorf("bound did not grow with the prompt: %s then %s", small, large)
	}
}

func TestPreflightBoundIncludesExplicitOutputAboveCatalogDefault(t *testing.T) {
	cat, target := pricedTarget(t)
	info, _, _ := cat.Lookup(target)
	target.Params.MaxOutputTokens = info.MaxOutput + 10_000
	ordinary := preflightBound(info, 1_000)
	concrete := preflightBoundForTarget(info, target, 1_000)
	if concrete <= ordinary {
		t.Fatalf("concrete output bound = %s, catalog-only bound = %s", concrete, ordinary)
	}
}

func TestPreflightBoundUsesAdaptiveWireAllowance(t *testing.T) {
	cat, target := pricedTarget(t)
	target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}
	info, _, _ := cat.Lookup(target)
	got := preflightBoundForTarget(info, target, 10_000)
	want := preflightBoundWithOutput(info, 10_000, 8_192)
	if got != want {
		t.Fatalf("adaptive bound = %s, want the 8192-token wire allowance %s", got, want)
	}
}

func TestOneShotBudgetUsesByteLevelHardBound(t *testing.T) {
	cat, target := pricedTarget(t)
	info, _, _ := cat.Lookup(target)
	req := provider.Request{Messages: []provider.Message{
		provider.UserText(strings.Repeat("🧪", 2_000)),
	}}
	floorBound := preflightBoundForTarget(info, target, prefix.RequestTokens(req))
	hardBound := preflightBoundForTarget(info, target, prefix.RequestTokenCeiling(req))
	if hardBound <= floorBound {
		t.Fatalf("hard bound = %s, floor bound = %s", hardBound, floorBound)
	}

	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), target.ID(), cat.Revision)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	budget := &budgetState{}
	budget.set(floorBound + (hardBound-floorBound)/2)
	if _, err := beginMeteredCall(budget, cat, sess, target, req, session.UsagePurposeAdvisor); err == nil {
		t.Fatal("one-shot call was admitted by the chars/4 floor instead of the byte-level hard bound")
	}
	if reserve := sess.State().RetryReserveMicroUSD; reserve != 0 {
		t.Fatalf("refused call left a durable reservation: %d", reserve)
	}
}

func TestBudgetGateRefusesAndClears(t *testing.T) {
	cat, target := pricedTarget(t)
	bs := &budgetState{}
	spent := catalog.Money(0)
	gate := budgetGate(bs, cat,
		func() provider.RouteTarget { return target },
		func() catalog.Money { return spent },
		func() string { return "session-a" })

	if err := gate.before(50_000, 1); err != nil {
		t.Fatalf("no ceiling set, but the gate refused: %v", err)
	}

	bs.set(1 * catalog.MicroUSD)
	err := gate.before(50_000, 1)
	if err == nil {
		t.Fatal("a one-micro-dollar ceiling let a 50k-token call through")
	}
	if !strings.Contains(err.Error(), "/budget") {
		t.Errorf("refusal %q does not say how to raise the ceiling", err)
	}

	bs.set(1_000 * catalog.USD)
	if err := gate.before(50_000, 1); err != nil {
		t.Errorf("a thousand-dollar ceiling refused a single call: %v", err)
	}

	// Spend eats the headroom: the same ceiling refuses once spent nears it.
	bs.set(2 * catalog.USD)
	spent = 2 * catalog.USD
	if err := gate.before(1_000, 1); err == nil {
		t.Error("a session at its ceiling was allowed another call")
	}
}

func TestBudgetGatePassesUnpricedTargets(t *testing.T) {
	cat, _ := pricedTarget(t)
	bs := &budgetState{}
	bs.set(1 * catalog.MicroUSD)
	gate := budgetGate(bs, cat,
		func() provider.RouteTarget {
			return provider.RouteTarget{Provider: "nobody", Surface: "nowhere", ModelID: "unknown"}
		},
		func() catalog.Money { return 0 },
		func() string { return "session-a" })
	if err := gate.before(1_000_000, 1); err != nil {
		t.Errorf("a ceiling cannot govern what has no price, but the gate refused: %v", err)
	}
}

func TestBudgetBlocksMoveNamesTheReason(t *testing.T) {
	cat, target := pricedTarget(t)
	bs := &budgetState{}
	dest := config.Tier{ID: "t3", Target: target}

	if _, blocked := budgetBlocksMove(bs, cat, dest, "session-a", 0, 50_000); blocked {
		t.Fatal("no ceiling, but the move was blocked")
	}

	bs.set(1 * catalog.MicroUSD)
	reason, blocked := budgetBlocksMove(bs, cat, dest, "session-a", 0, 50_000)
	if !blocked {
		t.Fatal("a one-micro-dollar ceiling allowed an escalation onto a priced rung")
	}
	if !strings.Contains(reason, "t3") || !strings.Contains(reason, "ceiling") {
		t.Errorf("reason %q does not name the rung and the ceiling", reason)
	}
}

func TestFailedAttemptDebtPersistsAcrossLaterModelCalls(t *testing.T) {
	cat, target := pricedTarget(t)
	info, _, _ := cat.Lookup(target)
	bound := preflightBoundForTarget(info, target, 10_000)
	bs := &budgetState{}
	bs.set(bound*2 - 1)
	scope := func() string { return "session-a" }
	first := budgetGate(bs, cat,
		func() provider.RouteTarget { return target },
		func() catalog.Money { return 0 }, scope)

	if err := first.before(10_000, 1); err != nil {
		t.Fatalf("first attempt was refused: %v", err)
	}
	if err := first.result(10_000, 1, session.Usage{}, errors.New("stream dropped")); err != nil {
		t.Fatal(err)
	}
	if got := bs.retryDebtFor(scope()); got != bound {
		t.Fatalf("retry debt = %s, want %s", got, bound)
	}

	// A fresh guard models a later call (or another loop such as a delegate).
	// The failed attempt remains charged against this session's hard ceiling.
	later := budgetGate(bs, cat,
		func() provider.RouteTarget { return target },
		func() catalog.Money { return 0 }, scope)
	if err := later.before(10_000, 1); err == nil || !strings.Contains(err.Error(), "reserved for failed attempts") {
		t.Fatalf("later call ignored retry debt: %v", err)
	}

	other := budgetGate(bs, cat,
		func() provider.RouteTarget { return target },
		func() catalog.Money { return 0 },
		func() string { return "session-b" })
	if err := other.before(10_000, 1); err != nil {
		t.Fatalf("one session's retry debt leaked into another: %v", err)
	}
}

func TestSuccessfulAttemptAddsNoRetryDebt(t *testing.T) {
	cat, target := pricedTarget(t)
	bs := &budgetState{}
	guard := budgetGate(bs, cat,
		func() provider.RouteTarget { return target },
		func() catalog.Money { return 0 },
		func() string { return "session-a" })
	if err := guard.before(10_000, 1); err != nil {
		t.Fatal(err)
	}
	if err := guard.result(10_000, 1, session.Usage{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := bs.retryDebtFor("session-a"); got != 0 {
		t.Fatalf("successful attempt reserved %s", got)
	}
}

func TestConcurrentRetriesCannotSpendTheSameHeadroom(t *testing.T) {
	cat, target := pricedTarget(t)
	info, _, _ := cat.Lookup(target)
	bound := preflightBoundForTarget(info, target, 10_000)
	bs := &budgetState{}
	bs.set(3 * bound)
	scope := func() string { return "shared-race" }
	newGuard := func() *budgetGuard {
		return budgetGate(bs, cat,
			func() provider.RouteTarget { return target },
			func() catalog.Money { return 0 }, scope)
	}
	a, b := newGuard(), newGuard()
	for _, guard := range []*budgetGuard{a, b} {
		if err := guard.before(10_000, 1); err != nil {
			t.Fatalf("first attempts should fit together: %v", err)
		}
	}
	if err := a.result(10_000, 1, session.Usage{}, errors.New("a failed")); err != nil {
		t.Fatal(err)
	}
	if err := b.result(10_000, 1, session.Usage{}, errors.New("b failed")); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, guard := range []*budgetGuard{a, b} {
		wg.Add(1)
		go func(guard *budgetGuard) {
			defer wg.Done()
			<-start
			results <- guard.before(10_000, 2)
		}(guard)
	}
	close(start)
	wg.Wait()
	close(results)
	admitted := 0
	for err := range results {
		if err == nil {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("concurrent retries admitted = %d, want exactly one under three-call headroom", admitted)
	}
}

func TestSuccessfulUsageCannotLeaveAStaleSpendAdmissionGap(t *testing.T) {
	cat, target := pricedTarget(t)
	info, _, _ := cat.Lookup(target)
	bound := preflightBoundForTarget(info, target, 10_000)
	bs := &budgetState{}
	bs.set(2*bound - 1)
	scope := func() string { return "shared" }
	spent := catalog.Money(0)
	a := budgetGate(bs, cat,
		func() provider.RouteTarget { return target },
		func() catalog.Money { return spent }, scope)
	// Model a concurrent caller that took its session-cost snapshot before A's
	// successful usage append, then reaches the budget mutex after A releases.
	bWithStaleSpend := budgetGate(bs, cat,
		func() provider.RouteTarget { return target },
		func() catalog.Money { return 0 }, scope)

	if err := a.before(10_000, 1); err != nil {
		t.Fatal(err)
	}
	spent = bound
	if err := a.result(10_000, 1, session.Usage{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := bWithStaleSpend.before(10_000, 1); err == nil {
		t.Fatal("stale spend snapshot reused headroom released by a successful concurrent call")
	}
}

func TestPrimaryBudgetGuardLoadsRetryDebtAfterResume(t *testing.T) {
	cat, target := pricedTarget(t)
	info, _, _ := cat.Lookup(target)
	bound := preflightBoundForTarget(info, target, 10_000)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), target.ID(), cat.Revision)
	if err != nil {
		t.Fatal(err)
	}
	firstLoop := &agent.Loop{Session: sess, Target: target}
	firstBudget := &budgetState{}
	firstGuard := primaryGate(firstBudget, firstLoop, cat)
	if err := firstGuard.before(10_000, 1); err != nil {
		t.Fatal(err)
	}
	if err := firstGuard.result(10_000, 1, session.Usage{}, errors.New("provider dropped the stream")); err != nil {
		t.Fatal(err)
	}
	if got := sess.State().RetryReserveMicroUSD; got != int64(bound) {
		t.Fatalf("durable retry reserve = %d, want %d", got, bound)
	}
	id := sess.ID()
	sess.Close()

	reopened, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loop := &agent.Loop{Session: reopened, Target: target}
	bs := &budgetState{}
	bs.set(2*bound - 1)
	guard := primaryGate(bs, loop, cat)
	if err := guard.before(10_000, 1); err == nil || !strings.Contains(err.Error(), "reserved for failed attempts") {
		t.Fatalf("resumed guard forgot durable retry debt: %v", err)
	}
}

func TestPrimaryGuardPersistsPendingAttemptBeforeProviderSend(t *testing.T) {
	cat, target := pricedTarget(t)
	info, _, _ := cat.Lookup(target)
	bound := preflightBoundForTarget(info, target, 10_000)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), target.ID(), cat.Revision)
	if err != nil {
		t.Fatal(err)
	}
	loop := &agent.Loop{Session: sess, Target: target}
	guard := primaryGate(&budgetState{}, loop, cat)
	if err := guard.before(10_000, 1); err != nil {
		t.Fatal(err)
	}
	if got := sess.State().RetryReserveMicroUSD; got != int64(bound) {
		t.Fatalf("provider could be called before durable reserve: got %d, want %d", got, bound)
	}
	id := sess.ID()
	sess.Close() // model a process death before any result callback
	reopened, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.State().RetryReserveMicroUSD; got != int64(bound) {
		t.Fatalf("restart forgot unresolved pending attempt: got %d, want %d", got, bound)
	}
}

func TestUnissuedCancellationClearsDurableBudgetReservation(t *testing.T) {
	cat, target := pricedTarget(t)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), target.ID(), cat.Revision)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	registry, err := tools.NewRegistry(t.TempDir(), execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	client := &racedProvider{turns: []racedTurn{racedText("must never be sent")}}
	loop := &agent.Loop{
		Provider: client, Target: target, Tools: registry,
		Perms:   permission.NewEngine(permission.ModeDefault, execution.Capability{}),
		Session: sess, Catalog: cat,
	}
	guard := primaryGate(&budgetState{}, loop, cat)
	ctx, cancel := context.WithCancel(context.Background())
	loop.Budget = func(tokens, attempt int) error {
		if err := guard.before(tokens, attempt); err != nil {
			return err
		}
		cancel() // after durable admission, before Provider.Stream
		return nil
	}
	loop.BudgetResult = guard.result

	err = loop.Turn(ctx, "hello")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("turn error = %v, want context.Canceled", err)
	}
	if client.calls != 0 {
		t.Fatalf("cancelled-before-send provider calls = %d, want zero", client.calls)
	}
	if reserve := sess.State().RetryReserveMicroUSD; reserve != 0 {
		t.Fatalf("unissued cancellation became permanent retry debt: %d", reserve)
	}
}

type cancelAfterIssueProvider struct {
	cancel context.CancelFunc
	calls  int
}

func (*cancelAfterIssueProvider) Name() string { return "cancel-after-issue" }
func (p *cancelAfterIssueProvider) Stream(ctx context.Context, _ provider.RouteTarget, _ provider.Request) (provider.EventStream, error) {
	p.calls++
	p.cancel()
	return nil, ctx.Err()
}
func (*cancelAfterIssueProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}
func (*cancelAfterIssueProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true}, nil
}

func TestIssuedCancellationRetainsDurableBudgetReservation(t *testing.T) {
	cat, target := pricedTarget(t)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), target.ID(), cat.Revision)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	registry, err := tools.NewRegistry(t.TempDir(), execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	client := &cancelAfterIssueProvider{cancel: cancel}
	loop := &agent.Loop{
		Provider: client, Target: target, Tools: registry,
		Perms:   permission.NewEngine(permission.ModeDefault, execution.Capability{}),
		Session: sess, Catalog: cat,
	}
	guard := primaryGate(&budgetState{}, loop, cat)
	wireBudget(loop, guard)

	err = loop.Turn(ctx, "hello")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("turn error = %v, want context.Canceled", err)
	}
	if client.calls != 1 {
		t.Fatalf("issued cancellation provider calls = %d, want one", client.calls)
	}
	if reserve := sess.State().RetryReserveMicroUSD; reserve <= 0 {
		t.Fatalf("issued cancellation lost its conservative retry debt: %d", reserve)
	}
}

func TestLocalCapabilityErrorReleasesDurableBudgetReservation(t *testing.T) {
	cat, target := pricedTarget(t)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), target.ID(), cat.Revision)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	registry, err := tools.NewRegistry(t.TempDir(), execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	client := &localCapabilityThenSuccessProvider{target: target}
	loop := &agent.Loop{
		Provider: client, Target: target, Tools: registry,
		Perms:   permission.NewEngine(permission.ModeDefault, execution.Capability{}),
		Session: sess, Catalog: cat,
	}
	state := &budgetState{}
	state.set(10 * catalog.USD)
	wireBudget(loop, primaryGate(state, loop, cat))

	if err := loop.Turn(context.Background(), "first"); err == nil {
		t.Fatal("local capability failure unexpectedly completed")
	}
	if reserve := sess.State().RetryReserveMicroUSD; reserve != 0 {
		t.Fatalf("unissued local capability failure became retry debt: %d", reserve)
	}
	if err := loop.Turn(context.Background(), "second"); err != nil {
		t.Fatalf("released reservation did not preserve headroom for the next valid call: %v", err)
	}
	if client.calls != 2 {
		t.Fatalf("provider calls = %d, want one local failure and one successful call", client.calls)
	}
}

func TestOneShotLocalCapabilityErrorReleasesDurableBudgetReservation(t *testing.T) {
	cat, target := pricedTarget(t)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), target.ID(), cat.Revision)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	request := provider.Request{Messages: []provider.Message{provider.UserText("summarize")}}
	finish, err := beginMeteredCall(&budgetState{}, cat, sess, target, request, session.UsagePurposeCompact)
	if err != nil {
		t.Fatal(err)
	}
	callErr := &provider.CapabilityError{Target: target.ID(), Capability: "local request", Detail: "rejected before transport"}
	if err := finish(provider.Usage{}, callErr); err != nil {
		t.Fatal(err)
	}
	if reserve := sess.State().RetryReserveMicroUSD; reserve != 0 {
		t.Fatalf("one-shot local failure became retry debt: %d", reserve)
	}
}

func TestBudgetGuardDistinguishesExplicitFreeFromMissingPaidBound(t *testing.T) {
	cat := catalogWithLocalModels(t,
		localModelSpec{name: "pricing-gap", contextWindow: 10_000, inputPerMTok: "1", outputPerMTok: "1", priceMaxInput: 500},
		localModelSpec{name: "explicit-free", contextWindow: 10_000, inputPerMTok: "0", outputPerMTok: "0"},
	)
	check := func(model string) error {
		target := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: model}
		return budgetGate(&budgetState{}, cat, func() provider.RouteTarget { return target },
			func() catalog.Money { return 0 }, func() string { return "scope" }).before(600, 1)
	}
	if err := check("pricing-gap"); err == nil || !errors.Is(err, errBudgetUnavailable) ||
		!strings.Contains(err.Error(), "no positive conservative cost bound") {
		t.Fatalf("known paid target without a conservative price was admitted: %v", err)
	}
	if err := check("explicit-free"); err != nil {
		t.Fatalf("explicit all-zero per-token target was refused: %v", err)
	}
}

func TestPrimaryGuardRefusesSendWhenPendingAppendFails(t *testing.T) {
	cat, target := pricedTarget(t)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), target.ID(), cat.Revision)
	if err != nil {
		t.Fatal(err)
	}
	loop := &agent.Loop{Session: sess, Target: target}
	guard := primaryGate(&budgetState{}, loop, cat)
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	err = guard.before(10_000, 1)
	if err == nil || !strings.Contains(err.Error(), "before send") {
		t.Fatalf("pre-send append failure = %v, want provider attempt refused", err)
	}
	if got := sess.State().RetryReserveMicroUSD; got != 0 {
		t.Fatalf("failed pending append mutated live state: %d", got)
	}
}

func TestPrimaryGuardSettlementFailureRetainsReservationAfterUsage(t *testing.T) {
	cat, target := pricedTarget(t)
	info, _, _ := cat.Lookup(target)
	bound := preflightBoundForTarget(info, target, 10_000)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), target.ID(), cat.Revision)
	if err != nil {
		t.Fatal(err)
	}
	loop := &agent.Loop{Session: sess, Target: target}
	bs := &budgetState{}
	guard := primaryGate(bs, loop, cat)
	if err := guard.before(10_000, 1); err != nil {
		t.Fatal(err)
	}
	usage := session.Usage{CostMicroUSD: int64(bound / 4)}
	if err := sess.AppendUsage(usage); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	if err := guard.result(10_000, 1, usage, nil); err == nil || !strings.Contains(err.Error(), "settling provider attempt") {
		t.Fatalf("settlement on closed log = %v, want durable append failure", err)
	}
	state := sess.State()
	if state.RetryReserveMicroUSD != int64(bound) || state.CostMicroUSD != usage.CostMicroUSD {
		t.Fatalf("successful response was orphaned after settlement failure: %+v", state)
	}
	if got := bs.retryDebtFor(sess.ID()); got != bound {
		t.Fatalf("in-memory admission forgot failed settlement: %s, want %s", got, bound)
	}
}

func TestDurableConcurrentAdmissionIsAtomic(t *testing.T) {
	cat, target := pricedTarget(t)
	info, _, _ := cat.Lookup(target)
	bound := preflightBoundForTarget(info, target, 10_000)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), target.ID(), cat.Revision)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	loopA := &agent.Loop{Session: sess, Target: target}
	loopB := &agent.Loop{Session: sess, Target: target}
	bs := &budgetState{}
	bs.set(bound)
	a, b := primaryGate(bs, loopA, cat), primaryGate(bs, loopB, cat)

	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, guard := range []*budgetGuard{a, b} {
		wg.Add(1)
		go func(guard *budgetGuard) {
			defer wg.Done()
			<-start
			results <- guard.before(10_000, 1)
		}(guard)
	}
	close(start)
	wg.Wait()
	close(results)
	admitted := 0
	for err := range results {
		if err == nil {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("durable concurrent admissions = %d, want exactly one", admitted)
	}
	if got := sess.State().RetryReserveMicroUSD; got != int64(bound) {
		t.Fatalf("durable pending reserve = %d, want one bound %d", got, bound)
	}
}

func TestOneShotMeterPersistsBeforeSendAndSettlesRealUsage(t *testing.T) {
	cat, target := pricedTarget(t)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), target.ID(), cat.Revision)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	req := provider.Request{Messages: []provider.Message{provider.UserText("summarize this paid request")}}
	finish, err := beginMeteredCall(&budgetState{}, cat, sess, target, req, session.UsagePurposeCompact)
	if err != nil {
		t.Fatal(err)
	}
	if got := sess.State().RetryReserveMicroUSD; got <= 0 {
		t.Fatalf("one-shot provider could send before durable WAL: reserve=%d", got)
	}

	usage := provider.Usage{InputTokens: 100, OutputTokens: 20}
	wantCost, _, ok := func() (catalog.Money, catalog.PriceBand, bool) {
		info, _, found := cat.Lookup(target)
		if !found {
			return 0, catalog.PriceBand{}, false
		}
		return info.Cost(usage)
	}()
	if !ok {
		t.Fatal("priced fixture did not price one-shot usage")
	}
	if err := finish(usage, nil); err != nil {
		t.Fatal(err)
	}
	state := sess.State()
	if state.RetryReserveMicroUSD != 0 || state.CostMicroUSD != int64(wantCost) {
		t.Fatalf("one-shot settlement = %+v, want cost %d and no reserve", state, wantCost)
	}
	if state.Calls != 1 || state.Usage != usage {
		t.Fatalf("one-shot usage was not recorded as the real provider call: %+v", state)
	}
	recorded, err := session.ReadUsages(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 1 || recorded[0].Purpose != session.UsagePurposeCompact || recorded[0].CallID == "" {
		t.Fatalf("one-shot correlation metadata = %+v", recorded)
	}
}

func TestOneShotMeterKeepsFailedAttemptDebtAndHonorsCeiling(t *testing.T) {
	cat, target := pricedTarget(t)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), target.ID(), cat.Revision)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	req := provider.Request{Messages: []provider.Message{provider.UserText("distill this paid request")}}
	bs := &budgetState{}
	finish, err := beginMeteredCall(bs, cat, sess, target, req, session.UsagePurposeCompact)
	if err != nil {
		t.Fatal(err)
	}
	bound := sess.State().RetryReserveMicroUSD
	if err := finish(provider.Usage{}, errors.New("stream dropped")); err != nil {
		t.Fatal(err)
	}
	if got := sess.State().RetryReserveMicroUSD; got != bound || got <= 0 {
		t.Fatalf("failed one-shot debt = %d, want full bound %d", got, bound)
	}

	bs.set(1 * catalog.MicroUSD)
	if _, err := beginMeteredCall(bs, cat, sess, target, req, session.UsagePurposeCompact); err == nil || !strings.Contains(err.Error(), "stopped before") {
		t.Fatalf("one-shot call bypassed hard ceiling: %v", err)
	}
}
