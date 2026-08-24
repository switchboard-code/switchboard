package eval

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/costmodel"
	"github.com/switchboard-code/switchboard/internal/prefix"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/router"
)

var errRoutedFidelity = errors.New("routed evaluation fidelity is unavailable")

func fidelityErrorf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errRoutedFidelity, fmt.Sprintf(format, args...))
}

// RoutedArmFor builds the arm under test: the same targets as the baselines,
// with the router choosing between them.
//
// The comparison §7.1 asks for only means something if the arms differ in the
// routing and nothing else. Same corpus, same tools, same sandbox, same
// verifier; the one variable is who picks the target.
type RoutedArmFor struct {
	Catalog *catalog.Catalog
	Router  router.Router

	// Ladder is the ordered set of targets the router may choose from, lowest
	// first. Order is the user's policy ladder, not a capability claim (§3.1).
	Ladder []Arm

	// Requirements and Budgets are the production hard constraints applied to
	// every concrete primary and fallback. Tool use is always required because
	// the evaluation runner is an agent loop, and image need is derived from the
	// assembled request in addition to any explicit requirement here.
	Requirements router.Requirements
	Budgets      router.Budgets
}

// Pick is retained to make the old unsafe entry point fail closed. A task by
// itself does not contain the workspace-bound system prompt or tool schemas,
// so it cannot support a faithful routing decision. Run assembles the request
// once and calls PickRequest with the exact value execution will use.
func (r RoutedArmFor) Pick(Task) (Arm, router.Decision, error) {
	return Arm{}, router.Decision{}, fidelityErrorf(
		"opening selection requires the Runner's assembled system, tools, and message request")
}

// PickRequest chooses a target from an already assembled prospective opening
// request and reports why. It performs the same live reachability/capability
// and hard feasibility checks used again for mid-turn moves.
//
// The router sees the prompt and nothing that would leak the answer. It gets no
// task id, no provenance, and no knowledge of which package is broken: a router
// told which task it was solving would be measured on a job nobody has.
func (r RoutedArmFor) PickRequest(ctx context.Context, task Task, request provider.Request) (Arm, router.Decision, error) {
	arm, decision, _, err := r.pickRequest(ctx, task, request, r.Budgets)
	return arm, decision, err
}

func (r RoutedArmFor) pickRequest(
	ctx context.Context,
	task Task,
	request provider.Request,
	budgets router.Budgets,
) (Arm, router.Decision, int, error) {
	if r.Catalog == nil {
		return Arm{}, router.Decision{}, 0, fidelityErrorf("the routed arm has no catalog")
	}
	if len(r.Ladder) == 0 {
		return Arm{}, router.Decision{}, 0, fidelityErrorf("the routed arm has no ladder")
	}
	if err := validateRungOutputCaps(r.Ladder); err != nil {
		return Arm{}, router.Decision{}, 0, err
	}

	promptTokens := prefix.RequestTokens(request)
	contextTokens := prefix.RequestTokenCeiling(request)
	requirements := r.Requirements
	requirements.NeedsTools = true
	requirements.NeedsVision = requirements.NeedsVision || requestNeedsVision(request)

	candidates := make([]router.Candidate, 0, len(r.Ladder))
	resolved := make(map[string]Arm, len(r.Ladder))
	ranks := make(map[string]int, len(r.Ladder))
	var unavailable []string
	for rank, arm := range r.Ladder {
		if arm.Name == "" {
			return Arm{}, router.Decision{}, 0, fidelityErrorf("ladder rank %d has no tier name", rank)
		}
		if _, duplicate := resolved[arm.Name]; duplicate {
			return Arm{}, router.Decision{}, 0, fidelityErrorf("ladder contains duplicate tier %q", arm.Name)
		}
		selected, candidate, err := r.resolveTier(
			ctx, arm, rank, promptTokens, contextTokens, requirements, budgets)
		if err != nil {
			unavailable = append(unavailable, fmt.Sprintf("%s: %v", arm.Name, err))
			continue
		}
		resolved[arm.Name] = selected
		ranks[arm.Name] = rank
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		return Arm{}, router.Decision{}, 0, fidelityErrorf(
			"no tier has a live, feasible target:\n  %s", strings.Join(unavailable, "\n  "))
	}

	selector := r.Router
	if selector == nil {
		selector = router.Heuristic{}
	}
	decision, err := selector.Route(router.Input{
		Prompt: requestPrompt(request, task.Prompt),
		Session: router.SessionFeatures{
			PromptTokens:  promptTokens,
			ContextTokens: contextTokens,
			TestsInvolved: strings.Contains(strings.ToLower(requestPrompt(request, task.Prompt)), "test"),
		},
		Candidates:   candidates,
		Requirements: requirements,
		Budgets:      budgets,
	})
	if err != nil {
		return Arm{}, decision, 0, fidelityErrorf("router rejected the assembled opening request: %v", err)
	}
	decision.Infeasible = append(decision.Infeasible, unavailable...)

	arm, ok := resolved[decision.Tier]
	if !ok {
		return Arm{}, decision, 0, fidelityErrorf(
			"router selected tier %q, which has no live feasible binding", decision.Tier)
	}
	if decision.Target == "" || decision.Target != arm.Target.ID() {
		return Arm{}, decision, 0, fidelityErrorf(
			"router selected target %s for tier %q, but the live feasible binding is %s",
			provider.DisplayRouteTargetID(decision.Target), decision.Tier, arm.Target.Display())
	}
	// Renamed so the report can tell the routed arm apart from the baseline
	// that happens to use the same target.
	arm.Name = RoutedArm
	return arm, decision, ranks[decision.Tier], nil
}

