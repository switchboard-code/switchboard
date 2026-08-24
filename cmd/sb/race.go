package main

// /race assembly: the same prompt, from the same prefix, on two rungs at
// once, judged by the user. §8.4's complaint about natural outcomes is that
// they are weak evidence — a clean completion says nothing about necessity —
// and its shadow-routing answer is gated on verifiers and sandboxes that do
// not exist yet. A race is the interactive form of the same counterfactual:
// both outcomes are independently judged by the person whose task it is,
// which is the strongest label class ordinary use can produce. The verdict
// is recorded and deliberately never consulted by routing; collecting the
// corpus is phase 2b's job, acting on it is gated behind phase 7.
//
// Each arm is a fork of the session (§12), so its messages are
// byte-identical to the prefix a provider may still hold warm: the arm on
// the sitting rung rides that prefix warm, and the challenger pays the cold
// read any first contact pays. The asymmetry is real and stays in the
// record, because hiding it would misprice the comparison.

import (
	"fmt"
	"time"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/prefix"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

// A raceArm is one branch of the trial: its rung, the client that probed
// for it, the forked session it runs in, and the loop assembled around
// them. base* is the forked prefix's accounting, so the arm's own spend is
// what grew past it.
type raceArm struct {
	tier   config.Tier
	client provider.Provider
	sess   *session.Session
	loop   *agent.Loop

	baseCost  int64
	baseUsage provider.Usage

	status  string // "completed", "error", "cancelled", "round_limit"
	failure string // the error text behind a status of "error"
	started time.Time
	wall    time.Duration
}

// record prices the arm's own work: the fork copied the prefix's usage
// records, so the branch total minus the base is what this arm added.
func (a *raceArm) record() session.RaceArm {
	state := a.sess.State()
	return session.RaceArm{
		Tier:         a.tier.ID,
		Target:       a.tier.Target.ID(),
		SessionID:    state.ID,
		Status:       a.status,
		Usage:        state.Usage.Sub(a.baseUsage),
		CostMicroUSD: state.CostMicroUSD - a.baseCost,
		WallTimeMS:   a.wall.Milliseconds(),
	}
}

// assembleRaceArm forks the session onto the arm's rung and builds its
// loop. The loop is the primary's in every byte that reaches the provider —
// the same system blocks, and a Branch of the same registry so the tool
// schemas render identically (§6.1) — and not in what it may do: the
// permission engine is a fresh one in plan mode, which denies every
// non-read effect outright before rules or remembered answers are
// consulted, whatever mode the session itself runs in. That is the §8.4
// isolation rule for counterfactual runs, enforced where no mode can route
// around it. The asker is deliberately nil: plan mode never asks for what
// it denies, and anything that somehow reached an ask would be refused with
// the reason rather than answered by a dialog the user thinks is about the
// session. Mutation is what the winner does after the pick.
func assembleRaceArm(app *tuiApp, tier config.Tier, client provider.Provider, obs agent.Observer) (*raceArm, error) {
	state := app.loop.Session.State()
	var sess *session.Session
	var err error
	if n := len(state.Messages); n > 0 {
		sess, err = app.store.ForkSessionOntoStaged(app.loop.Session, n, tier.Target.ID())
	} else {
		// The conversation prefix is empty, but its accounting lineage may not
		// be (for example, a first-turn retry whose routing failed). Carry it.
		sess, err = app.store.ForkSessionAccountingOntoStaged(app.loop.Session, tier.Target.ID())
	}
	if err != nil {
		return nil, err
	}
	if err := sess.MarkRaceBranchPending(state.ID); err != nil {
		_ = sess.CloseDiscardingStaged()
		return nil, err
	}

	branchState := sess.State()
	arm := &raceArm{
		tier:      tier,
		client:    client,
		sess:      sess,
		baseCost:  branchState.CostMicroUSD,
		baseUsage: branchState.Usage,
	}
	arm.loop = &agent.Loop{
		Provider: client,
		Target:   tier.Target,
		Tools: app.loop.Tools.Branch(map[string]string{
			"delegate": "delegate is unavailable in a race branch: an errand spawned here would outlive the pick; the branch that wins can delegate after it continues",
			"ask":      "ask is unavailable in a race branch: both arms answer unattended so they can be compared, and the pick at the end is the user's answer",
		}),
		Perms:           permission.NewEngineWithExecution(permission.ModePlan, app.loop.Tools.Execution()),
		Session:         sess,
		Catalog:         app.catalog,
		Cache:           cacheFor(tier.Target, app.catalog),
		System:          app.loop.System,
		Observer:        obs,
		Hooks:           app.loop.Hooks,
		ContextWindow:   app.loop.ContextWindow,
		OutputAllowance: app.loop.OutputAllowance,
	}
	if err := arm.loop.BindSession(sess); err != nil {
		_ = sess.CloseDiscardingStaged()
		return nil, fmt.Errorf("restore race branch context: %w", err)
	}
	return arm, nil
}

// raceGates wires the shared ceiling across both arms: each gate charges
// the pre-race session plus what both branches have added, so two arms
// cannot each spend up to the ceiling by not counting the other.
func raceGates(bs *budgetState, cat *catalog.Catalog, origin *session.Session, before session.State, a, b *raceArm) {
	spent := func() catalog.Money {
		return addMoney(
			catalog.Money(before.AccountedCostMicroUSD()),
			catalog.Money(a.sess.State().CostMicroUSD-a.baseCost),
			catalog.Money(b.sess.State().CostMicroUSD-b.baseCost))
	}
	scope := func() string { return before.ID }
	persisted := func() catalog.Money { return catalog.Money(origin.State().RetryReserveMicroUSD) }
	begin := func(amount catalog.Money) (string, error) {
		return origin.BeginBudgetAttempt(int64(amount))
	}
	settle := func(id, outcome string, charge catalog.Money) error {
		return origin.SettleBudgetAttempt(id, outcome, int64(charge))
	}
	// The pre-race session is the sole live ledger while both arms run. A
	// verdict later transfers one cumulative delta to a chosen branch; no
	// attempt is partially replicated across three logs.
	wireBudget(a.loop, budgetGate(bs, cat, func() provider.RouteTarget { return a.loop.Binding().Target }, spent, scope).withLedger(persisted, begin, settle, true).withOutputAllowance(a.loop.OutputAllowance))
	wireBudget(b.loop, budgetGate(bs, cat, func() provider.RouteTarget { return b.loop.Binding().Target }, spent, scope).withLedger(persisted, begin, settle, true).withOutputAllowance(b.loop.OutputAllowance))
}

// racePreflight refuses a race the ceiling cannot hold. §15's rule applied
// twice over: the arms run at once, so both upper bounds have to fit at
// once, and a race affordable arm-by-arm is not a race under a ceiling.
// Unpriced and non-dollar rungs pass the way they pass everywhere — a
// ceiling governs dollars only.
func racePreflight(bs *budgetState, cat *catalog.Catalog, before session.State,
	system []provider.Block, defs []provider.ToolDefinition, opening provider.Message,
	a, b config.Tier, allowances ...func(provider.RouteTarget, int) int) (string, bool) {
	tokens := prefix.RequestTokenCeiling(provider.ReplayRequest(provider.Request{
		System:   system,
		Tools:    defs,
		Messages: append(append([]provider.Message(nil), before.Messages...), opening),
	}))
	var bound catalog.Money
	for _, tier := range []config.Tier{a, b} {
		if info, _, ok := cat.Lookup(tier.Target); ok {
			output := reservedOutputTokens(tier.Target, info)
			if len(allowances) > 0 && allowances[0] != nil {
				output = allowances[0](tier.Target, info.MaxOutput)
			}
			armBound := preflightBoundWithOutput(info, tokens, output)
			if armBound <= 0 && info.Metering != catalog.Local && info.Metering != catalog.Plan && !info.Free() {
				return fmt.Sprintf("%s has no positive conservative cost bound in the catalog, so its race arm cannot be authorized",
					tier.Target.Display()), true
			}
			bound = addMoney(bound, armBound)
		}
	}
	if bs == nil {
		return "", false
	}
	ceiling := bs.get()
	if ceiling == 0 {
		return "", false
	}
	spent := catalog.Money(before.AccountedCostMicroUSD())
	debt := bs.syncRetryDebt(before.ID, catalog.Money(before.RetryReserveMicroUSD))
	accounted := bs.accounted(before.ID, catalog.Money(before.RetryReserveMicroUSD), spent)
	observed := accounted - debt
	if crossesMoneyCeiling(ceiling, accounted, bound) {
		return fmt.Sprintf("both arms together could cost up to %s against %s already spent and %s reserved for failed attempts, crossing the %s ceiling; /budget raises or clears it",
			bound, observed, debt, ceiling), true
	}
	return "", false
}

// reconcileRaceAccounting makes the origin ledger hold every arm's actual
// cost even when a successful response could not append its settlement. The
// unresolved bound remains as reserve in that case, intentionally
// conservative; this record prevents the actual work from disappearing too.
func reconcileRaceAccounting(origin *session.Session, before session.State, a, b *raceArm) error {
	actual := int64(addMoney(
		catalog.Money(a.sess.State().CostMicroUSD-a.baseCost),
		catalog.Money(b.sess.State().CostMicroUSD-b.baseCost)))
	recorded := origin.State().ExternalCostMicroUSD - before.ExternalCostMicroUSD
	missing := actual - recorded
	if missing <= 0 {
		return nil
	}
	source := fmt.Sprintf("race-reconcile:%s:%s:%s", before.ID, a.sess.ID(), b.sess.ID())
	return origin.AppendBudgetTransfer(source, missing, 0)
}

// transferRaceAccounting moves the authoritative race delta from the origin
// ledger to a branch that may continue. The branch already owns its copied
// lineage and local Usage, so the transfer is the difference between that and
// the origin's fully reconciled ledger. This also captures metered origin work
// completed after the pre-race snapshot without fabricating provider usage.
func transferRaceAccounting(origin *session.Session, before session.State, kept, other *raceArm) error {
	after := origin.State()
	branch := kept.sess.State()
	external := after.AccountedCostMicroUSD() - branch.AccountedCostMicroUSD()
	if external < 0 {
		external = 0
	}
	debt := after.RetryReserveMicroUSD - branch.RetryReserveMicroUSD
	if debt < 0 {
		debt = 0
	}
	source := fmt.Sprintf("race:%s:%s:%s", before.ID, kept.sess.ID(), other.sess.ID())
	return kept.sess.AppendBudgetTransfer(source, external, debt)
}

// raceRecord assembles the verdict. outcome and kept follow the Race type's
// vocabulary; the prompt rides along so the record reads on its own.
func raceRecord(prompt string, a, b *raceArm, outcome, kept string) session.Race {
	return session.Race{
		Prompt:  prompt,
		A:       a.record(),
		B:       b.record(),
		Outcome: outcome,
		Kept:    kept,
	}
}
