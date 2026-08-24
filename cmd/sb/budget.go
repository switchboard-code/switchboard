package main

// The /budget ceiling. §15 is specific about what a hard budget is checked
// against: a conservative preflight bound, not the expectation, because a
// turn affordable on average is not a turn under a ceiling. The check runs
// in three places — the router refuses rungs whose upper bound could cross
// it, the escalation policy cannot move onto one, and the loop stops before
// the call that would — and all three price the same way, through the §6.4
// estimator with its measured upward widening.
//
// The ceiling is dollars, and only dollars. A local rung consumes nothing
// scarce and a plan rung consumes quota; neither is governed, because the
// three meterings are never collapsed (§4).

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/costmodel"
	"github.com/switchboard-code/switchboard/internal/prefix"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

var errBudgetUnavailable = errors.New("budget prevents provider call")

// budgetState holds the ceiling behind a lock because two goroutines meet
// here: the loop reads it before every model call, and the UI writes it when
// /budget changes mid-turn — which is exactly how a runaway turn gets reined
// in without waiting for it to finish.
type budgetState struct {
	mu          sync.Mutex
	ceiling     catalog.Money // zero means no ceiling
	retryDebt   map[string]catalog.Money
	inFlight    map[string]catalog.Money
	debtLoaded  map[string]bool
	spent       map[string]catalog.Money
	spentLoaded map[string]bool
}

func (b *budgetState) set(c catalog.Money) {
	b.mu.Lock()
	b.ceiling = c
	b.mu.Unlock()
}

func (b *budgetState) get() catalog.Money {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ceiling
}

// addRetryDebt remembers the conservative cost of a provider attempt that
// failed without usage. Providers may still bill such a request. The debt is
// scoped to the session whose ceiling admitted it, and survives subsequent
// model calls, retries, target changes, and temporary race/delegate loops.
func (b *budgetState) loadDebtLocked(scope string, persisted catalog.Money) {
	if b.retryDebt == nil {
		b.retryDebt = map[string]catalog.Money{}
	}
	if b.debtLoaded == nil {
		b.debtLoaded = map[string]bool{}
	}
	if !b.debtLoaded[scope] || persisted > b.retryDebt[scope] {
		b.retryDebt[scope] = persisted
		b.debtLoaded[scope] = true
	}
}

func (b *budgetState) syncRetryDebt(scope string, persisted catalog.Money) catalog.Money {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.loadDebtLocked(scope, persisted)
	return b.retryDebt[scope]
}

func (b *budgetState) retryDebtFor(scope string) catalog.Money {
	return b.syncRetryDebt(scope, 0)
}

func (b *budgetState) accounted(scope string, persisted, spent catalog.Money) catalog.Money {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.loadDebtLocked(scope, persisted)
	b.loadSpentLocked(scope, spent)
	return addMoney(b.spent[scope], b.retryDebt[scope])
}

func addMoney(values ...catalog.Money) catalog.Money {
	var total catalog.Money
	for _, value := range values {
		if value < 0 || value > catalog.Money(math.MaxInt64)-total {
			return catalog.Money(math.MaxInt64)
		}
		total += value
	}
	return total
}

func crossesMoneyCeiling(ceiling catalog.Money, values ...catalog.Money) bool {
	if ceiling <= 0 {
		return false
	}
	remaining := ceiling
	for _, value := range values {
		if value < 0 || value > remaining {
			return true
		}
		remaining -= value
	}
	return false
}

func (b *budgetState) loadSpentLocked(scope string, observed catalog.Money) {
	if b.spent == nil {
		b.spent = map[string]catalog.Money{}
	}
	if b.spentLoaded == nil {
		b.spentLoaded = map[string]bool{}
	}
	if !b.spentLoaded[scope] || observed > b.spent[scope] {
		b.spent[scope] = observed
		b.spentLoaded[scope] = true
	}
}

// finish replaces an in-flight reservation with durable pessimistic debt on
// failure, or releases it after successful usage has been appended.
func (b *budgetState) finishLocked(scope string, bound, spent catalog.Money, failed bool) {
	b.loadSpentLocked(scope, spent)
	if b.inFlight[scope] <= bound {
		delete(b.inFlight, scope)
	} else {
		b.inFlight[scope] -= bound
	}
	if failed {
		b.loadDebtLocked(scope, 0)
		b.retryDebt[scope] = addMoney(b.retryDebt[scope], bound)
	}
}

