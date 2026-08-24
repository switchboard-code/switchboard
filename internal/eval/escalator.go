package eval

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/breakpoint"
	"github.com/switchboard-code/switchboard/internal/cachestate"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/prefix"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// escalator lets the routed arm change its target mid-task.
//
// This is the same wiring cmd/sb uses, and it is here rather than shared with it
// because the two want different things at the edges: the terminal renders every
// move for the user, and the harness only needs to count them. What must not
// differ is the policy, and that comes from the same package either way.
type escalator struct {
	sticky  *router.Sticky
	detect  *router.Detector
	ladder  []Arm
	catalog *catalog.Catalog

	loop   *agent.Loop
	inner  agent.Observer
	caches map[provider.RouteTargetID]*agent.Cache

	contextMu      sync.RWMutex
	contextWindows map[provider.RouteTargetID]int

	moves   int
	visited []provider.RouteTargetID

	routed RoutedArmFor
	budget *evalBudget
}

func (e *escalator) attach(loop *agent.Loop) {
	e.loop = loop
	e.inner = loop.Observer
	e.visited = []provider.RouteTargetID{loop.Target.ID()}
	e.caches = map[provider.RouteTargetID]*agent.Cache{loop.Target.ID(): loop.Cache}
	e.contextWindows = map[provider.RouteTargetID]int{}
	priorContextWindow := loop.ContextWindow
	loop.ContextWindow = func(target provider.RouteTarget) int {
		e.contextMu.RLock()
		window, ok := e.contextWindows[target.ID()]
		e.contextMu.RUnlock()
		if ok {
			return window
		}
		if priorContextWindow != nil {
			return priorContextWindow(target)
		}
		if e.catalog != nil {
			if info, _, found := e.catalog.Lookup(target); found {
				return info.ContextWindow
			}
		}
		return 0
	}
	e.budget = newEvalBudget(e.routed.Budgets, e.catalog, loop)
	e.budget.attach(loop, e.fidelityError)
	loop.SetObserver(e)
}

func (*escalator) fidelityError() error { return nil }

// finalTarget reports where the run ended up, which is what the cost was
// actually paid to. Reporting the opening choice would attribute an escalated
// run's spend to the rung it started on.
func (e *escalator) finalTarget(start Arm) provider.RouteTargetID {
	if e.loop == nil {
		return start.Target.ID()
	}
	return e.loop.Binding().Target.ID()
}

func (e *escalator) ThinkingDelta(text string) { e.inner.ThinkingDelta(text) }

func (e *escalator) TextDelta(text string) {
	e.inner.TextDelta(text)
	e.observe(e.detect.AssistantText(text))
}

func (e *escalator) ToolStart(call provider.ToolUse, req permission.Request) {
	e.inner.ToolStart(call, req)
	e.observe(e.detect.ToolCall(call.Name, call.Input))
}

func (e *escalator) ToolEnd(call provider.ToolUse, req permission.Request, res tools.Result, took time.Duration) {
	e.inner.ToolEnd(call, req, res, took)
	e.observe(e.detect.ToolResult(call.Name, strings.Join(req.Argv, " "), res.Content, res.IsError))
}

func (e *escalator) ToolBatchEnd(ctx context.Context) {
	e.inner.ToolBatchEnd(ctx)
	e.assess(ctx)
}

func (e *escalator) Notice(level, text string) { e.inner.Notice(level, text) }

func (e *escalator) TurnUsage(u session.Usage) {
	e.inner.TurnUsage(u)
	e.sticky.CallServed()
}

func (e *escalator) observe(signals []router.Signal) {
	for _, s := range signals {
		e.sticky.Observe(s)
	}
}

func (e *escalator) assess(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	move := e.sticky.Assess(len(e.ladder) - 1)
	if move.Direction == 0 {
		return
	}

	request := e.loop.Request(e.loop.Session.State().Messages)
	requirements := e.routed.Requirements
	requirements.NeedsTools = true
	requirements.NeedsVision = requirements.NeedsVision || requestNeedsVision(request)
	promptTokens := prefix.RequestTokens(request)
	contextTokens := prefix.RequestTokenCeiling(request)
	budgets := e.routed.Budgets
	if e.budget != nil {
		budgets = e.budget.remaining()
	}
	arm, _, err := e.routed.resolveTier(
		ctx, e.ladder[move.ToRank], move.ToRank, promptTokens, contextTokens, requirements, budgets)
	if err != nil {
		// Production treats an infeasible policy move as a stay: evidence may
		// prefer another rung, but it cannot weaken reachability, fallback,
		// capability, context, destination, or budget policy. Keeping Sticky
		// uncommitted preserves the current binding and lets the turn continue.
		e.inner.Notice("warn", fmt.Sprintf(
			"staying on %s: cannot prepare move from rank %d to %d: %v",
			e.loop.Binding().Target.Display(), move.FromRank, move.ToRank, err))
		return
	}
	cache := e.cacheFor(arm)
	if !e.sticky.Apply(move, func() {
		e.contextMu.Lock()
		e.contextWindows[arm.Target.ID()] = arm.resolvedContextWindow
		e.contextMu.Unlock()
		e.loop.Bind(agent.Binding{Provider: arm.Provider, Target: arm.Target, Cache: cache})
	}) {
		return
	}
	e.moves++
	e.visited = append(e.visited, arm.Target.ID())
}

