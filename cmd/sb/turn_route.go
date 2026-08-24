package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/prefix"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/rootedfs"
	route "github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/session"
)

// turnPlan is the complete, auditable opening decision for one user turn.
// Keeping the features beside the decision prevents the recorder from trying
// to reconstruct them after the turn has changed the session.
type turnPlan struct {
	Decision      route.Decision
	Features      route.SessionFeatures
	PromptTokens  int
	ContextTokens int
}

func prospectiveTurnPlan(loop *agent.Loop, sticky *route.Sticky, opening provider.Message, workspace string) turnPlan {
	state := loop.Session.State()
	messages := append(append([]provider.Message(nil), state.Messages...), opening)
	request := loop.Request(messages)
	promptTokens := prefix.RequestTokens(request)
	features := sessionFeatures(state.Messages, opening.Text(), sticky, workspace)
	features.PromptTokens = promptTokens
	contextTokens := prefix.RequestTokenCeiling(request)
	features.ContextTokens = contextTokens
	return turnPlan{Features: features, PromptTokens: promptTokens, ContextTokens: contextTokens}
}

// planUserTurn runs the deterministic router against the prospective request,
// including the opening message that has not entered the session yet. It does
// no probing or mutation; surfaces can perform those operations in their own
// event loop and commit the selected rung only after they succeed.
func planUserTurn(
	loop *agent.Loop,
	cfg *config.Config,
	cat *catalog.Catalog,
	probes *providers,
	budget *budgetState,
	caches *cacheSet,
	sticky *route.Sticky,
	current config.Tier,
	opening provider.Message,
	workspace string,
) (config.Tier, turnPlan, error) {
	return planUserTurnSkipping(loop, cfg, cat, probes, budget, caches, sticky, current, opening, workspace, nil)
}

func planUserTurnSkipping(
	loop *agent.Loop,
	cfg *config.Config,
	cat *catalog.Catalog,
	probes *providers,
	budget *budgetState,
	caches *cacheSet,
	sticky *route.Sticky,
	current config.Tier,
	opening provider.Message,
	workspace string,
	skipped map[string]string,
) (config.Tier, turnPlan, error) {
	state := loop.Session.State()
	messages := append(append([]provider.Message(nil), state.Messages...), opening)
	plan := prospectiveTurnPlan(loop, sticky, opening, workspace)
	promptTokens := plan.PromptTokens
	features := plan.Features
	remaining, limited := remainingBudget(budget, state.ID,
		catalog.Money(state.RetryReserveMicroUSD), catalog.Money(state.AccountedCostMicroUSD()))

	request := loop.Request(messages)
	hitProbabilities := map[provider.RouteTargetID]float64{}
	// The current tier may be served by a live fallback whose ID does not occur
	// in the configured ladder. Score the target that would actually receive the
	// request, including the warmth its session-local tracker has observed.
	hitProbabilities[current.Target.ID()] = caches.HitProbability(current.Target, cat, request)
	for _, tier := range cfg.Tiers {
		hitProbabilities[tier.Target.ID()] = caches.HitProbability(tier.Target, cat, request)
		for _, fallback := range tier.Fallbacks {
			hitProbabilities[fallback.ID()] = caches.HitProbability(fallback, cat, request)
		}
	}
	requirements := route.Requirements{
		NeedsTools:        true,
		NeedsVision:       messagesNeedVision(request.Messages),
		ApprovedProviders: cfg.Destinations,
	}
	budgets := route.Budgets{MaxCost: remaining, MaxCostSet: limited}
	candidates := make([]route.Candidate, 0, len(cfg.Tiers))
	for rank, configured := range cfg.Tiers {
		scored := configured
		if configured.ID == current.ID {
			scored = tierWithActiveTargetFirst(configured, current.Target)
		}
		primary := candidateForTierContext(scored, rank, cat, promptTokens, plan.ContextTokens, hitProbabilities[scored.Target.ID()])
		candidates = append(candidates, candidateWithLiveFallback(scored, primary, rank,
			cat, probes, promptTokens, plan.ContextTokens, hitProbabilities, requirements, budgets))
	}
	if len(skipped) > 0 {
		kept := candidates[:0]
		for _, candidate := range candidates {
			if _, rejected := skipped[candidate.Tier]; !rejected {
				kept = append(kept, candidate)
			}
		}
		candidates = kept
	}
	in := route.Input{
		Prompt:       opening.Text(),
		Session:      features,
		Candidates:   candidates,
		Requirements: requirements,
		Budgets:      budgets,
	}
	// Routing off and a user pin are the same instruction at this seam: the
	// current rung is the decision, and the router's work is hard-checking it
	// rather than choosing. Either way an infeasible rung is an actionable
	// error, never a silent substitution.
	if !cfg.RouteAutoOn() || (sticky != nil && sticky.Pinned()) {
		in.Pin = current.ID
	}

	decision, err := (route.Heuristic{}).Route(in)
	for _, tier := range cfg.Tiers {
		if reason := skipped[tier.ID]; reason != "" {
			decision.Infeasible = append(decision.Infeasible, reason)
		}
	}
	plan.Decision = decision
	if err != nil {
		if len(decision.Infeasible) > 0 {
			return config.Tier{}, plan, fmt.Errorf("%v\n  %s", err, strings.Join(decision.Infeasible, "\n  "))
		}
		return config.Tier{}, plan, err
	}
	tier, ok := cfg.Tier(decision.Tier)
	if !ok {
		return config.Tier{}, plan, fmt.Errorf("the router chose %q, which is not on the ladder", decision.Tier)
	}
	if decision.Tier == current.ID && decision.Target == current.Target.ID() {
		tier = current
	}
	return tier, plan, nil
}