// preflightBound prices the §15 worst case for one call from an already
// conservative input-token ceiling: the whole request cold and the exact
// output allowance the adapter sends. TokensAreExact prevents the cost
// estimator from widening an upper bound a second time. Eligibility mirrors candidatesFor, because a
// target that places markers pays the write rate on a miss.
func preflightBound(info catalog.ModelInfo, contextTokens int) catalog.Money {
	return preflightBoundWithOutput(info, contextTokens, info.MaxOutput)
}

func preflightBoundForTarget(info catalog.ModelInfo, target provider.RouteTarget, contextTokens int) catalog.Money {
	return preflightBoundWithOutput(info, contextTokens, reservedOutputTokens(target, info))
}

func preflightBoundWithOutput(info catalog.ModelInfo, contextTokens, outputTokens int) catalog.Money {
	est := costmodel.Estimator{}.Turn(costmodel.Inputs{
		Info:           info,
		PrefixTokens:   contextTokens,
		OutputTokens:   outputTokens,
		Eligible:       info.Cache.UsageAccounting == catalog.AccountingSeparate,
		TokensAreExact: true,
	})
	return est.High
}

// budgetGate builds a loop's pre-call check. Target and spent are read at
// call time because both move under the gate: an escalation rebinds the
// loop's target, and every priced call raises the spend. An unpriced target
// passes — a ceiling cannot govern what has no price, and /budget says so
// when it is set.
type budgetGuard struct {
	state     *budgetState
	catalog   *catalog.Catalog
	target    func() provider.RouteTarget
	spent     func() catalog.Money
	scope     func() string
	persisted func() catalog.Money
	begin     func(catalog.Money) (string, error)
	settle    func(string, string, catalog.Money) error
	allowance func(provider.RouteTarget, int) int
	external  bool

	mu           sync.Mutex
	reservations map[int]budgetReservation
}

type budgetReservation struct {
	bound    catalog.Money
	ledgerID string
	durable  bool
}

func budgetGate(bs *budgetState, cat *catalog.Catalog, target func() provider.RouteTarget, spent func() catalog.Money, scope func() string) *budgetGuard {
	return &budgetGuard{state: bs, catalog: cat, target: target, spent: spent, scope: scope}
}

func (g *budgetGuard) withLedger(
	persisted func() catalog.Money,
	begin func(catalog.Money) (string, error),
	settle func(string, string, catalog.Money) error,
	external bool,
) *budgetGuard {
	g.persisted = persisted
	g.begin = begin
	g.settle = settle
	g.external = external
	return g
}

func (g *budgetGuard) withOutputAllowance(resolve func(provider.RouteTarget, int) int) *budgetGuard {
	g.allowance = resolve
	return g
}

func (g *budgetGuard) scopeID() string {
	if g.scope == nil {
		return ""
	}
	return g.scope()
}

func (g *budgetGuard) before(promptTokens, attempt int) error {
	target := g.target()
	info, _, ok := g.catalog.Lookup(target)
	if !ok {
		return nil
	}
	output := reservedOutputTokens(target, info)
	if g.allowance != nil {
		output = g.allowance(target, info.MaxOutput)
	}
	bound := preflightBoundWithOutput(info, promptTokens, output)
	if bound <= 0 {
		if info.Metering != catalog.Local && info.Metering != catalog.Plan && !info.Free() {
			return fmt.Errorf("%w: refusing priced provider attempt %d because the catalog produced no positive conservative cost bound", errBudgetUnavailable, attempt)
		}
		return nil
	}
	scope := g.scopeID()
	ceiling, observed, debt, inFlight, reservation, admitted, reserveErr := g.reserve(scope, bound)
	if reserveErr != nil {
		return fmt.Errorf("recording provider attempt %d before send: %w", attempt, reserveErr)
	}
	if !admitted {
		return fmt.Errorf("%w: stopped before provider attempt %d: %s spent, %s reserved for failed attempts or pending requests, %s reserved by in-flight attempts, "+
			"and this attempt could cost up to %s, crossing the %s ceiling; /budget raises or clears it",
			errBudgetUnavailable, attempt, observed, debt, inFlight, bound, ceiling)
	}
	g.mu.Lock()
	if g.reservations == nil {
		g.reservations = map[int]budgetReservation{}
	}
	g.reservations[attempt] = reservation
	g.mu.Unlock()
	return nil
}

