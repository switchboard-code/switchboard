package main

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/breakpoint"
	"github.com/switchboard-code/switchboard/internal/cachestate"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/costmodel"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/provider/anthropic"
	"github.com/switchboard-code/switchboard/internal/provider/kimi"
	route "github.com/switchboard-code/switchboard/internal/router"
)

// cacheSet keeps one tracker per target for the life of a session. Moving away
// from a target abandons its warmth for the next call, but it does not erase
// the provider's cache or Switchboard's observations; moving back can still
// estimate whether that prefix survives.
type cacheSet struct {
	mu       sync.Mutex
	byTarget map[provider.RouteTargetID]*agent.Cache
}

func newCacheSet(target provider.RouteTarget, cache *agent.Cache) *cacheSet {
	set := &cacheSet{byTarget: map[provider.RouteTargetID]*agent.Cache{}}
	if cache != nil {
		set.byTarget[target.ID()] = cache
	}
	return set
}

func (s *cacheSet) For(target provider.RouteTarget, cat *catalog.Catalog) *agent.Cache {
	if s == nil {
		return cacheFor(target, cat)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cache, ok := s.byTarget[target.ID()]; ok {
		return cache
	}
	cache := cacheFor(target, cat)
	if cache != nil {
		s.byTarget[target.ID()] = cache
	}
	return cache
}

func (s *cacheSet) Reset(target provider.RouteTarget, cache *agent.Cache) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byTarget = map[provider.RouteTargetID]*agent.Cache{}
	if cache != nil {
		s.byTarget[target.ID()] = cache
	}
}

func (s *cacheSet) HitProbability(target provider.RouteTarget, cat *catalog.Catalog, req provider.Request) float64 {
	cache := s.For(target, cat)
	if cache == nil {
		// Unknown catalog targets deliberately run cache-unaware. They have no
		// controller to query, and unknown warmth is not evidence of a hit.
		return 0
	}
	if expectation, ok := cache.Predict(req.System, req.Tools, req.Messages, time.Now()); ok {
		return expectation.HitProbability
	}
	return 0
}

// candidatesFor turns the configured ladder into what the router scores.
//
// Every tier becomes a candidate, including ones that will be excluded: the
// router reports why each was ruled out, and a user who expected a target needs
// to see the reason rather than its absence.
func candidatesFor(cfg *config.Config, cat *catalog.Catalog, promptTokens int, hitProbabilities map[provider.RouteTargetID]float64) []route.Candidate {
	// An aggregate chars/4 floor cannot be inverted into a hard upper bound:
	// block boundaries and framing have already been lost. Legacy callers that
	// only have that floor therefore fail closed for finite context/budget
	// targets. Production request paths call candidatesForContext instead.
	return candidatesForContext(cfg, cat, promptTokens, math.MaxInt, hitProbabilities)
}

func candidatesForContext(cfg *config.Config, cat *catalog.Catalog, promptTokens, contextTokens int, hitProbabilities map[provider.RouteTargetID]float64) []route.Candidate {
	out := make([]route.Candidate, 0, len(cfg.Tiers))
	for rank, tier := range cfg.Tiers {
		out = append(out, candidateForTierContext(tier, rank, cat, promptTokens, contextTokens, hitProbabilities[tier.Target.ID()]))
	}
	return out
}

func candidateForTier(tier config.Tier, rank int, cat *catalog.Catalog, promptTokens int, hitProbability float64) route.Candidate {
	// See candidatesFor: the expected-cost floor is still useful for display,
	// but it is insufficient evidence for a hard admission decision.
	return candidateForTierContext(tier, rank, cat, promptTokens, math.MaxInt, hitProbability)
}

func candidateForTierContext(tier config.Tier, rank int, cat *catalog.Catalog, promptTokens, contextTokens int, hitProbability float64) route.Candidate {
	info, _, ok := cat.Lookup(tier.Target)
	if !ok {
		// No catalog entry means nothing is known about capability or price.
		// Live probing remains authoritative for reachability and tools.
		info = catalog.ModelInfo{}
	}
	candidate := route.Candidate{
		Tier: tier.ID, Target: tier.Target, Info: info, Rank: rank, PromptTokens: promptTokens,
		ContextTokens: contextTokens, ReservedOutputTokens: reservedOutputTokens(tier.Target, info),
		CatalogKnown: ok,
	}
	candidate.Estimate = costmodel.Estimator{}.Turn(costmodel.Inputs{
		Target: tier.Target, Info: info, PrefixTokens: promptTokens,
		// Output is unknown before the turn runs. A flat allowance keeps the
		// displayed expectation comparable while CeilingCost below remains the
		// hard maximum-output bound.
		OutputTokens: 512, Eligible: info.Cache.UsageAccounting == catalog.AccountingSeparate,
		HitProbability: hitProbability,
	})
	candidate.CeilingCost = preflightBoundForTarget(info, tier.Target, contextTokens)
	return candidate
}

func reservedOutputTokens(target provider.RouteTarget, info catalog.ModelInfo) int {
	return effectiveOutputTokenAllowance(nil, target, info.MaxOutput)
}

// effectiveOutputTokenAllowance is the serving-surface resolver shared by
// routing, context admission, and budget admission. A bound adapter gets first
// say; Messages targets can also be scored before binding through the same pure
// dialect policy their adapter uses to build max_tokens.
func effectiveOutputTokenAllowance(bound provider.Provider, target provider.RouteTarget, catalogMax int) int {
	if bound != nil {
		return provider.EffectiveOutputTokenAllowance(bound, target, catalogMax)
	}
	if target.Provider == anthropic.Name || target.Provider == kimi.Name {
		return anthropic.OutputTokenAllowance(target)
	}
	return provider.EffectiveOutputTokenAllowance(nil, target, catalogMax)
}

// describeRoute renders a decision.
//
// §8.1 says Rationale and Source are not diagnostics: design principle 3
// requires that a user can see why a target was chosen, so this is printed
// rather than logged.
func describeRoute(d route.Decision) []string {
	lines := []string{
		fmt.Sprintf("  route      %s via %s (%s)", d.Tier, d.Source, d.Rationale),
	}
	if d.EstimatedCost.Expected > 0 || d.EstimatedCost.High > 0 {
		lines = append(lines, fmt.Sprintf("  estimate   %s, between %s and %s",
			d.EstimatedCost.Expected, d.EstimatedCost.Low, d.EstimatedCost.High))
	}
	for _, why := range d.Infeasible {
		lines = append(lines, "  ruled out  "+why)
	}
	return lines
}

// cacheFor builds the cache controller for a target, or nil when the catalog
// knows nothing about it.
//
// A nil controller is a cache-unaware loop, which is the right answer for a
// target whose caching behaviour has not been recorded: placing markers on a
// guess would spend writes to learn nothing, and §6.3 would then have
// observations it could not interpret.
func cacheFor(target provider.RouteTarget, cat *catalog.Catalog) *agent.Cache {
	info, _, ok := cat.Lookup(target)
	if !ok {
		return nil
	}
	return &agent.Cache{
		Manager: &breakpoint.Manager{Policy: info.Cache, Target: target.ID()},
		Tracker: cachestate.New(),
		Policy:  info.Cache,
		Target:  target.ID(),
	}
}
