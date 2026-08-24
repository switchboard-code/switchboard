package main

import (
	"fmt"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

// sessionRuntimeBinding returns the durable moving binding when the log has
// one, and the immutable session-start target for legacy logs.
func sessionRuntimeBinding(state session.State) session.RuntimeBinding {
	if state.RuntimeBinding.Target != "" {
		return state.RuntimeBinding
	}
	return session.RuntimeBinding{Target: provider.RouteTargetID(state.Target)}
}

// tierForSessionState reconstructs both the named rung and its exact active
// target. The tier name removes the ambiguity of two configured rungs sharing
// one target; the target identity preserves a fallback or parameterized target
// that was active when the process stopped.
func tierForSessionState(cfg *config.Config, state session.State) (config.Tier, bool, error) {
	binding := sessionRuntimeBinding(state)
	if binding.Target == "" {
		return config.Tier{}, false, fmt.Errorf("session %s recorded no target", state.ID)
	}

	if binding.Tier != "" {
		if configured, ok := cfg.Tier(binding.Tier); ok {
			target, err := parseRecordedTarget(string(binding.Target))
			if err != nil {
				return config.Tier{}, false, err
			}
			return tierWithActiveTargetFirst(configured, target), true, nil
		}
	}

	// Legacy logs did not record a tier. Retain their migration and ambiguity
	// checks before falling back to a standalone resumed target.
	if binding.Tier == "" {
		if configured, ok, err := tierForTarget(cfg, string(binding.Target)); err != nil || ok {
			return configured, ok, err
		}
	}
	target, err := parseRecordedTarget(string(binding.Target))
	if err != nil {
		return config.Tier{}, false, err
	}
	tierID := binding.Tier
	if tierID == "" {
		tierID = "-resumed"
	}
	return config.Tier{ID: tierID, Label: "resumed", Target: target}, false, nil
}

func persistRuntimeBinding(sess *session.Session, tier config.Tier, pinned bool) error {
	if sess == nil {
		return fmt.Errorf("cannot persist a runtime binding without a session")
	}
	return sess.AppendRuntimeBinding(tier.ID, tier.Target.ID(), pinned)
}

// persistRuntimeBindingFallback commits an availability substitution as one
// event: the exact served target and the sentence explaining why it differs
// from the configured primary. A caller may render note only after this
// succeeds; otherwise content could leave on a fallback whose audit evidence
// was lost in a second, best-effort append.
func persistRuntimeBindingFallback(sess *session.Session, tier config.Tier, pinned bool, note string) error {
	if note == "" {
		return persistRuntimeBinding(sess, tier, pinned)
	}
	if sess == nil {
		return fmt.Errorf("cannot persist a runtime binding without a session")
	}
	return sess.AppendRuntimeBindingNote(tier.ID, tier.Target.ID(), pinned, "warn", note)
}

// persistAutomaticPosture clears only the durable pin bit. This intentionally
// retains the last durable target rather than the live target because /think
// is a process-only parameter override and must not leak into the WAL when the
// user subsequently runs /tier auto.
func persistAutomaticPosture(sess *session.Session, live config.Tier) error {
	state := sess.State()
	binding := state.RuntimeBinding
	if binding.Target == "" {
		return persistRuntimeBinding(sess, live, false)
	}
	return sess.AppendRuntimeBinding(binding.Tier, binding.Target, false)
}
