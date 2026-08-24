// Package router chooses which target serves a turn.
//
// This is the heuristic router §8.2 says ships first: rules over the prompt and
// a handful of structured features, in pure Go, with no network call and no
// model. It is also the fallback for everything that comes later, so it has to
// work with no data at all.
//
// A learned router is deliberately not here. §8.2 defines each of a classifier's
// dimensions by a measurement procedure against the eval corpus, and that corpus
// is phase 2b: weights cannot be fit against data that does not exist, and "a
// dimension nobody can measure is not a dimension". The same section records the
// null hypothesis that a profile loses to a scalar, and §19.2 gates a learned
// router on beating heuristics after runtime and distribution costs. Running
// this router is what produces the evidence to settle that.
//
// Two things shape the design more than the rules do. Feasibility is not
// economics: a target that cannot hold the context or is not approved is
// infeasible, not expensive, and filtering it by price would let a budget
// override a policy. And §8.3 is explicit that the opening choice matters less
// than the mid-task ones, because a single user message produces dozens of model
// calls, so the escalation policy carries more weight here than the first pick.
package router

import (
	"fmt"
	"math"
	"strings"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/costmodel"
	"github.com/switchboard-code/switchboard/internal/provider"
)

// Source records what made a decision. §8.1 is explicit that this and Rationale
// are rendered rather than logged, because design principle 3 requires a user to
// see why a target was chosen.
type Source string

const (
	SourceHeuristic Source = "heuristic"
	SourceUserPin   Source = "user pin"
	SourceFallback  Source = "fallback"
)

// PolicyRevision identifies the exact deterministic policy that produced a
// record. Training or auditing rows without it would mix behavior changes into
// one apparent distribution.
const PolicyRevision = "heuristic-v2"

// SessionFeatures are the structured signals a turn carries with it.
type SessionFeatures struct {
	TurnDepth         int
	PromptTokens      int
	ContextTokens     int
	PriorFailures     int
	TestFailures      int
	FilesInContext    int
	DiffSizeSoFar     int
	DiffSizeKnown     bool
	RepoLanguages     []string
	TestsInvolved     bool
	LastTurnEscalated bool
}

// Candidate is a target the router may choose, with what is known about it.
type Candidate struct {
	Tier   string
	Target provider.RouteTarget
	Info   catalog.ModelInfo

	// Rank is the position on the user's ladder, ascending. §3.1 is clear that
	// this is the user's intended policy order and not a claim about capability,
	// so the router uses it as a preference and never as a fact.
	Rank int

	// PromptTokens is what the assembled request would cost this target to
	// read according to the ordinary estimator. It remains the input to dollar
	// estimation so the cost model can apply its own measured widening once.
	PromptTokens int

	// ContextTokens is the conservative upper bound used only for the hard
	// context-window check. Zero preserves compatibility for callers with an
	// exact count and falls back to PromptTokens.
	ContextTokens int

	// ReservedOutputTokens is the maximum output allowance the concrete
	// adapter will put on this request. Context feasibility is the whole
	// envelope, not input alone. Zero is valid only for adapters that reserve no
	// output; production candidate builders must resolve provider defaults.
	ReservedOutputTokens int

	Estimate costmodel.Estimate

	// CeilingCost is the conservative maximum-output preflight bound used for
	// a hard budget. Estimate remains the expected working-turn cost shown to
	// the user; conflating the two either lies in the estimate or weakens the
	// ceiling. Zero means the candidate cannot be priced conservatively.
	CeilingCost catalog.Money

	// CatalogKnown distinguishes a missing catalog entry (which production
	// permits as explicitly unpriced) from a paid entry whose pricing bands
	// cannot produce a positive conservative bound. The latter must be rejected
	// here because the immediate pre-provider budget guard rejects it too.
	CatalogKnown bool
}

// Requirements are the hard constraints. They are checked before economics,
// because an infeasible target is not a cheap one.
type Requirements struct {
	NeedsVision bool
	NeedsTools  bool

	// ApprovedProviders limits where a turn may go. Empty means no restriction.
	ApprovedProviders []string
}

// Budgets are ceilings, not preferences. §8.3 allows a quality trigger to
// override a cost preference and never a hard ceiling.
type Budgets struct {
	MaxCost catalog.Money

	// MaxCostSet distinguishes an exhausted zero-dollar allowance from no
	// dollar ceiling. Positive MaxCost values imply true for compatibility.
	MaxCostSet bool
}

type Input struct {
	Prompt       string
	Session      SessionFeatures
	Candidates   []Candidate
	Requirements Requirements
	Budgets      Budgets

	// Pin is a target the user asked for by name. §8.1 short-circuits model
	// selection only after the hard checks, so an infeasible pin is an error
	// rather than a way around policy.
	Pin string
}

