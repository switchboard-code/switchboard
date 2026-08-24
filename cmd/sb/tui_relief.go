package main

// Relief: a round the bound target will not take, answered by the ladder.
//
// Two refusals end a turn today that a second rung could have finished. A
// request the target cannot hold is refused before anything leaves the
// process, and the only answer built for it is compaction, which throws away
// conversation to fit a window another rung already has. A target that spent
// its retries on rate limits or server faults ends the turn too, having
// learned only that this one target is busy.
//
// A ladder can answer both, and that is the half of this product a
// single-model agent cannot copy at any price: it meets an overflow with a
// lossy summary and a 429 with sleep, because it has nowhere else to go.
//
// The two halves keep different records and must not be collapsed. An overflow
// rebind is a fact about the request that any reader should see as a move, so
// it is persisted and rendered like one. An availability substitution is a
// fact about one target at one moment and is recorded only as a runtime
// binding: writing it as a route record would tell /why, /ladder, and every
// per-rung aggregate that the router made a decision it never made.
//
// Everything a primary binding must pass, these pass. The candidate is probed,
// checked for capability, context, destination policy, and budget through the
// same closure a move uses, and a user pin refuses relief outright — a pin is
// the user saying which rung, and answering a refusal by leaving it would be
// the program overruling that quietly.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/config"
)

// relief answers the loop's request for a different binding. It runs on the
// loop's goroutine between rounds, which is why it probes rather than assumes:
// the rung it hands back has to be one that just answered.
func (a *tuiApp) relief(ctx context.Context, reason agent.ReliefReason, cause error) (agent.Binding, string, error) {
	if a.sticky != nil && a.sticky.Pinned() {
		return agent.Binding{}, "", errors.New("the session is pinned, so the rung is yours to change")
	}
	if !a.config.RouteAutoOn() {
		return agent.Binding{}, "", errors.New("routing is off, so the rung is yours to change")
	}

	_, current := a.runtimeSnapshot()
	candidates := a.reliefCandidates(reason, cause)
	if len(candidates) == 0 {
		return agent.Binding{}, "", fmt.Errorf("no other rung is configured")
	}

	var refusals []string
	for _, candidate := range candidates {
		rank := candidate.rank
		probed, client, fallbackNote, err := a.providers.probeTierFallbackFeasible(ctx, candidate.tier, func(tier config.Tier) error {
			return checkMoveFeasible(a.loop, a.catalog, a.providers, a.budget, a.config.Destinations, tier, rank)
		})
		if err != nil {
			refusals = append(refusals, candidate.tier.ID+": "+err.Error())
			continue
		}
		if ctx.Err() != nil {
			return agent.Binding{}, "", ctx.Err()
		}
		if a.reliefAfterProbe != nil {
			a.reliefAfterProbe()
		}
		if !a.providers.preparedClientCurrent(client) {
			return agent.Binding{}, "", &providerReconfiguredError{}
		}
		if probed.Target.ID() == current.Target.ID() {
			// The same target cannot answer the refusal it just made.
			refusals = append(refusals, candidate.tier.ID+": resolves to the rung that refused")
			continue
		}

		// The old rung's warmth is abandoned exactly as a move abandons it,
		// and priced before the rebind discards the tracker.
		abandoned := abandonedCacheNote(current.Cache, a.catalog, time.Now())

		if fallbackNote != "" {
			err = a.loop.Session.AppendRuntimeBindingNote(probed.ID, probed.Target.ID(), false, "warn", fallbackNote)
		} else {
			err = persistRuntimeBinding(a.loop.Session, probed, false)
		}
		if err != nil {
			return agent.Binding{}, "", fmt.Errorf("the substitution was not saved, so it was not made: %w", err)
		}
		a.bindRuntime(probed, client)

		note := reliefNote(reason, current.Target.Display(), probed.Target.Display(), cause)

		// The routing dots are fed by every rebind, whoever asked for it,
		// because they have to agree with /why about how far the session
		// moved. Both reasons send this; what differs is the sentence, which
		// says plainly which of the two happened.
		if a.p != nil {
			a.p.Send(tierNowMsg{line: note, rank: rank, tier: probed, abandoned: abandoned})
		}
		// The loop renders this exact fallback sentence before it starts the
		// next provider call. The generic relief sentence above remains the UI's
		// routing-state update and must not replace the substitution evidence.
		return a.loop.Binding(), fallbackNote, nil
	}

	return agent.Binding{}, "", fmt.Errorf("no rung could take this over: %s", joinRefusals(refusals))
}

type reliefCandidate struct {
	tier config.Tier
	rank int
}

// reliefCandidates orders the rungs worth trying, and the order differs by
// reason. An overflow wants room, so the widest window first; an unavailable
// target wants anything else that answers, so the ladder's own order stands.
func (a *tuiApp) reliefCandidates(reason agent.ReliefReason, _ error) []reliefCandidate {
	_, current := a.runtimeSnapshot()
	var out []reliefCandidate
	for rank, tier := range a.config.Tiers {
		if tier.Target.ID() == current.Target.ID() {
			continue
		}
		out = append(out, reliefCandidate{tier: tier, rank: rank})
	}
	if reason != agent.ReliefContext {
		return out
	}

	// Widest first. A window this program does not know the size of sorts
	// last rather than first: an unpriced rung is not evidence of room.
	windows := make(map[string]int, len(out))
	for _, candidate := range out {
		if info, _, ok := a.catalog.Lookup(candidate.tier.Target); ok {
			windows[candidate.tier.ID] = info.ContextWindow
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && windows[out[j].tier.ID] > windows[out[j-1].tier.ID]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func reliefNote(reason agent.ReliefReason, from, to string, cause error) string {
	switch reason {
	case agent.ReliefContext:
		return fmt.Sprintf("%s could not hold this request, so the turn moved to %s: %v", from, to, cause)
	default:
		return fmt.Sprintf("%s did not answer, so the turn is being served by %s instead; "+
			"this is an availability substitution and not a routing decision: %v", from, to, cause)
	}
}

func joinRefusals(refusals []string) string {
	if len(refusals) == 0 {
		return "every candidate was the rung that refused"
	}
	out := refusals[0]
	for _, refusal := range refusals[1:] {
		out += "; " + refusal
	}
	return out
}