func withLiveVision(candidate route.Candidate, probes *providers) route.Candidate {
	return withLiveCapabilities(candidate, probes)
}

func withLiveCapabilities(candidate route.Candidate, probes *providers) route.Candidate {
	if probes != nil {
		candidate.Info.ContextWindow = resolveContextWindow(probes.config, probes, candidate.Target, candidate.Info.ContextWindow)
		candidate.ReservedOutputTokens = probes.outputTokenAllowance(candidate.Target, candidate.Info.MaxOutput)
		candidate.CeilingCost = preflightBoundWithOutput(candidate.Info, candidate.ContextTokens, candidate.ReservedOutputTokens)
		// A live modality attestation outranks a surface-wide catalog default in
		// both directions. Silence does not: several APIs cannot report per-model
		// vision at all.
		if probe, known := probes.probedCapabilities(candidate.Target); known {
			if probe.VisionKnown {
				candidate.Info.Vision = probe.Vision
			}
			switch probe.Tools {
			case provider.ToolsNone:
				candidate.Info.Tools = catalog.ToolsNone
			case provider.ToolsSerial:
				candidate.Info.Tools = catalog.ToolsSerial
			case provider.ToolsParallel:
				candidate.Info.Tools = catalog.ToolsParallel
			case provider.ToolsUnreliable:
				// A live call established that tools exist; reliability remains an
				// eval judgment, which the catalog type deliberately does not model.
				candidate.Info.Tools = catalog.ToolsSerial
			}
		}
	}
	return candidate
}

func withLiveCandidateCapabilities(candidates []route.Candidate, probes *providers) []route.Candidate {
	for index := range candidates {
		candidates[index] = withLiveCapabilities(candidates[index], probes)
	}
	return candidates
}

func candidateWithLiveFallback(tier config.Tier, primary route.Candidate, rank int, cat *catalog.Catalog,
	probes *providers, promptTokens, contextTokens int, hitProbabilities map[provider.RouteTargetID]float64, requirements route.Requirements,
	budgets route.Budgets,
) route.Candidate {
	primary = withLiveCapabilities(primary, probes)
	if candidatePassesHardFilters(primary, probes, requirements, budgets) || !probeSaysUnavailable(probes, primary.Target) {
		return primary
	}
	for _, target := range tier.Fallbacks {
		fallbackTier := tier
		fallbackTier.Target = target
		candidate := withLiveCapabilities(candidateForTierContext(fallbackTier, rank, cat, promptTokens, contextTokens,
			hitProbabilities[target.ID()]), probes)
		if candidatePassesHardFilters(candidate, probes, requirements, budgets) {
			return candidate
		}
	}
	return primary
}

// probeSaysUnavailable is the only condition that unlocks a tier's fallback
// list during pure selection. Context, vision, destination, and budget failures
// belong to the concrete primary and make the rung infeasible; silently changing
// models to satisfy one of them would turn fallback into hidden routing.
func probeSaysUnavailable(probes *providers, target provider.RouteTarget) bool {
	if probes == nil {
		return false
	}
	probe, known := probes.probedCapabilities(target)
	return known && (!probe.Reachable || !probe.ModelPresent || probe.Tools == provider.ToolsNone)
}