type Decision struct {
	Tier   string
	Target provider.RouteTargetID

	Confidence     float64
	Source         Source
	Rationale      string
	PolicyRevision string

	EstimatedCost costmodel.Estimate

	// Infeasible records what was excluded and why, so a user who expected a
	// target can see that it was ruled out rather than out-priced.
	Infeasible []string
}

// Router is the interface the loop uses. Later implementations chain behind
// this one and fall back to it whenever they are unavailable or uncertain.
type Router interface {
	Route(in Input) (Decision, error)
}

// Heuristic is the rules-based router.
type Heuristic struct {
	// EscalateAtFiles and EscalateAtDiff are where breadth starts to look like
	// work a stronger target should do. They are named parameters rather than
	// literals because they are guesses until the eval corpus exists.
	EscalateAtFiles int
	EscalateAtDiff  int
}

const (
	DefaultEscalateAtFiles = 4
	DefaultEscalateAtDiff  = 200
)

func (h Heuristic) files() int {
	if h.EscalateAtFiles > 0 {
		return h.EscalateAtFiles
	}
	return DefaultEscalateAtFiles
}

func (h Heuristic) diff() int {
	if h.EscalateAtDiff > 0 {
		return h.EscalateAtDiff
	}
	return DefaultEscalateAtDiff
}

func (h Heuristic) Route(in Input) (Decision, error) {
	feasible, excluded := h.filter(in)

	// The pin is answered before the general case, so that a pinned target that
	// was ruled out says so rather than being reported as part of a ladder with
	// nothing left in it.
	if in.Pin != "" {
		for _, c := range feasible {
			if c.Tier == in.Pin || string(c.Target.ID()) == in.Pin {
				return Decision{
					Tier: c.Tier, Target: c.Target.ID(),
					Confidence: 1, Source: SourceUserPin,
					Rationale:      "pinned by you; capability, context, and budget were checked first",
					PolicyRevision: PolicyRevision,
					EstimatedCost:  c.Estimate,
					Infeasible:     excluded,
				}, nil
			}
		}
		// §8.1: an infeasible pin is an actionable error, not a silent
		// substitution, because quietly serving a different target than the one
		// asked for is worse than failing.
		return Decision{Infeasible: excluded}, fmt.Errorf(
			"the pinned target %q cannot serve this turn:\n  %s", in.Pin, strings.Join(excluded, "\n  "))
	}

	if len(feasible) == 0 {
		return Decision{Infeasible: excluded}, fmt.Errorf(
			"no target can serve this turn:\n  %s", strings.Join(excluded, "\n  "))
	}

	want, reasons := h.wantedRank(in)

	var chosen Candidate
	found := false
	for _, c := range feasible {
		if c.Rank <= want && (!found || c.Rank > chosen.Rank ||
			(c.Rank == chosen.Rank && string(c.Target.ID()) < string(chosen.Target.ID()))) {
			chosen = c
			found = true
		}
	}
	// Nothing at or below the wanted rank: take the lowest available rather
	// than exceeding what was asked for.
	if !found {
		for _, c := range feasible {
			if !found || c.Rank < chosen.Rank ||
				(c.Rank == chosen.Rank && string(c.Target.ID()) < string(chosen.Target.ID())) {
				chosen = c
				found = true
			}
		}
	}

	rationale := "a short request with no signals that call for more"
	if len(reasons) > 0 {
		rationale = strings.Join(reasons, "; ")
	}

	return Decision{
		Tier: chosen.Tier, Target: chosen.Target.ID(),
		Confidence:     h.confidence(reasons),
		Source:         SourceHeuristic,
		Rationale:      rationale,
		PolicyRevision: PolicyRevision,
		EstimatedCost:  chosen.Estimate,
		Infeasible:     excluded,
	}, nil
}

