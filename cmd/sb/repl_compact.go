package main

// Compaction on the REPL.
//
// The REPL had none, of either kind. A session here ran until the provider
// refused a request, which is the failure the design calls out as the one
// worth avoiding: a session that works until the moment it does not, with the
// end arriving as an error rather than as a visible handoff.

import (
	"context"
	"errors"
	"fmt"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/session"
)

func (r *repl) compact(ctx context.Context, objective string) {
	r.compactWithMode(ctx, objective, false)
}

// compactWithMode keeps manual and threshold-triggered compaction on the same
// summarizer-slot policy as the TUI. A manual request refuses an unreachable
// configured summarizer; an automatic boundary falls back visibly to the live
// tier because ending at the context wall would be worse than using it.
func (r *repl) compactWithMode(ctx context.Context, objective string, auto bool) {
	if r.store == nil {
		r.out.Notice("error", "this session has no store to compact into")
		return
	}
	state := r.loop.Session.State()
	if len(state.Messages) == 0 {
		r.out.Notice("", "nothing to compact yet")
		return
	}
	if err := validateCompactScope(state.Messages, objective); err != nil {
		r.out.Notice("error", "compact stopped before summarizing, session unchanged: "+err.Error())
		return
	}

	// No fixed deadline: a slow target's summary outlasts any cap, and a
	// cap's only answer was killing the compact at five minutes. A turn has
	// no cap either; interruption is ctrl-c, as everywhere in the REPL.
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	summarizer, fromSlot, err := resolveSlotTier(r.config, r.tier, "summarizer")
	if err != nil {
		r.out.Notice("error", "compact failed, session unchanged: "+err.Error())
		return
	}
	active := r.loop.Binding()
	client, target := active.Provider, active.Target
	usedSlot := false
	if fromSlot {
		probed, slotClient, probeErr := r.providers.probeTier(callCtx, summarizer)
		switch {
		case probeErr == nil:
			client, target = slotClient, probed.Target
			usedSlot = true
		case auto:
			r.out.Notice("warn", "summarizer slot "+summarizer.Target.Display()+
				" is unreachable ("+probeErr.Error()+"); compacting with the current tier instead")
		default:
			r.out.Notice("error", "summarizer slot "+summarizer.Target.Display()+
				" is unreachable, session unchanged: "+probeErr.Error())
			return
		}
	}
	line := fmt.Sprintf("  compacting: summarizing %d messages on %s", len(state.Messages), target.Display())
	if usedSlot {
		line += " (the summarizer slot)"
	}
	r.out.line(r.out.style(dim, cliText(line)+"…"))
	r.out.flush()

	sess, err := compactSession(callCtx, compactInputs{
		Source:        r.loop.Session,
		Store:         r.store,
		Workspace:     r.workspace,
		Catalog:       r.catalog,
		Budget:        r.budget,
		Client:        client,
		Target:        target,
		SessionTarget: active.Target.ID(),
		ContextWindow: effectiveContextWindow(r.config, r.providers, r.catalog, target),
		Objective:     objective,
	})
	if err != nil {
		r.out.Notice("error", "compact failed, session unchanged: "+err.Error())
		return
	}

	previous := r.loop.Session
	if err := adoptREPLCompactionWithPublisher(r.loop, previous, sess, r.publishDurably); err != nil {
		r.out.Notice("error", err.Error())
		var restart *publicationRestartRequiredError
		if errors.As(err, &restart) {
			r.restartRequired = err
		}
		return
	}
	r.callTokens = 0
	r.out.Notice("", "compacted into session "+sess.ID()+"; the summary is its opening message")

	// A compacted session does not wait to be told what it already knows:
	// the continuation runs as the new session's first turn. That one launch
	// skips the threshold check: a frozen zone or conservative provider receipt
	// can still sit above the threshold after a valid handoff, and immediately
	// compacting it again would recurse without reducing that zone.
	r.out.Notice("", "Switchboard automatic continuation")
	if err := r.turnPreparedSyntheticMode(ctx, compactContinuePrompt, nil, false, true); err != nil {
		r.out.Notice("error", "the continuation could not start: "+err.Error())
	}
}

// adoptREPLCompaction binds a staged compact session and publishes it as one
// adoption transaction. The source stays open until publication succeeds, so
// either fallible seam can return the loop to the prior durable session and
// close the invisible stage for bounded maintenance.
func adoptREPLCompaction(loop *agent.Loop, previous, fresh *session.Session) error {
	return adoptREPLCompactionWithPublisher(loop, previous, fresh, nil)
}