func (g *budgetGuard) reserve(scope string, bound catalog.Money) (ceiling, observed, debt, inFlight catalog.Money, reservation budgetReservation, admitted bool, err error) {
	g.state.mu.Lock()
	defer g.state.mu.Unlock()
	persisted := catalog.Money(0)
	if g.persisted != nil {
		persisted = g.persisted()
	}
	spent := g.spent()
	g.state.loadDebtLocked(scope, persisted)
	g.state.loadSpentLocked(scope, spent)
	observed = g.state.spent[scope]
	debt = g.state.retryDebt[scope]
	if g.state.inFlight == nil {
		g.state.inFlight = map[string]catalog.Money{}
	}
	inFlight = g.state.inFlight[scope]
	ceiling = g.state.ceiling
	if crossesMoneyCeiling(ceiling, observed, debt, inFlight, bound) {
		return ceiling, observed, debt, inFlight, budgetReservation{}, false, nil
	}
	reservation.bound = bound
	if g.begin != nil {
		id, beginErr := g.begin(bound)
		if beginErr != nil {
			return ceiling, observed, debt, inFlight, budgetReservation{}, false, beginErr
		}
		reservation.ledgerID = id
		reservation.durable = true
		g.state.retryDebt[scope] = addMoney(g.state.retryDebt[scope], bound)
		return ceiling, observed, debt, inFlight, reservation, true, nil
	}
	g.state.inFlight[scope] += bound
	return ceiling, observed, debt, inFlight, reservation, true, nil
}

func (g *budgetGuard) result(_ int, attempt int, usage session.Usage, attemptErr error) error {
	g.mu.Lock()
	reservation, reserved := g.reservations[attempt]
	delete(g.reservations, attempt)
	g.mu.Unlock()
	if !reserved {
		return nil
	}
	failed := attemptErr != nil && provider.RequestIssued(attemptErr)
	scope := g.scopeID()
	g.state.mu.Lock()
	defer g.state.mu.Unlock()
	if !reservation.durable {
		g.state.finishLocked(scope, reservation.bound, g.spent(), failed)
		return nil
	}
	outcome := session.BudgetOutcomeSucceeded
	charge := catalog.Money(0)
	if failed {
		outcome = session.BudgetOutcomeFailed
	} else if g.external {
		charge = catalog.Money(usage.CostMicroUSD)
	}
	if g.settle == nil {
		return fmt.Errorf("durable budget attempt %s has no settlement writer", reservation.ledgerID)
	}
	if err := g.settle(reservation.ledgerID, outcome, charge); err != nil {
		// The pending record stays authoritative. In particular a successful
		// provider response is never allowed to make its pre-send reservation
		// disappear merely because the settlement append failed.
		g.state.loadSpentLocked(scope, g.spent())
		return fmt.Errorf("settling provider attempt %s: %w", reservation.ledgerID, err)
	}
	if !failed {
		g.state.retryDebt[scope] -= reservation.bound
		if g.state.retryDebt[scope] < 0 {
			g.state.retryDebt[scope] = 0
		}
	}
	g.state.loadSpentLocked(scope, g.spent())
	return nil
}

func wireBudget(loop *agent.Loop, guard *budgetGuard) {
	loop.Budget = guard.before
	loop.BudgetResult = guard.result
}

// primaryGate wires the gate to the primary loop: its own moving target,
// its own session's priced record — the same number /cost shows.
func primaryGate(bs *budgetState, loop *agent.Loop, cat *catalog.Catalog) *budgetGuard {
	guard := budgetGate(bs, cat,
		func() provider.RouteTarget { return loop.Binding().Target },
		func() catalog.Money { return catalog.Money(loop.Session.State().AccountedCostMicroUSD()) },
		func() string { return loop.Session.ID() }).withLedger(
		func() catalog.Money { return catalog.Money(loop.Session.State().RetryReserveMicroUSD) },
		func(amount catalog.Money) (string, error) { return loop.Session.BeginBudgetAttempt(int64(amount)) },
		func(id, outcome string, charge catalog.Money) error {
			return loop.Session.SettleBudgetAttempt(id, outcome, int64(charge))
		}, false)
	return guard.withOutputAllowance(func(target provider.RouteTarget, catalogMax int) int {
		if loop.OutputAllowance != nil {
			return loop.OutputAllowance(target, catalogMax)
		}
		return effectiveOutputTokenAllowance(loop.Binding().Provider, target, catalogMax)
	})
}