func validateRungOutputCaps(ladder []Arm) error {
	for _, tier := range ladder {
		for index, fallback := range tier.Fallbacks {
			if fallback.Target.Params.MaxOutputTokens != tier.Target.Params.MaxOutputTokens {
				return fidelityErrorf(
					"tier %s fallback %d has max_output %d, different from the rung's %d",
					tier.Name, index+1, fallback.Target.Params.MaxOutputTokens,
					tier.Target.Params.MaxOutputTokens)
			}
		}
	}
	return nil
}

// Run attempts a task with the router choosing the target and the escalation
// policy allowed to change it mid-task.
//
// The escalation is the point. §8.3 says the opening choice is worth less than
// the mid-task adjustments because one message produces dozens of model calls,
// and a routed arm that picks once and never revises is a fixed target wearing
// a different name. Measuring that against a fixed target would answer a
// question nobody asked.
func (r RoutedArmFor) Run(ctx context.Context, runner Runner, task Task, seed int) Run {
	return runner.runSelected(ctx, task, RoutedArm, "", seed,
		func(ctx context.Context, task Task, prepared *preparedAttempt) (armSelection, error) {
			arm, decision, startRank, err := r.pickRequest(ctx, task, prepared.openingRequest(), r.Budgets)
			if err != nil {
				return armSelection{}, err
			}
			escalation := &escalator{
				sticky:  router.NewSticky(router.Policy{}, startRank),
				detect:  router.NewDetector(),
				ladder:  r.Ladder,
				catalog: r.Catalog,
				routed:  r,
			}
			return armSelection{
				arm:             arm,
				escalation:      escalation,
				estimatedCost:   decision.EstimatedCost.Expected,
				estimatedTarget: arm.Target.ID(),
			}, nil
		})
}

func (r RoutedArmFor) resolveTier(
	ctx context.Context,
	tier Arm,
	rank, promptTokens, contextTokens int,
	requirements router.Requirements,
	budgets router.Budgets,
) (Arm, router.Candidate, error) {
	for index, fallback := range tier.Fallbacks {
		if fallback.Target.Params.MaxOutputTokens != tier.Target.Params.MaxOutputTokens {
			return Arm{}, router.Candidate{}, fmt.Errorf(
				"tier %s fallback %d has max_output %d, different from the rung's %d",
				tier.Name, index+1, fallback.Target.Params.MaxOutputTokens,
				tier.Target.Params.MaxOutputTokens)
		}
	}

	primary := Arm{
		Name: tier.Name, Target: tier.Target, Provider: tier.Provider, CacheAware: tier.CacheAware,
		ContextWindow: tier.ContextWindow,
	}
	resolved, info, primaryProbeErr := resolveArmEvidence(ctx, r.Catalog, primary)
	if primaryProbeErr == nil {
		candidate := candidateForRequest(resolved, rank, info, promptTokens, contextTokens)
		_, _, candidate.CatalogKnown = r.Catalog.Lookup(resolved.Target)
		if _, err := (router.Heuristic{}).Route(router.Input{
			Candidates: []router.Candidate{candidate}, Requirements: requirements, Budgets: budgets, Pin: tier.Name,
		}); err != nil {
			// A reachable primary owns this rung. Availability fallbacks cannot
			// weaken its context, vision, destination, or budget refusal.
			return Arm{}, router.Candidate{}, fmt.Errorf(
				"primary %s cannot serve this request: %w", tier.Target.Display(), err)
		}
		return resolved, candidate, nil
	}

	attempts := []string{fmt.Sprintf("%s: %v", tier.Target.Display(), primaryProbeErr)}
	for _, fallback := range tier.Fallbacks {
		candidateArm := Arm{
			Name: tier.Name, Target: fallback.Target, Provider: fallback.Provider,
			CacheAware: fallback.CacheAware, ContextWindow: fallback.ContextWindow,
		}
		candidateArm, info, err := resolveArmEvidence(ctx, r.Catalog, candidateArm)
		if err == nil {
			candidate := candidateForRequest(candidateArm, rank, info, promptTokens, contextTokens)
			_, _, candidate.CatalogKnown = r.Catalog.Lookup(candidateArm.Target)
			if _, routeErr := (router.Heuristic{}).Route(router.Input{
				Candidates: []router.Candidate{candidate}, Requirements: requirements, Budgets: budgets, Pin: tier.Name,
			}); routeErr == nil {
				return candidateArm, candidate, nil
			} else {
				err = routeErr
			}
		}
		attempts = append(attempts, fmt.Sprintf("%s: %v", fallback.Target.Display(), err))
	}
	return Arm{}, router.Candidate{}, fmt.Errorf(
		"tier and fallbacks cannot serve this request:\n  %s", strings.Join(attempts, "\n  "))
}