func adoptREPLCompactionWithPublisher(loop *agent.Loop, previous, fresh *session.Session, publisher durableSessionPublisher) error {
	// Mirror the TUI adoption seam: compaction continues the source's durable
	// routing posture, including an explicit pin and a fallback/parameterized
	// target. SessionTarget gives the fresh header a safe identity, but the
	// moving runtime binding is what a later resume treats as authoritative.
	binding := previous.State().RuntimeBinding
	if binding.Target != "" && binding.Tier != "" {
		if err := fresh.AppendRuntimeBinding(binding.Tier, binding.Target, binding.Pinned); err != nil {
			err = errors.Join(err, fresh.CloseDiscardingStaged())
			return fmt.Errorf("compact could not carry the runtime binding, session unchanged: %w", err)
		}
	}
	if err := loop.BindSession(fresh); err != nil {
		// BindSession publishes its pointer only after recovery and task-context
		// restoration succeed, so a failure here leaves previous active.
		err = errors.Join(err, fresh.CloseDiscardingStaged())
		return fmt.Errorf("compact could not adopt the new session, session unchanged: %w", err)
	}
	outcome, rawErr := publishDurablyWith(fresh, publisher)
	disposition, err := publicationResult(outcome, rawErr, "compacted session "+fresh.ID())
	if disposition == publicationUnpublished {
		// Publication is rollbackable only while no marker is visible. The source
		// remains open specifically so this in-memory bind can be reversed without
		// leaving the REPL on an invisible crash artifact.
		if rollbackErr := loop.BindSession(previous); rollbackErr != nil {
			fatalErr := fmt.Errorf(
				"compact publication failed and source-session rollback also failed; restart Switchboard before continuing: %w",
				errors.Join(err, rollbackErr),
			)
			// BindSession already exposed fresh as the loop authority. A failed
			// rollback therefore cannot return as an ordinary command error: doing so
			// would accept the next prompt on an invisible child. Close both possible
			// authorities and leave the unpublished stage hidden for bounded
			// maintenance before marking the REPL restart-only.
			freshCloseErr := fresh.CloseDiscardingStaged()
			previousCloseErr := previous.Close()
			if freshCloseErr != nil {
				freshCloseErr = fmt.Errorf("closing failed compact child: %w", freshCloseErr)
			}
			if previousCloseErr != nil {
				previousCloseErr = fmt.Errorf("closing compact source after failed rollback: %w", previousCloseErr)
			}
			return &publicationRestartRequiredError{err: errors.Join(fatalErr, freshCloseErr, previousCloseErr)}
		}
		err = errors.Join(err, fresh.CloseDiscardingStaged())
		return fmt.Errorf("compact could not publish the new session, session unchanged: %w", err)
	}
	_ = previous.Close()
	if disposition == publicationVisibleUncertain {
		// The marker already commits adoption, so neither rollback nor staged
		// cleanup is legal. The caller stops the REPL before its automatic
		// continuation can append anything to a child that may vanish on power loss.
		// Close the adopted handle as an additional mutation backstop; Close never
		// removes a visible log or its marker.
		return &publicationRestartRequiredError{err: errors.Join(err, fresh.Close())}
	}
	return nil
}

// autoCompactIfFull runs at turn end, which is where occupancy is known from
// what the provider last reported rather than from an estimate. A turn
// boundary is also the only place a session may be replaced without cutting a
// turn in half.
func (r *repl) autoCompactIfFull(ctx context.Context) {
	if !r.shouldCompactNow() {
		return
	}
	r.out.Notice("", fmt.Sprintf("context is %d%% full; compacting to keep the session going",
		r.callTokens*100/r.ctxWindow))
	r.compactWithMode(ctx, "", true)
}

// shouldCompactNow is the same decision the TUI makes, in the same terms: a
// window nobody stated is not a window to measure against, and an occupancy of
// zero is a turn nobody has taken yet.
func (r *repl) shouldCompactNow() bool {
	if !r.config.CompactAuto || r.ctxWindow <= 0 || r.callTokens <= 0 {
		return false
	}
	return r.callTokens >= r.ctxWindow*compactThreshold(r.config)/100
}

// refreshCtxWindow settles how much room this target has, from the same
// sources and in the same order the TUI uses: the user's declaration beats a
// metadata-inferred probe, an enforced probe beats the declaration, and the
// catalog is last.
func (r *repl) refreshCtxWindow() {
	target := r.loop.Binding().Target
	r.ctxWindow = effectiveContextWindow(r.config, r.providers, r.catalog, target)
}