// filter removes targets that cannot serve the turn at all.
//
// Order matters: capability, context fit, and destination policy come before
// budget, so that a target excluded by policy is never reported as one that was
// too expensive.
func (h Heuristic) filter(in Input) (feasible []Candidate, excluded []string) {
	for _, c := range in.Candidates {
		switch {
		case in.Requirements.NeedsVision && !c.Info.Vision:
			excluded = append(excluded, fmt.Sprintf("%s cannot read images, which this turn needs", c.Target.Display()))

		case in.Requirements.NeedsTools && c.Info.Tools == catalog.ToolsNone:
			excluded = append(excluded, fmt.Sprintf("%s cannot call tools, so it cannot drive the loop", c.Target.Display()))

		case !approved(c.Target.Provider, in.Requirements.ApprovedProviders):
			excluded = append(excluded, fmt.Sprintf("%s is not an approved destination for this workspace", c.Target.Display()))

		case !candidateFitsContext(c):
			if c.ReservedOutputTokens == math.MaxInt {
				if c.Target.Params.MaxOutputTokens > 0 {
					excluded = append(excluded, fmt.Sprintf(
						"%s has no valid finite output allowance for its %d-token context window: configured max_output %d conflicts with the reasoning settings; raise max_output or lower or disable reasoning",
						c.Target.Display(), c.Info.ContextWindow, c.Target.Params.MaxOutputTokens))
				} else {
					excluded = append(excluded, fmt.Sprintf(
						"%s has no finite output bound for its %d-token context window; set a positive tier max_output with /models or in config",
						c.Target.Display(), c.Info.ContextWindow))
				}
			} else {
				excluded = append(excluded, fmt.Sprintf(
					"%s holds %d tokens and this turn may need up to %d input plus %d reserved output tokens",
					c.Target.Display(), c.Info.ContextWindow, candidateContextTokens(c), c.ReservedOutputTokens))
			}

		case c.CatalogKnown && c.Info.Metering != catalog.Local && c.Info.Metering != catalog.Plan && !c.Info.Free() && c.CeilingCost <= 0:
			excluded = append(excluded, fmt.Sprintf(
				"%s has no positive conservative cost bound in the catalog, so a provider call cannot be authorized",
				c.Target.Display()))

		case (in.Budgets.MaxCostSet || in.Budgets.MaxCost > 0) && candidateCeiling(c) > in.Budgets.MaxCost:
			// The upper bound is what a ceiling is checked against. Using the
			// expectation would approve a turn that is only affordable on
			// average, which is not what a ceiling means.
			excluded = append(excluded, fmt.Sprintf(
				"%s could cost up to %s, above the %s ceiling",
				c.Target.Display(), candidateCeiling(c), in.Budgets.MaxCost))

		default:
			feasible = append(feasible, c)
		}
	}
	return feasible, excluded
}

func candidateContextTokens(c Candidate) int {
	if c.ContextTokens > 0 {
		return c.ContextTokens
	}
	return c.PromptTokens
}

func candidateFitsContext(c Candidate) bool {
	window := c.Info.ContextWindow
	if window <= 0 {
		return true
	}
	input := candidateContextTokens(c)
	output := c.ReservedOutputTokens
	if input < 0 || output < 0 || output > window {
		return false
	}
	// Subtraction avoids overflowing when an unknown input was represented by
	// MaxInt. It also makes equality explicit: filling the entire advertised
	// envelope is allowed, exceeding it by one token is not.
	return input <= window-output
}

func candidateCeiling(c Candidate) catalog.Money {
	if c.CeilingCost > 0 {
		return c.CeilingCost
	}
	return c.Estimate.High
}

func approved(providerName string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if a == providerName {
			return true
		}
	}
	return false
}

// wantedRank is the ladder position the signals argue for, with the reasons.
//
// The rules are §8.2's worked examples plus the §8.3 triggers that are already
// visible at the start of a turn. They are guesses, and the reasons exist so a
// user can see which guess moved them.
func (h Heuristic) wantedRank(in Input) (int, []string) {
	want := 0
	var reasons []string
	top := 0
	for _, candidate := range in.Candidates {
		if candidate.Rank > top {
			top = candidate.Rank
		}
	}
	strong := top

	raise := func(to int, why string) {
		if to > want {
			want = to
		}
		reasons = append(reasons, why)
	}

	prompt := strings.ToLower(in.Prompt)
	words := len(strings.Fields(prompt))

	// A turn straight after a failure is §8.2's own example of escalation.
	if in.Session.TestFailures > 0 {
		raise(strong, fmt.Sprintf("following %d test failure(s)", in.Session.TestFailures))
	} else if in.Session.PriorFailures > 1 {
		raise(strong, fmt.Sprintf("following %d failed attempts", in.Session.PriorFailures))
	}

	if in.Session.FilesInContext >= h.files() {
		raise(strong, fmt.Sprintf("%d files in play", in.Session.FilesInContext))
	}
	if in.Session.DiffSizeSoFar >= h.diff() {
		raise(strong, fmt.Sprintf("a diff of %d lines so far", in.Session.DiffSizeSoFar))
	}

	// Breadth words are a weak signal and deliberately do not raise on their
	// own past one rung: they describe intent, and intent is not difficulty.
	for _, marker := range []string{"refactor", "migrate", "redesign", "architecture", "across the codebase"} {
		if strings.Contains(prompt, marker) {
			raise(1, fmt.Sprintf("the request mentions %q", marker))
			break
		}
	}
	if words > 80 {
		raise(1, "a long request")
	}

	// Sticky escalation: §8.3 asks for hysteresis, and dropping straight back
	// down after one good turn is the oscillation it warns about.
	if in.Session.LastTurnEscalated {
		raise(1, "the previous turn was escalated, so the level is held")
	}

	return want, reasons
}

// confidence is low by construction. These are rules over shallow features and
// the honest report is that the chain above should override them when it exists
// (§8.2 composes on confidence thresholds).
func (h Heuristic) confidence(reasons []string) float64 {
	switch {
	case len(reasons) == 0:
		return 0.5
	case len(reasons) == 1:
		return 0.55
	default:
		return 0.65
	}
}
