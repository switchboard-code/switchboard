package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	route "github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// watcher feeds the escalation policy from what the loop is already reporting.
//
// It wraps the observer rather than changing the loop, because the observer
// already carries everything §8.3's detectable triggers need: which tool ran,
// with what arguments, whether it failed, and what the model said. Threading a
// second callback through the loop for the same events would be two things to
// keep in step.
//
// Every method passes through to the inner observer first. A watcher that
// swallowed output while deciding what to escalate would trade the thing the
// user is watching for a decision they cannot see.
type watcher struct {
	inner    agent.Observer
	detector *route.Detector
	sticky   *route.Sticky

	// maxRank is the top of the configured ladder, so a move that would run off
	// the end is reported rather than silently ignored.
	maxRank int

	// onMove is called when the primary actually changes, so the caller can
	// rebind the target without this needing to know how.
	// onMove probes and prepares an infallible live bind plus a post-commit
	// presentation callback. Sticky invokes the bind only if the proposal is
	// still current; UI/log publication likewise waits for that commit.
	onMove func(ctx context.Context, rank int, why string) (bind func() bool, afterCommit func(), ok bool)

	mu    sync.Mutex
	moves int

	// lastUsage is the most recent provider receipt in the current turn. The
	// REPL has no event loop of its own to retain TurnUsage, so it reads this at
	// the turn boundary when deciding whether the last request filled the context
	// window. Resetting it with the detector prevents a call from the prior turn
	// being mistaken for a zero-usage or locally refused current one.
	lastUsage session.Usage
	usageSeen bool

	// paused stops the policy moving the primary, without stopping the
	// observation behind it. Signals keep being detected and recorded, so /why
	// still answers what the router would have done, and the advisor still has
	// the stream it reads. Turning routing off is a decision about who moves
	// the rung, not about whether the session is watched.
	paused bool
}

// setPaused turns automatic movement off or on. A pause takes effect from the
// next assessment; a move already committed stands, because unwinding a rung
// the user has seen and a provider has been billed for would be a second
// surprise rather than the removal of the first.
func (w *watcher) setPaused(paused bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.paused = paused
}

func (w *watcher) isPaused() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.paused
}

func newWatcher(inner agent.Observer, sticky *route.Sticky, maxRank int, onMove func(context.Context, int, string) (func() bool, func(), bool)) *watcher {
	return &watcher{
		inner:    inner,
		detector: route.NewDetector(),
		sticky:   sticky,
		maxRank:  maxRank,
		onMove:   onMove,
	}
}

// StartTurn resets the per-turn state. Failure signatures do not carry across
// turns, because §8.3 counts consecutive failures within one.
func (w *watcher) StartTurn() {
	w.detector.Reset()
	w.sticky.StartTurn()
	w.mu.Lock()
	w.moves = 0
	w.lastUsage = session.Usage{}
	w.usageSeen = false
	w.mu.Unlock()
}

func (w *watcher) MoveCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.moves
}

// LastUsage returns the final provider receipt observed in the current turn.
// A true second result with zero input is meaningful: the endpoint completed
// but reported no occupancy, so the caller must use its local request estimate.
func (w *watcher) LastUsage() (session.Usage, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastUsage, w.usageSeen
}

func (w *watcher) ThinkingDelta(text string) { w.inner.ThinkingDelta(text) }

func (w *watcher) TextDelta(text string) {
	w.inner.TextDelta(text)
	w.observe(w.detector.AssistantText(text))
}

func (w *watcher) ToolStart(call provider.ToolUse, req permission.Request) {
	w.inner.ToolStart(call, req)
	// The provider call carries every argument, including grep patterns, read
	// ranges, browser actions, and MCP payloads that Path/Argv do not represent.
	// Detector canonicalizes JSON so formatting and key order are irrelevant.
	w.observe(w.detector.ToolCall(call.Name, call.Input))
}

func (w *watcher) ToolEnd(call provider.ToolUse, req permission.Request, res tools.Result, took time.Duration) {
	w.inner.ToolEnd(call, req, res, took)
	w.observe(w.detector.ToolResult(call.Name, strings.Join(req.Argv, " "), res.Content, res.IsError))
}

// ToolBatchEnd is the single routing boundary for one model call's tools. It
// is invoked after parallel work has joined and the ordered results are in the
// session, so a rebind affects only the next model call.
func (w *watcher) ToolBatchEnd(ctx context.Context) {
	w.inner.ToolBatchEnd(ctx)
	w.assess(ctx)
}

func (w *watcher) Notice(level, text string) { w.inner.Notice(level, text) }

// VerifierFailures feeds a /watch run's failures into the same evidence
// stream the model's own test runs feed, then weighs a move the way a tool
// result does: it is called from the loop's goroutine at a round boundary,
// which is also the one safe moment to rebind the primary — no call is
// outstanding.
func (w *watcher) VerifierFailures(ctx context.Context, sigs []string) {
	w.observe(w.detector.VerifierFailures(sigs))
	w.assess(ctx)
}

func (w *watcher) TurnUsage(u session.Usage) {
	w.inner.TurnUsage(u)
	w.mu.Lock()
	w.lastUsage = u
	w.usageSeen = true
	w.mu.Unlock()
	w.sticky.CallServed()
}

func (w *watcher) observe(signals []route.Signal) {
	for _, s := range signals {
		w.sticky.Observe(s)
	}
}

func (w *watcher) assess(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return
	}
	move := w.sticky.Assess(w.maxRank)
	if w.isPaused() {
		// Assessed and discarded rather than never assessed: the policy's own
		// counters advance, so turning routing back on resumes from what the
		// session actually looks like instead of from a standing start.
		return
	}
	switch {
	case move.Direction != 0:
		if w.onMove == nil {
			return
		}
		bind, afterCommit, ok := w.onMove(ctx, move.ToRank, move.Rationale)
		if !ok || ctx.Err() != nil || !w.sticky.ApplyChecked(move, bind) {
			return
		}
		if afterCommit != nil {
			afterCommit()
		}
		w.mu.Lock()
		w.moves++
		w.mu.Unlock()
		// §8.1 renders the reason rather than logging it, and a target changing
		// under the user is exactly the case principle 3 is about.
		direction := "escalated"
		if move.Direction < 0 {
			direction = "stepped down"
		}
		w.inner.Notice("route", direction+": "+move.Rationale)

	case move.Held:
		// Saying that a switch was warranted and held is worth as much as the
		// switch itself: otherwise the dwell looks like the policy doing
		// nothing.
		w.inner.Notice("route", move.Rationale)

	case move.Boundary:
		w.inner.Notice("route", move.Rationale)
	}
}