func candidatePassesHardFilters(candidate route.Candidate, probes *providers, requirements route.Requirements, budgets route.Budgets) bool {
	if probe, known := probes.probedCapabilities(candidate.Target); known &&
		(!probe.Reachable || !probe.ModelPresent || probe.Tools == provider.ToolsNone) {
		return false
	}
	_, err := (route.Heuristic{}).Route(route.Input{
		Candidates: []route.Candidate{candidate}, Requirements: requirements, Budgets: budgets, Pin: candidate.Tier,
	})
	return err == nil
}

func tierWithActiveTargetFirst(configured config.Tier, active provider.RouteTarget) config.Tier {
	if active.ID() == configured.Target.ID() {
		return configured
	}
	if active.Params.MaxOutputTokens != configured.Target.Params.MaxOutputTokens {
		// A durable session binding predating a max_output change keeps its exact
		// wire identity until the user deliberately rebinds the rung. It cannot
		// inherit the current outage list: max_output is rung policy, and mixing
		// the historical primary with current-cap fallbacks creates a tier no
		// config can represent and the runtime correctly refuses. Keep the named
		// rung for pin/routing continuity, but make this historical binding its
		// sole approved target.
		return config.Tier{ID: configured.ID, Label: configured.Label, Target: active}
	}
	ordered := make([]provider.RouteTarget, 0, len(configured.Fallbacks)+1)
	for _, target := range append([]provider.RouteTarget{configured.Target}, configured.Fallbacks...) {
		if target.ID() != active.ID() {
			ordered = append(ordered, target)
		}
	}
	configured.Target = active
	configured.Fallbacks = ordered
	return configured
}

func ensureLiveCapabilityEvidence(ctx context.Context, cfg *config.Config, cat *catalog.Catalog, probes *providers, requirements route.Requirements) {
	if probes == nil || (!requirements.NeedsVision && !requirements.NeedsTools) {
		return
	}
	for _, tier := range cfg.Tiers {
		targets := append([]provider.RouteTarget{tier.Target}, tier.Fallbacks...)
		for _, target := range targets {
			if ctx.Err() != nil {
				return
			}
			if _, known := probes.probedCapabilities(target); known {
				continue
			}
			info, confidence, ok := cat.Lookup(target)
			unknownVision := requirements.NeedsVision && (!ok || confidence != catalog.Verified) && !info.Vision
			unknownTools := requirements.NeedsTools && (!ok || confidence != catalog.Verified) && info.Tools == catalog.ToolsNone
			if !unknownVision && !unknownTools {
				continue
			}
			candidate := tier
			candidate.Target = target
			// The result, including a negative capability result, is retained by
			// the registry. Selection below decides whether it is usable.
			_, _, _ = probes.probeTier(ctx, candidate)
		}
	}
}

// resolveUserTurn couples pure scoring to live binding. A concrete target that
// fails a hard check is rejected and the deterministic router is run again;
// the failure never weakens capability, context, or budget policy.
func resolveUserTurn(ctx context.Context, loop *agent.Loop, cfg *config.Config, cat *catalog.Catalog,
	probes *providers, budget *budgetState, caches *cacheSet, sticky *route.Sticky, current config.Tier,
	_ provider.Provider, opening provider.Message, workspace string,
) (config.Tier, provider.Provider, string, turnPlan, error) {
	state := loop.Session.State()
	messages := append(append([]provider.Message(nil), state.Messages...), opening)
	messages = loop.Request(messages).Messages
	requirements := route.Requirements{
		NeedsTools:        true,
		NeedsVision:       messagesNeedVision(messages),
		ApprovedProviders: cfg.Destinations,
	}
	ensureLiveCapabilityEvidence(ctx, cfg, cat, probes, requirements)

	rejected := map[string]string{}
	var lastPlan turnPlan
	for {
		if len(rejected) >= len(cfg.Tiers) {
			return config.Tier{}, nil, "", lastPlan, liveRejectionError(cfg, rejected)
		}
		tier, plan, err := planUserTurnSkipping(loop, cfg, cat, probes, budget, caches, sticky,
			current, opening, workspace, rejected)
		lastPlan = plan
		if err != nil {
			return config.Tier{}, nil, "", plan, err
		}
		rank := -1
		for index, configured := range cfg.Tiers {
			if configured.ID == tier.ID {
				rank = index
				break
			}
		}
		if rank < 0 {
			return config.Tier{}, nil, "", plan, fmt.Errorf("the router selected %s but it is not on the configured ladder", tier.ID)
		}

		configured := cfg.Tiers[rank]
		// A resumed/current fallback stays first within its rung. Every opening is
		// still live-probed: cached capability evidence cannot prove that a server
		// which answered yesterday is reachable for this turn.
		if configured.ID == current.ID && current.Target.ID() != configured.Target.ID() {
			configured = tierWithActiveTargetFirst(configured, current.Target)
		}
		probed, client, note, err := probes.probeTierFallbackFeasible(ctx, configured, func(candidate config.Tier) error {
			return checkTurnFeasible(loop, cat, probes, budget, cfg.Destinations, candidate, rank, plan, opening)
		})
		if err != nil {
			rejected[tier.ID] = fmt.Sprintf("tier %s was rejected after live feasibility checking: %v", tier.ID, err)
			continue
		}
		retargetTurnPlan(&plan, loop, cat, caches, probed, rank, opening)
		return probed, client, note, plan, nil
	}
}