func (e *escalator) cacheFor(arm Arm) *agent.Cache {
	if cache, ok := e.caches[arm.Target.ID()]; ok {
		return cache
	}
	var cache *agent.Cache
	if arm.CacheAware {
		if info, _, ok := e.catalog.Lookup(arm.Target); ok {
			cache = &agent.Cache{
				Manager: &breakpoint.Manager{Policy: info.Cache, Target: arm.Target.ID()},
				Tracker: cachestate.New(),
				Policy:  info.Cache,
				Target:  arm.Target.ID(),
			}
		}
	}
	e.caches[arm.Target.ID()] = cache
	return cache
}

// Visited is every target the run touched, in order. A routed run that only
// ever touched one is a fixed baseline, and the report says so rather than
// letting the arm names imply otherwise.
func (e *escalator) Visited() []provider.RouteTargetID { return e.visited }

// evalBudget is a run-local version of the production preflight ledger. It
// reserves the conservative maximum before every provider attempt, converts a
// failed attempt into pessimistic retry debt, and releases a successful
// reservation only after the agent has made the usage record durable.
type evalBudget struct {
	configured router.Budgets
	catalog    *catalog.Catalog
	loop       *agent.Loop

	mu           sync.Mutex
	failedDebt   catalog.Money
	reservations map[int]catalog.Money
}

func newEvalBudget(configured router.Budgets, cat *catalog.Catalog, loop *agent.Loop) *evalBudget {
	return &evalBudget{
		configured:   configured,
		catalog:      cat,
		loop:         loop,
		reservations: map[int]catalog.Money{},
	}
}

func (b *evalBudget) enabled() bool {
	return b != nil && (b.configured.MaxCostSet || b.configured.MaxCost > 0)
}

func (b *evalBudget) attach(loop *agent.Loop, fidelityFailure func() error) {
	if b == nil || loop == nil {
		return
	}
	priorBefore := loop.Budget
	priorResult := loop.BudgetResult
	loop.Budget = func(contextTokens, attempt int) error {
		if err := fidelityFailure(); err != nil {
			return fmt.Errorf("%w: %v", errRoutedFidelity, err)
		}
		if priorBefore != nil {
			if err := priorBefore(contextTokens, attempt); err != nil {
				return err
			}
		}
		return b.before(contextTokens, attempt)
	}
	loop.BudgetResult = func(promptTokens, attempt int, usage session.Usage, callErr error) error {
		b.finish(attempt, callErr != nil)
		if priorResult != nil {
			return priorResult(promptTokens, attempt, usage, callErr)
		}
		return nil
	}
}

func (b *evalBudget) before(contextTokens, attempt int) error {
	if !b.enabled() {
		return nil
	}
	if attempt < 1 || b.loop == nil || b.catalog == nil {
		return fidelityErrorf("budget preflight lacks a valid attempt, loop, or catalog")
	}
	binding := b.loop.Binding()
	info, _, ok := b.catalog.Lookup(binding.Target)
	if !ok {
		return fidelityErrorf("budget preflight cannot price target %s", binding.Target.Display())
	}
	bound := candidateForRequest(
		Arm{Name: "budget", Target: binding.Target, Provider: binding.Provider},
		0, info, contextTokens, contextTokens).CeilingCost
	if bound == 0 && info.Metering == catalog.PerToken && !info.Free() {
		return fidelityErrorf("budget preflight has no conservative price for target %s", binding.Target.Display())
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	accounted := b.accountedLocked()
	if existing := b.reservations[attempt]; existing > 0 {
		accounted -= existing
	}
	if accounted > b.configured.MaxCost || bound > b.configured.MaxCost-accounted {
		return fidelityErrorf(
			"target %s could cost up to %s with %s of the %s evaluation budget already accounted",
			binding.Target.Display(), bound, accounted, b.configured.MaxCost)
	}
	b.reservations[attempt] = bound
	return nil
}

func (b *evalBudget) finish(attempt int, failed bool) {
	if !b.enabled() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	bound := b.reservations[attempt]
	delete(b.reservations, attempt)
	if failed {
		b.failedDebt = addEvalMoney(b.failedDebt, bound)
	}
}

func (b *evalBudget) remaining() router.Budgets {
	if !b.enabled() {
		return b.configured
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	accounted := b.accountedLocked()
	if accounted >= b.configured.MaxCost {
		return router.Budgets{MaxCostSet: true}
	}
	return router.Budgets{MaxCost: b.configured.MaxCost - accounted, MaxCostSet: true}
}

func (b *evalBudget) accountedLocked() catalog.Money {
	accounted := b.failedDebt
	if b.loop != nil && b.loop.Session != nil {
		state := b.loop.Session.State()
		accounted = addEvalMoney(accounted, catalog.Money(state.AccountedCostMicroUSD()))
		accounted = addEvalMoney(accounted, catalog.Money(state.RetryReserveMicroUSD))
	}
	for _, reserved := range b.reservations {
		accounted = addEvalMoney(accounted, reserved)
	}
	return accounted
}

func addEvalMoney(a, b catalog.Money) catalog.Money {
	if a < 0 || b < 0 || a > catalog.MaxMoney-b {
		return catalog.MaxMoney
	}
	return a + b
}
