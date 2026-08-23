package main

// Compaction on the REPL.
//
// The REPL had none, of either kind. A session here ran until the provider
// refused a request, which is the failure the design calls out as the one
// worth avoiding: a session that works until the moment it does not, with the
// end arriving as an error rather than as a visible handoff.

import (
	"context"
	"fmt"
)

func (r *repl) compact(ctx context.Context, instructions string) {
	if r.store == nil {
		r.out.Notice("error", "this session has no store to compact into")
		return
	}
	state := r.loop.Session.State()
	if len(state.Messages) == 0 {
		r.out.Notice("", "nothing to compact yet")
		return
	}
	r.out.line(r.out.style(dim, fmt.Sprintf("  compacting: summarizing %d messages on %s…",
		len(state.Messages), r.tier.Target.Display())))
	r.out.flush()

	// No fixed deadline: a slow target's summary outlasts any cap, and a
	// cap's only answer was killing the compact at five minutes. A turn has
	// no cap either; interruption is ctrl-c, as everywhere in the REPL.
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sess, err := compactSession(callCtx, compactInputs{
		Source:       r.loop.Session,
		Store:        r.store,
		Workspace:    r.workspace,
		Catalog:      r.catalog,
		Budget:       r.budget,
		Client:       r.loop.Binding().Provider,
		Target:       r.tier.Target,
		Instructions: instructions,
	})
	if err != nil {
		r.out.Notice("error", "compact failed, session unchanged: "+err.Error())
		return
	}

	previous := r.loop.Session
	if err := r.loop.BindSession(sess); err != nil {
		// The replacement is discarded rather than half-adopted: a loop still
		// pointed at the old log with a new session open beside it would spend
		// the rest of the run writing to one and reading the other.
		sess.Close()
		r.out.Notice("error", "compact could not adopt the new session, session unchanged: "+err.Error())
		return
	}
	previous.Close()
	r.callTokens = 0
	r.out.Notice("", "compacted into session "+sess.ID()+"; the summary is its opening message")

	// A compacted session does not wait to be told what it already knows:
	// the continuation runs as the new session's first turn.
	r.out.line(r.out.style(bold, "› ") + compactContinuePrompt)
	if err := r.turnPrepared(ctx, compactContinuePrompt, nil, false); err != nil {
		r.out.Notice("error", "the continuation could not start: "+err.Error())
	}
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
	r.compact(ctx, "")
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
	probed, enforced := r.providers.probedContextWindow(target)
	declared := r.config.ProviderForTarget(target.Provider, target.Surface).ContextWindow
	switch {
	case declared > 0 && !enforced:
		r.ctxWindow = declared
	case probed > 0:
		r.ctxWindow = probed
	case declared > 0:
		r.ctxWindow = declared
	default:
		if info, _, ok := r.catalog.Lookup(target); ok {
			r.ctxWindow = info.ContextWindow
			return
		}
		r.ctxWindow = 0
	}
}