func liveRejectionError(cfg *config.Config, rejected map[string]string) error {
	reasons := make([]string, 0, len(rejected))
	for _, tier := range cfg.Tiers {
		if reason := rejected[tier.ID]; reason != "" {
			reasons = append(reasons, reason)
		}
	}
	if len(reasons) == 0 {
		return fmt.Errorf("no target can serve this turn after live feasibility checking")
	}
	return fmt.Errorf("no target can serve this turn after live feasibility checking:\n  %s", strings.Join(reasons, "\n  "))
}

func retargetTurnPlan(plan *turnPlan, loop *agent.Loop, cat *catalog.Catalog, caches *cacheSet,
	tier config.Tier, rank int, opening provider.Message,
) {
	if plan == nil {
		return
	}
	state := loop.Session.State()
	messages := append(append([]provider.Message(nil), state.Messages...), opening)
	request := loop.Request(messages)
	hitProbability := caches.HitProbability(tier.Target, cat, request)
	candidate := candidateForTierContext(tier, rank, cat, plan.PromptTokens, plan.ContextTokens, hitProbability)
	plan.Decision.Target = tier.Target.ID()
	plan.Decision.EstimatedCost = candidate.Estimate
}

func messageNeedsVision(m provider.Message) bool {
	for _, block := range m.Content {
		if _, ok := block.(provider.Image); ok {
			return true
		}
	}
	return false
}

func messagesNeedVision(messages []provider.Message) bool {
	for _, message := range messages {
		if messageNeedsVision(message) {
			return true
		}
	}
	return false
}

// checkMoveFeasible re-runs the router's hard filters against the conversation
// that the destination would receive on its next call. Mid-turn evidence is a
// quality preference, never permission to overflow context, lose image
// fidelity, or cross a hard budget.
func checkMoveFeasible(loop *agent.Loop, cat *catalog.Catalog, probes *providers, budget *budgetState, destinations []string, tier config.Tier, rank int) error {
	state := loop.Session.State()
	request := loop.Request(state.Messages)
	promptTokens := prefix.RequestTokens(request)
	contextTokens := prefix.RequestTokenCeiling(request)
	remaining, limited := remainingBudget(budget, state.ID,
		catalog.Money(state.RetryReserveMicroUSD), catalog.Money(state.AccountedCostMicroUSD()))
	_, err := (route.Heuristic{}).Route(route.Input{
		Candidates: []route.Candidate{withLiveVision(candidateForTierContext(tier, rank, cat, promptTokens, contextTokens, 0), probes)},
		Requirements: route.Requirements{
			NeedsTools:        true,
			NeedsVision:       messagesNeedVision(request.Messages),
			ApprovedProviders: destinations,
		},
		Budgets: route.Budgets{MaxCost: remaining, MaxCostSet: limited},
		Pin:     tier.ID,
	})
	return err
}