func resolveArmEvidence(ctx context.Context, cat *catalog.Catalog, arm Arm) (Arm, catalog.ModelInfo, error) {
	if arm.Provider == nil {
		return Arm{}, catalog.ModelInfo{}, fmt.Errorf("target %s has no provider binding", arm.Target.Display())
	}
	probe, err := arm.Provider.Probe(ctx, arm.Target)
	if err != nil {
		return Arm{}, catalog.ModelInfo{}, fmt.Errorf("live probe failed: %w", err)
	}
	switch {
	case !probe.Reachable:
		return Arm{}, catalog.ModelInfo{}, fmt.Errorf("target is unreachable: %s", probe.Detail)
	case !probe.ModelPresent:
		return Arm{}, catalog.ModelInfo{}, fmt.Errorf("model is unavailable: %s", probe.Detail)
	case probe.Tools == provider.ToolsNone:
		return Arm{}, catalog.ModelInfo{}, fmt.Errorf("target cannot call tools")
	}

	var info catalog.ModelInfo
	if cat != nil {
		info, _, _ = cat.Lookup(arm.Target)
	}
	if probe.VisionKnown {
		info.Vision = probe.Vision
	}
	switch probe.Tools {
	case provider.ToolsNone:
		info.Tools = catalog.ToolsNone
	case provider.ToolsSerial, provider.ToolsUnreliable:
		info.Tools = catalog.ToolsSerial
	case provider.ToolsParallel:
		info.Tools = catalog.ToolsParallel
	}
	info.ContextWindow = resolvedContextWindow(
		arm.ContextWindow, probe.ContextWindow, probe.WindowEnforced, info.ContextWindow)
	arm.resolvedContextWindow = info.ContextWindow
	return arm, info, nil
}

func resolvedContextWindow(declared, probed int, enforced bool, catalogWindow int) int {
	switch {
	case probed > 0 && enforced:
		return probed
	case declared > 0:
		return declared
	case probed > 0:
		return probed
	default:
		return catalogWindow
	}
}

func candidateForRequest(arm Arm, rank int, info catalog.ModelInfo, promptTokens, contextTokens int) router.Candidate {
	outputReserve := provider.EffectiveOutputTokenAllowance(arm.Provider, arm.Target, info.MaxOutput)
	candidate := router.Candidate{
		Tier: arm.Name, Target: arm.Target, Info: info, Rank: rank,
		PromptTokens: promptTokens, ContextTokens: contextTokens,
		ReservedOutputTokens: outputReserve,
	}
	candidate.Estimate = costmodel.Estimator{}.Turn(costmodel.Inputs{
		Target: arm.Target, Info: info, PrefixTokens: promptTokens,
		OutputTokens: 512, Eligible: info.Cache.UsageAccounting == catalog.AccountingSeparate,
		HitProbability: 0,
	})
	candidate.CeilingCost = costmodel.Estimator{}.Turn(costmodel.Inputs{
		Target: arm.Target, Info: info, PrefixTokens: contextTokens,
		OutputTokens:   outputReserve,
		Eligible:       info.Cache.UsageAccounting == catalog.AccountingSeparate,
		TokensAreExact: true,
	}).High
	return candidate
}

func requestNeedsVision(request provider.Request) bool {
	for _, message := range request.Messages {
		for _, block := range message.Content {
			if _, ok := block.(provider.Image); ok {
				return true
			}
		}
	}
	return false
}

func requestPrompt(request provider.Request, fallback string) string {
	if len(request.Messages) == 0 {
		return fallback
	}
	if prompt := request.Messages[len(request.Messages)-1].Text(); prompt != "" {
		return prompt
	}
	return fallback
}

// TargetsUsed reports which targets the router actually chose, which is the
// first thing to check when a routed arm matches a baseline exactly: a router
// that always picks the same rung is a baseline wearing a different name.
func TargetsUsed(runs []Run) map[provider.RouteTargetID]int {
	out := map[provider.RouteTargetID]int{}
	for _, r := range runs {
		if r.Arm == RoutedArm {
			out[r.Target]++
		}
	}
	return out
}

// Escalations reports how many times a routed run changed target, which is the
// number that says whether the arm under test was actually doing anything a
// fixed baseline could not.
func Escalations(runs []Run) (moved, total int) {
	for _, r := range runs {
		if r.Arm != RoutedArm {
			continue
		}
		total++
		if r.Escalations > 0 {
			moved++
		}
	}
	return moved, total
}
