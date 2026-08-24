package main

import (
	"context"
	"fmt"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/prefix"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/provider/ollama"
	route "github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/session"
)

// resumeTier resolves the target a startup resume asked to keep or explicitly
// replace. It is deliberately pure: opening a log may repair its crash tail,
// but no new runtime binding is recorded until the complete prospective replay
// has passed the same hard checks as an ordinary turn.
func resumeTier(cfg *config.Config, state session.State, opts *options) (config.Tier, bool, error) {
	switch {
	case opts != nil && opts.model != "":
		target := ollama.Target(opts.model)
		applyEffort(&target, opts.think)
		return config.Tier{ID: "-model", Label: "ad hoc", Target: target}, false, nil
	case opts != nil && opts.tier != "":
		tier, ok := cfg.Tier(opts.tier)
		if !ok {
			return config.Tier{}, false, fmt.Errorf("no tier %s is configured; run sb -tiers to see the ladder", opts.tier)
		}
		applyEffort(&tier.Target, opts.think)
		return tier, true, nil
	default:
		return tierForSessionState(cfg, state)
	}
}

// probeResumeTarget is the single admission gate for startup and in-process
// resume. The request includes the frozen system and tool zones plus the exact
// replay projection, while the candidate carries the concrete fallback's live
// context, output, tool, and vision evidence. Nothing is persisted here.
func probeResumeTarget(
	ctx context.Context,
	sess *session.Session,
	loop *agent.Loop,
	cat *catalog.Catalog,
	probes *providers,
	budget *budgetState,
	destinations []string,
	requested config.Tier,
	configured bool,
	rank int,
) (config.Tier, provider.Provider, string, error) {
	if rank < 0 {
		rank = 0
	}
	feasible := func(candidate config.Tier) error {
		return checkResumeFeasible(sess, loop, cat, probes, budget, destinations, candidate, rank)
	}

	var (
		probed config.Tier
		client provider.Provider
		note   string
		err    error
	)
	if configured {
		probed, client, note, err = probes.probeTierFallbackFeasible(ctx, requested, feasible)
	} else {
		probed, client, err = probes.probeTier(ctx, requested)
		if err == nil {
			err = feasible(probed)
		}
	}
	if err != nil {
		return config.Tier{}, nil, "", fmt.Errorf(
			"session %s was not adopted on %s: %w; update the tier, destination, context/output, or budget policy, or restart with -tier/-model naming a feasible target",
			sess.ID(), requested.Target.Display(), err)
	}
	if err := ctx.Err(); err != nil {
		return config.Tier{}, nil, "", err
	}
	return probed, client, note, nil
}

func checkResumeFeasible(sess *session.Session, loop *agent.Loop, cat *catalog.Catalog, probes *providers, budget *budgetState, destinations []string, tier config.Tier, rank int) error {
	state := sess.State()
	request := loop.Request(state.Messages)
	promptTokens := prefix.RequestTokens(request)
	contextTokens := prefix.RequestTokenCeiling(request)
	remaining, limited := remainingBudget(budget, state.ID,
		catalog.Money(state.RetryReserveMicroUSD), catalog.Money(state.AccountedCostMicroUSD()))
	_, err := (route.Heuristic{}).Route(route.Input{
		Candidates: []route.Candidate{withLiveCapabilities(candidateForTierContext(tier, rank, cat, promptTokens, contextTokens, 0), probes)},
		Requirements: route.Requirements{
			NeedsTools:        true,
			NeedsVision:       messagesNeedVision(request.Messages),
			ApprovedProviders: destinations,
		},
		Budgets: route.Budgets{MaxCost: remaining, MaxCostSet: limited}, Pin: tier.ID,
	})
	return err
}

// finalizeStartupResume performs the first mutation that adopts a resumed
// target. Callers assemble the complete loop and tool registry before entering
// here. A failed probe or hard check therefore leaves the recorded binding and
// the preliminary provider binding untouched.
func finalizeStartupResume(
	ctx context.Context,
	sess *session.Session,
	loop *agent.Loop,
	cfg *config.Config,
	cat *catalog.Catalog,
	probes *providers,
	budget *budgetState,
	opts *options,
) (config.Tier, provider.Provider, string, error) {
	state := sess.State()
	requested, configured, err := resumeTier(cfg, state, opts)
	if err != nil {
		return config.Tier{}, nil, "", err
	}
	rank := tierRank(cfg, requested.ID)
	probed, client, note, err := probeResumeTarget(ctx, sess, loop, cat, probes, budget,
		cfg.Destinations, requested, configured, rank)
	if err != nil {
		return config.Tier{}, nil, "", err
	}
	pinned := opts != nil && (opts.model != "" || opts.tier != "") ||
		(state.RuntimeBinding.Target != "" && state.RuntimeBinding.Pinned)
	if err := persistRuntimeBindingFallback(sess, probed, pinned, note); err != nil {
		return config.Tier{}, nil, "", fmt.Errorf("saving resumed runtime binding: %w", err)
	}
	loop.Bind(agent.Binding{Provider: client, Target: probed.Target, Cache: cacheFor(probed.Target, cat)})
	return probed, client, note, nil
}

func tierRank(cfg *config.Config, id string) int {
	if cfg == nil {
		return -1
	}
	for rank, tier := range cfg.Tiers {
		if tier.ID == id {
			return rank
		}
	}
	return -1
}