// checkTurnFeasible validates the concrete target returned by a live probe
// against the prospective opening request. This is especially important for
// fallbacks: availability substitution stays inside a tier, but it may have a
// smaller context window, no vision, or a different price than the configured
// primary the router originally selected.
func checkTurnFeasible(loop *agent.Loop, cat *catalog.Catalog, probes *providers, budget *budgetState, destinations []string, tier config.Tier, rank int, plan turnPlan, opening provider.Message) error {
	state := loop.Session.State()
	remaining, limited := remainingBudget(budget, state.ID,
		catalog.Money(state.RetryReserveMicroUSD), catalog.Money(state.AccountedCostMicroUSD()))
	messages := append(append([]provider.Message(nil), state.Messages...), opening)
	messages = loop.Request(messages).Messages
	_, err := (route.Heuristic{}).Route(route.Input{
		Candidates: []route.Candidate{withLiveVision(candidateForTierContext(tier, rank, cat, plan.PromptTokens, plan.ContextTokens, 0), probes)},
		Requirements: route.Requirements{
			NeedsTools:        true,
			NeedsVision:       messagesNeedVision(messages),
			ApprovedProviders: destinations,
		},
		Budgets: route.Budgets{MaxCost: remaining, MaxCostSet: limited},
		Pin:     tier.ID,
	})
	return err
}

func remainingBudget(budget *budgetState, scope string, persisted, spent catalog.Money) (catalog.Money, bool) {
	if budget == nil {
		return 0, false
	}
	ceiling := budget.get()
	if ceiling == 0 {
		return 0, false
	}
	accounted := budget.accounted(scope, persisted, spent)
	if accounted >= ceiling {
		return 0, true
	}
	return ceiling - accounted, true
}

func sessionFeatures(messages []provider.Message, prompt string, sticky *route.Sticky, workspace string) route.SessionFeatures {
	priorFailures, testFailures, tests := priorTurnEvidence(messages)
	if strings.Contains(strings.ToLower(prompt), "test") {
		tests = true
	}
	features := route.SessionFeatures{
		TurnDepth:      len(messages),
		PriorFailures:  priorFailures,
		TestFailures:   testFailures,
		FilesInContext: filesInContext(messages),
		RepoLanguages:  repoLanguages(workspace),
		TestsInvolved:  tests,
		// Diff size is intentionally marked unavailable until the turn owns a
		// reliable VCS baseline. A numeric zero without this bit would become a
		// false training label for workspaces whose diff could not be measured.
		DiffSizeKnown: false,
	}
	if sticky != nil {
		features.LastTurnEscalated = sticky.EscalatedLastTurn()
	}
	return features
}

func priorTurnEvidence(messages []provider.Message) (failures, testFailures int, tests bool) {
	start := -1
	for i, message := range messages {
		if session.OpensTurn(message) {
			start = i
		}
	}
	if start < 0 {
		return 0, 0, false
	}

	testCalls := map[string]bool{}
	for _, message := range messages[start:] {
		for _, block := range message.Content {
			switch value := block.(type) {
			case provider.ToolUse:
				if toolInputLooksLikeTests(value.Input) {
					testCalls[value.ID] = true
					tests = true
				}
			case provider.ToolResult:
				if value.IsError {
					failures++
				}
				if testCalls[value.ToolUseID] {
					tests = true
					if value.IsError {
						testFailures++
					}
				}
			}
		}
	}
	return failures, testFailures, tests
}

// toolInputLooksLikeTests reconstructs the command-bearing shapes used by
// built-ins and extension tools. Searching raw JSON misses argv arrays because
// "go" and "test" are separate strings on the wire.
func toolInputLooksLikeTests(raw json.RawMessage) bool {
	var input struct {
		Argv    []string `json:"argv"`
		Command string   `json:"command"`
		Cmd     string   `json:"cmd"`
	}
	if json.Unmarshal(raw, &input) == nil {
		for _, command := range []string{strings.Join(input.Argv, " "), input.Command, input.Cmd} {
			if route.LooksLikeTests(command) {
				return true
			}
		}
	}
	// Extension tools can use a schema unknown to Switchboard. Keeping the raw
	// fallback preserves detection when they send an ordinary command string.
	return route.LooksLikeTests(string(raw))
}

func filesInContext(messages []provider.Message) int {
	paths := map[string]struct{}{}
	for _, message := range messages {
		for _, call := range message.ToolUses() {
			switch call.Name {
			case "read", "write", "edit", "grep", "glob", "ast_grep":
			default:
				continue
			}
			var input struct {
				Path string `json:"path"`
				File string `json:"file"`
			}
			if json.Unmarshal(call.Input, &input) != nil {
				continue
			}
			path := input.Path
			if path == "" {
				path = input.File
			}
			if path != "" && path != "." {
				paths[filepath.Clean(path)] = struct{}{}
			}
		}
	}
	return len(paths)
}