// beginMeteredCall gives one-shot model features (/compact, /learn, advisor)
// the same durable admission protocol as agent.Loop. The returned closure must
// be called exactly once with either EventDone usage or the call error.
func beginMeteredCall(bs *budgetState, cat *catalog.Catalog, sess *session.Session, target provider.RouteTarget, req provider.Request, purpose string, bound ...provider.Provider) (func(provider.Usage, error) error, error) {
	if bs == nil || cat == nil || sess == nil {
		return nil, fmt.Errorf("model call has no durable budget ledger")
	}
	switch purpose {
	case session.UsagePurposeCompact, session.UsagePurposeLearn, session.UsagePurposeAdvisor,
		session.UsagePurposeApproval, session.UsagePurposeAudit:
	default:
		return nil, fmt.Errorf("one-shot model call needs an explicit background purpose")
	}
	guard := budgetGate(bs, cat,
		func() provider.RouteTarget { return target },
		func() catalog.Money { return catalog.Money(sess.State().AccountedCostMicroUSD()) },
		func() string { return sess.ID() }).withLedger(
		func() catalog.Money { return catalog.Money(sess.State().RetryReserveMicroUSD) },
		func(amount catalog.Money) (string, error) { return sess.BeginBudgetAttempt(int64(amount)) },
		func(id, outcome string, charge catalog.Money) error {
			return sess.SettleBudgetAttempt(id, outcome, int64(charge))
		}, false)
	if len(bound) > 0 && bound[0] != nil {
		guard.withOutputAllowance(func(target provider.RouteTarget, catalogMax int) int {
			return effectiveOutputTokenAllowance(bound[0], target, catalogMax)
		})
	}
	tokens := prefix.RequestTokens(req)
	contextTokens := prefix.RequestTokenCeiling(req)
	if err := guard.before(contextTokens, 1); err != nil {
		return nil, err
	}
	started := time.Now()
	return func(usage provider.Usage, callErr error) error {
		if callErr != nil {
			return guard.result(tokens, 1, session.Usage{}, callErr)
		}
		record := session.Usage{Target: string(target.ID()), Usage: usage, Duration: time.Since(started), Attempts: 1, Purpose: purpose}
		if info, confidence, ok := cat.Lookup(target); ok {
			if cost, _, priced := info.Cost(usage); priced {
				record.CostMicroUSD = int64(cost)
				record.CatalogRevision = cat.Revision
				record.PriceConfidence = string(confidence)
			}
		}
		storedRecord, err := sess.AppendUsageRecord(record)
		if err != nil {
			// The pre-send attempt remains pending. Releasing it after a failed
			// usage append would make the known provider call vanish on restart.
			return err
		}
		record = storedRecord
		return guard.result(tokens, 1, record, nil)
	}, nil
}

// budgetBlocksMove answers whether an escalation may land on a rung. §8.3
// lets a quality trigger override a cost preference and never a hard
// ceiling, so a destination whose upper bound does not fit is refused with
// the reason, and the primary stays where it is.
func budgetBlocksMove(bs *budgetState, cat *catalog.Catalog, dest config.Tier, scope string, spent catalog.Money, promptTokens int) (string, bool) {
	ceiling := bs.get()
	if ceiling == 0 {
		return "", false
	}
	info, _, ok := cat.Lookup(dest.Target)
	if !ok {
		return "", false
	}
	bound := preflightBoundForTarget(info, dest.Target, promptTokens)
	debt := bs.retryDebtFor(scope)
	if crossesMoneyCeiling(ceiling, spent, debt, bound) {
		return fmt.Sprintf("a turn on %s could cost up to %s against %s already spent and %s reserved for failed attempts, crossing the %s ceiling",
			dest.ID, bound, spent, debt, ceiling), true
	}
	return "", false
}