var languageByExtension = map[string]string{
	".c": "C", ".cc": "C++", ".cpp": "C++", ".cs": "C#",
	".css": "CSS", ".dart": "Dart", ".ex": "Elixir", ".exs": "Elixir",
	".go": "Go", ".html": "HTML", ".java": "Java", ".js": "JavaScript",
	".jsx": "JavaScript", ".kt": "Kotlin", ".kts": "Kotlin", ".lua": "Lua",
	".php": "PHP", ".py": "Python", ".rb": "Ruby", ".rs": "Rust",
	".scala": "Scala", ".sh": "Shell", ".sql": "SQL", ".swift": "Swift",
	".ts": "TypeScript", ".tsx": "TypeScript", ".vue": "Vue", ".zig": "Zig",
}

const (
	// Language evidence is a routing hint, not permission to stall the turn
	// loop on an adversarial checkout. The former walker inspected as many as
	// 50,000 entries synchronously on every turn. Keep this deliberately lower
	// and abandon the hint altogether if either the work or latency budget is
	// exhausted.
	routeLanguageMaxEntries = 10_000
	routeLanguageMaxDirs    = 2_000
	routeLanguageMaxDepth   = 32
	routeLanguageBatch      = 64
	routeLanguageTimeout    = 100 * time.Millisecond
)

func repoLanguages(root string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), routeLanguageTimeout)
	defer cancel()
	return repoLanguagesWithContext(ctx, root, rootedfs.WalkLimits{
		MaxEntries:     routeLanguageMaxEntries,
		MaxDirectories: routeLanguageMaxDirs,
		MaxDepth:       routeLanguageMaxDepth,
		ReadDirBatch:   routeLanguageBatch,
	}, nil)
}

// repoLanguagesWithLimits is deliberately all-or-nothing. Language evidence
// influences routing; a prefix from a capped or raced traversal is not a fact
// about the repository and must be omitted rather than scored as complete.
func repoLanguagesWithLimits(root string, limits rootedfs.WalkLimits, beforeOpen func(string)) []string {
	return repoLanguagesWithContext(context.Background(), root, limits, beforeOpen)
}

func repoLanguagesWithContext(ctx context.Context, root string, limits rootedfs.WalkLimits, beforeOpen func(string)) []string {
	if ctx == nil || ctx.Err() != nil {
		return nil
	}
	if strings.TrimSpace(root) == "" {
		return nil
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil
	}
	expected, err := os.Lstat(real)
	if err != nil || expected.Mode()&os.ModeSymlink != 0 || !expected.IsDir() {
		return nil
	}
	if beforeOpen != nil {
		beforeOpen(real)
	}
	capability, err := rootedfs.OpenRoot(real)
	if err != nil {
		return nil
	}
	defer capability.Close()
	opened, err := capability.Stat(".")
	current, currentErr := os.Lstat(real)
	if err != nil || currentErr != nil || current.Mode()&os.ModeSymlink != 0 || !current.IsDir() ||
		!os.SameFile(expected, opened) || !os.SameFile(opened, current) {
		return nil
	}

	seen := map[string]struct{}{}
	status, walkErr := rootedfs.WalkRegularFiles(ctx, capability, ".", limits,
		func(relative string, _ os.FileInfo) bool {
			switch filepath.Base(relative) {
			case ".git", ".hg", ".svn", "node_modules", "vendor", "dist", "build", ".switchboard":
				return true
			default:
				return false
			}
		},
		func(_ string, _ *os.Root, name string, _ os.FileInfo) error {
			if language := languageByExtension[strings.ToLower(filepath.Ext(name))]; language != "" {
				seen[language] = struct{}{}
			}
			return nil
		})
	current, currentErr = os.Lstat(real)
	if walkErr != nil || status.Partial() || currentErr != nil || current.Mode()&os.ModeSymlink != 0 ||
		!current.IsDir() || !os.SameFile(expected, current) {
		return nil
	}
	if openedAfter, statErr := capability.Stat("."); statErr != nil || !os.SameFile(expected, openedAfter) {
		return nil
	}
	languages := make([]string, 0, len(seen))
	for language := range seen {
		languages = append(languages, language)
	}
	sort.Strings(languages)
	return languages
}
