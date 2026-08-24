package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/prefix"
	"github.com/switchboard-code/switchboard/internal/provider"
	route "github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/schedule"
	"github.com/switchboard-code/switchboard/internal/session"
)

type repl struct {
	loop       *agent.Loop
	out        *renderer
	in         *bufio.Reader
	capability execution.Capability
	workspace  string

	config    *config.Config
	catalog   *catalog.Catalog
	tier      config.Tier
	providers *providers

	// route is what chose the starting target, when a router chose it. §8.1
	// renders this rather than logging it, because principle 3 requires the
	// user can see why.
	route         *route.Decision
	routeFeatures route.SessionFeatures
	sticky        *route.Sticky
	watcher       *watcher

	// budget is the shared ceiling the loop's gate reads; the REPL checks it
	// before an escalation the same way the TUI does.
	budget       *budgetState
	caches       *cacheSet
	startupNotes startupNoteReport

	// store creates the replacement session a compaction seeds. The REPL
	// could not compact at all without it, which is why a long scripted run
	// used to end at the provider's refusal rather than at a handoff.
	store *session.Store

	// schedules is the per-workspace reminder ledger behind /every, /at, and
	// /schedule. Nil when it could not load, with schedulesErr holding the
	// reason in a form the commands append to "schedules are unavailable".
	schedules    *schedule.Store
	schedulesErr string

	// callTokens is the size of the last request the provider actually saw.
	// Auto-compaction reads it at turn end for the same reason the TUI does:
	// it is the honest measure of occupancy, and a mid-turn estimate is known
	// to run low.
	callTokens int
	ctxWindow  int

	// restartRequired is set only after a staged session became visible without
	// a proven persistence barrier. The REPL must leave instead of accepting a
	// command on that adopted-but-uncertain log.
	restartRequired error
	// publishDurably is a deterministic fault seam for compaction adoption
	// tests. Runtime REPLs leave it nil and call Session.PublishDurably.
	publishDurably durableSessionPublisher
}

// moveTo rebinds the loop after the escalation policy changed the primary.
//
// A move that cannot be served leaves the target where it is: reporting a
// switch and then not making it would be worse than staying, because every
// later line would describe the wrong target.
func (r *repl) moveTo(ctx context.Context, rank int, why string) (func() bool, func(), bool) {
	if rank < 0 || rank >= len(r.config.Tiers) {
		return nil, nil, false
	}
	tier := r.config.Tiers[rank]
	probed, client, note, err := r.providers.probeTierFallbackFeasible(ctx, tier, func(candidate config.Tier) error {
		return checkMoveFeasible(r.loop, r.catalog, r.providers, r.budget, r.config.Destinations, candidate, rank)
	})
	if err != nil {
		r.out.Notice("warn", "staying on "+r.tier.ID+": "+err.Error())
		return nil, nil, false
	}
	if ctx.Err() != nil {
		return nil, nil, false
	}
	oldBinding := r.loop.Binding()
	targetChanged := oldBinding.Target.ID() != probed.Target.ID()
	abandoned := ""
	if targetChanged {
		abandoned = abandonedCacheNote(oldBinding.Cache, r.catalog, time.Now())
	}
	cache := r.caches.For(probed.Target, r.catalog)
	bind := func() bool {
		if err := persistRuntimeBindingFallback(r.loop.Session, probed, false, note); err != nil {
			r.out.Notice("warn", "the automatic tier move was not saved: "+err.Error())
			return false
		}
		r.tier = probed
		r.loop.Bind(agent.Binding{Provider: client, Target: probed.Target, Cache: cache})
		return true
	}
	after := func() {
		if note != "" {
			r.out.Notice("warn", note)
		}
		r.out.line(r.out.style(dim, "  now on "+r.tierLine()))
		if abandoned != "" {
			r.out.line(r.out.style(dim, "  "+cliText(abandoned)))
			_ = r.loop.Session.AppendNote("info", abandoned)
		}
	}
	return bind, after, true
}

func (r *repl) banner(sess *session.Session, resumed bool) {
	state := sess.State()

	r.out.line(r.out.style(bold, "switchboard") + " " + r.out.style(dim, r.tierLine()))
	r.out.line(r.out.style(dim, "  workspace  "+cliText(r.workspace)))
	r.out.line(r.out.style(dim, "  mode       "+string(r.loop.Perms.Mode())))
	r.out.line(r.out.style(dim, "  execution  "+cliText(r.loop.Perms.Execution().Summary())))
	r.out.line(r.out.style(dim, "  catalog    "+cliText(r.catalog.Revision)+" ("+cliText(r.catalog.Source)+")"))
	if r.route != nil {
		for _, line := range describeRoute(*r.route) {
			r.out.line(r.out.style(dim, cliText(line)))
		}
	}

	if resumed {
		r.out.line(r.out.style(dim, fmt.Sprintf("  session    %s, resumed with %d messages",
			state.ID, len(state.Messages))))
	} else {
		r.out.line(r.out.style(dim, "  session    "+state.ID))
	}

	if lost := sess.TruncatedBytes(); lost > 0 {
		r.out.line(r.out.style(red, fmt.Sprintf(
			"  recovered from an interrupted write; %d bytes at the end of the log were unreadable and were dropped", lost)))
	}

	r.out.line("")
	r.out.line(r.out.style(dim, "  /help for commands, /exit to leave"))
	r.out.line("")
	r.out.flush()
}

func (r *repl) tierLine() string {
	target := r.loop.Binding().Target.Display()
	if r.tier.Label != "" {
		return cliText(fmt.Sprintf("%s %s  %s", r.tier.ID, r.tier.Label, target))
	}
	return cliText(fmt.Sprintf("%s  %s", r.tier.ID, target))
}

// once runs a single prompt. It is what makes the phase-0 exit gate scriptable:
// a turn can be started, interrupted, and resumed without a terminal.
func (r *repl) once(ctx context.Context, prompt string) error {
	return r.onceAuthored(ctx, prompt, prompt)
}

func (r *repl) onceAuthored(ctx context.Context, prompt, authored string) error {
	opening, err := stampTurnOpening(r.loop.Session, turnOpeningAuthored(prompt, authored, nil))
	if err == nil {
		// -p promises one completed prompt. It may take the ordinary tool-use
		// rounds needed to answer that prompt, but must not append compaction and
		// an automatic continuation after the result the caller asked for.
		err = r.turnPreparedMessageMode(ctx, opening, false, false)
	}
	r.summary()
	return err
}

func (r *repl) interactive(ctx context.Context) error {
	for {
		// Due reminders fire here: the loop top is both "before each read"
		// and "after each completed turn", the only seams a line reader has.
		r.fireDueSchedules(ctx)
		if r.restartRequired != nil {
			return r.restartRequired
		}

		r.out.w.WriteString(r.out.style(bold, "› "))
		r.out.atLineTop = false
		r.out.flush()

		input, err := r.in.ReadString('\n')
		if errors.Is(err, io.EOF) {
			r.out.line("")
			return nil
		}
		if err != nil {
			return err
		}

		input = strings.TrimSpace(input)
		if input == "" {
			continue
		}
		if strings.HasPrefix(input, "/") {
			if done := r.command(ctx, input); done {
				if r.restartRequired != nil {
					return r.restartRequired
				}
				return nil
			}
			if r.restartRequired != nil {
				return r.restartRequired
			}
			continue
		}

		prompt, authored, images, ok := r.prepareInteractivePromptAuthored(input)
		if !ok {
			continue
		}
		if err := r.turnPreparedAuthored(ctx, prompt, authored, images, false); err != nil {
			var restart *publicationRestartRequiredError
			if errors.As(err, &restart) {
				return err
			}
			if errors.Is(err, context.Canceled) {
				r.out.Notice("warn", "turn cancelled; the session is intact and can continue")
				continue
			}
			if errors.Is(err, agent.ErrRoundLimit) {
				continue
			}
			r.out.Notice("error", err.Error())
		}
	}
}

// prepareInteractivePrompt performs the assembly before the outbound secret
// gate, so credentials inside an @mentioned file are guarded too.
func (r *repl) prepareInteractivePrompt(input string) (string, []provider.Image, bool) {
	prompt, _, images, ok := r.prepareInteractivePromptAuthored(input)
	return prompt, images, ok
}

// prepareInteractivePromptAuthored keeps the exact typed projection beside
// the provider-visible expansion. A later TUI replay must never attribute
// attached file contents or harness context to the user merely because both
// were persisted in the same opening message.
func (r *repl) prepareInteractivePromptAuthored(input string) (string, string, []provider.Image, bool) {
	authored := input
	prompt, images := expandPromptMentions(r.workspace, input)
	if leaks := credential.ScanPrompt(prompt); len(leaks) > 0 {
		gated := r.secretGate(prompt, leaks)
		if gated == "" {
			return "", "", nil, false
		}
		if gated != prompt {
			authored = credential.Redact(authored, leaks)
		}
		prompt = gated
	}
	return prompt, authored, images, true
}

// secretGate holds a key-shaped prompt behind a one-line question, the
// REPL's form of the TUI's dialog. Anything that is not a deliberate
// answer drops the prompt, because the default direction for a question
// about a credential has to be the safe one — and the question itself
// names kinds and prefixes only, never the match.
func (r *repl) secretGate(input string, leaks []credential.Leak) string {
	kinds := make([]string, len(leaks))
	for i, l := range leaks {
		kinds[i] = l.String()
	}
	r.out.Notice("warn", "the prompt contains "+strings.Join(kinds, ", "))
	r.out.w.WriteString("[r]edact and send, [s]end as typed, anything else drops it: ")
	r.out.atLineTop = false
	r.out.flush()

	answer, err := r.in.ReadString('\n')
	if err != nil {
		return ""
	}
	r.out.atLineTop = true
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "r", "redact":
		return credential.Redact(input, leaks)
	case "s", "send":
		return input
	}
	r.out.Notice("", "not sent; the prompt was dropped before anything left this machine")
	return ""
}

// turn runs one message with an interrupt handler bound to it. Ctrl-C cancels
// the turn and returns to the prompt rather than killing the process, because
// the session is resumable and the work already done is worth keeping.
func (r *repl) turn(ctx context.Context, input string) error {
	return r.turnPrepared(ctx, input, nil, false)
}

func (r *repl) turnPrepared(ctx context.Context, input string, images []provider.Image, fixedTier bool) error {
	return r.turnPreparedAuthored(ctx, input, input, images, fixedTier)
}

func (r *repl) turnPreparedAuthored(ctx context.Context, input, authored string, images []provider.Image, fixedTier bool) error {
	opening, err := stampTurnOpening(r.loop.Session, turnOpeningAuthored(input, authored, images))
	if err != nil {
		return err
	}
	return r.turnPreparedMessage(ctx, opening, fixedTier)
}

func (r *repl) turnPreparedSynthetic(ctx context.Context, input string, images []provider.Image, fixedTier bool) error {
	return r.turnPreparedSyntheticMode(ctx, input, images, fixedTier, false)
}

// turnPreparedSyntheticMode carries the one-shot post-compaction policy on the
// launch itself. It is deliberately not inferred from input: an identical
// prompt typed by the user is an ordinary turn, and a failed launch must not
// leave a broad bypass armed for some later turn.
func (r *repl) turnPreparedSyntheticMode(ctx context.Context, input string, images []provider.Image, fixedTier, suppressAutoCompact bool) error {
	opening, err := stampTurnOpening(r.loop.Session, syntheticTurnOpening(input, images))
	if err != nil {
		return err
	}
	return r.turnPreparedMessageMode(ctx, opening, fixedTier, !suppressAutoCompact)
}

// turnPreparedMessage runs an already-stamped opening. The exact message that
// routing and feasibility inspect is the one later appended and sent; in
// particular, image blocks and continuity metadata are never reconstructed.
func (r *repl) turnPreparedMessage(ctx context.Context, opening provider.Message, fixedTier bool) error {
	return r.turnPreparedMessageMode(ctx, opening, fixedTier, true)
}

func (r *repl) turnPreparedMessageMode(ctx context.Context, opening provider.Message, fixedTier, autoCompact bool) error {
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-sig:
			cancel()
		case <-done:
		}
	}()

	if _, onLadder := r.config.Tier(r.tier.ID); onLadder && !fixedTier {
		binding := r.loop.Binding()
		tier, client, note, plan, err := resolveUserTurn(turnCtx, r.loop, r.config, r.catalog, r.providers,
			r.budget, r.caches, r.sticky, r.tier, binding.Provider, opening, r.workspace)
		if err != nil {
			return err
		}
		if err := r.acceptTurnResolution(turnCtx, tier, client, note, plan); err != nil {
			return err
		}
	} else {
		if !fixedTier {
			r.route = nil
			plan := prospectiveTurnPlan(r.loop, r.sticky, opening, r.workspace)
			r.routeFeatures = plan.Features
			// An explicit or synthetic resumed target cannot reroute to an
			// unapproved rung, but it still owes every hard preflight invariant.
			// Probe this concrete parameterized identity first: /think changes the
			// full target ID, while live tool/vision evidence belongs to the probe
			// result for the target about to receive the request.
			probed, client, err := r.providers.probeTier(turnCtx, r.tier)
			if err != nil {
				return fmt.Errorf("the current target cannot serve the turn: %w", err)
			}
			if err := checkTurnFeasible(r.loop, r.catalog, r.providers, r.budget, r.config.Destinations, probed, 0, plan, opening); err != nil {
				return fmt.Errorf("the current target cannot serve the turn: %w", err)
			}
			if err := turnCtx.Err(); err != nil {
				return err
			}
			r.tier = probed
			r.loop.Bind(agent.Binding{Provider: client, Target: probed.Target, Cache: r.caches.For(probed.Target, r.catalog)})
		}
	}
	if r.watcher != nil {
		r.watcher.StartTurn()
	}

	before := r.loop.Session.State()
	usageWindow := r.loop.Session.BeginUsageWindow()
	startedOn := r.tier
	started := time.Now()

	err := r.loop.TurnMessage(turnCtx, opening)
	r.out.endTurn()
	r.recordRoute(opening.AuthoredText(), startedOn, before, usageWindow, started, err)
	r.route = nil

	// Turn end is where occupancy is known and where a session may be
	// replaced without cutting a turn in half.
	r.refreshCtxWindow()
	r.noteOccupancy()
	if err == nil && autoCompact {
		r.autoCompactIfFull(ctx)
		if r.restartRequired != nil {
			return r.restartRequired
		}
	}
	return err
}

// noteOccupancy reads the final provider receipt from the current turn rather
// than the session's cumulative usage ledger. Summing every historical call
// and comparing it to one request's context window would compact earlier on
// every turn. A completed endpoint may still report zero usage, so that exact
// event (and a locally refused turn with no event) falls back to the assembled
// request's local token estimate.
func (r *repl) noteOccupancy() {
	state := r.loop.Session.State()
	r.callTokens = 0
	if r.watcher != nil {
		if latest, ok := r.watcher.LastUsage(); ok {
			r.callTokens = latest.Usage.InputTokens + latest.Usage.CacheReadTokens + latest.Usage.CacheWriteTokens
		}
	}
	if r.callTokens == 0 {
		r.callTokens = prefix.RequestTokens(r.loop.Request(state.Messages))
	}
}

// acceptTurnResolution is the commit boundary between a live route probe and
// the session runtime. Cancellation wins even when an adapter ignores it and
// returns success late: no binding, sticky rank, or durable runtime record may
// move after the user cancelled the turn.
func (r *repl) acceptTurnResolution(ctx context.Context, tier config.Tier, client provider.Provider, note string, plan turnPlan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	changed := tier.ID != r.tier.ID || tier.Target.ID() != r.tier.Target.ID()
	pinned := r.sticky != nil && r.sticky.Pinned()
	if changed || note != "" {
		oldBinding := r.loop.Binding()
		targetChanged := oldBinding.Target.ID() != tier.Target.ID()
		abandoned := ""
		if targetChanged {
			abandoned = abandonedCacheNote(oldBinding.Cache, r.catalog, time.Now())
		}
		if err := persistRuntimeBindingFallback(r.loop.Session, tier, pinned, note); err != nil {
			return fmt.Errorf("saving automatic tier selection: %w", err)
		}
		if note != "" {
			r.out.Notice("warn", note)
		}
		if changed {
			r.tier = tier
			r.out.line(r.out.style(dim, "  now on "+r.tierLine()))
			if abandoned != "" {
				r.out.line(r.out.style(dim, "  "+cliText(abandoned)))
				_ = r.loop.Session.AppendNote("info", abandoned)
			}
		}
	}
	// A registry reset can rebuild the client for the same canonical target.
	// The live probe returned the prepared client for this turn, so install it at
	// the commit boundary even when no tier move (and therefore no durable target
	// transition) occurred.
	r.loop.Bind(agent.Binding{Provider: client, Target: tier.Target, Cache: r.caches.For(tier.Target, r.catalog)})
	if r.sticky != nil {
		r.sticky.Rebase(slices.IndexFunc(r.config.Tiers, func(candidate config.Tier) bool { return candidate.ID == r.tier.ID }))
	}
	r.route = &plan.Decision
	r.routeFeatures = plan.Features
	r.out.Notice("route", fmt.Sprintf("%s: %s", plan.Decision.Tier, plan.Decision.Rationale))
	return nil
}

// recordRoute writes §8.4's training signal for the turn that just ended.
//
// It is written from ordinary sessions rather than only from eval runs, because
// a corpus of deliberate measurements is a corpus of tasks somebody thought to
// write down, and the distribution that matters is the one the user actually
// works in.
//
// The outcome is recorded raw. §8.4 is explicit that an escalation is not a
// negative label and a clean completion is weak evidence of sufficiency and none
// of necessity, so turning any of this into a label is a decision for whoever
// trains on it, not one to bake in here.
func (r *repl) recordRoute(prompt string, startedOn config.Tier, before session.State, usageWindow session.UsageCursor, started time.Time, turnErr error) {
	moves := 0
	if r.watcher != nil {
		moves = r.watcher.MoveCount()
	}
	err := appendRouteRecord(r.loop.Session, prompt, startedOn, r.tier, before, usageWindow, started, turnErr, r.route, r.routeFeatures, moves)
	if err != nil {
		r.out.Notice("warn", "the routing record for this turn was not saved: "+err.Error())
	}
}

// appendRouteRecord is the UI-independent half of recordRoute, shared with the
// TUI: it derives the record and appends it, leaving error reporting to the
// caller's surface.
func appendRouteRecord(sess *session.Session, prompt string, startedOn, endedOn config.Tier, before session.State, usageWindow session.UsageCursor, started time.Time, turnErr error, routeDec *route.Decision, features route.SessionFeatures, moves int) error {
	wallTime := time.Since(started).Milliseconds()
	if wallTime < 0 {
		wallTime = 0
	}
	rec := session.Route{
		TurnDepth:         len(before.Messages),
		PromptTokens:      features.PromptTokens,
		ContextTokens:     features.ContextTokens,
		PriorFailures:     features.PriorFailures,
		TestFailures:      features.TestFailures,
		FilesInContext:    features.FilesInContext,
		DiffSize:          features.DiffSizeSoFar,
		DiffSizeKnown:     features.DiffSizeKnown,
		TestsInvolved:     features.TestsInvolved,
		PromptChars:       len(prompt),
		Languages:         append([]string(nil), features.RepoLanguages...),
		LastTurnEscalated: features.LastTurnEscalated,
		Tier:              startedOn.ID,
		Target:            startedOn.Target.ID(),
		Source:            "manual",
		WallTimeMS:        wallTime,
		Outcome:           string(route.Completed),
		// Route records are currently appended before a task-specific verifier
		// can be correlated with this exact turn. Say that explicitly; false/
		// false alone is ambiguous between "not run" and "ran and failed".
		VerificationStatus: session.RouteVerificationUnavailable,
	}
	if routeDec != nil {
		rec.Source = string(routeDec.Source)
		rec.Rationale = routeDec.Rationale
		rec.PolicyRevision = routeDec.PolicyRevision
	}
	rec.Escalations = moves
	moved := moves > 0 || endedOn.ID != startedOn.ID || endedOn.Target.ID() != startedOn.Target.ID()
	if moved {
		rec.EndedOn = endedOn.Target.ID()
		rec.EndedTier = endedOn.ID
		rec.Outcome = string(route.Escalated)
	}
	switch {
	case errors.Is(turnErr, context.Canceled):
		// A cancelled turn is abandonment, which §8.4 censors rather than
		// counting against the target: the user walked away and told you
		// nothing about the choice.
		rec.Outcome = string(route.Abandoned)
		rec.FailureKind = session.RouteFailureCancelled
	case turnErr != nil:
		rec.FailureKind = routeFailureKind(turnErr)
		if !moved {
			rec.Outcome = string(route.Failed)
		}
	}

	return sess.AppendRouteWithUsage(usageWindow, rec)
}

func routeFailureKind(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.Canceled):
		return session.RouteFailureCancelled
	case errors.Is(err, agent.ErrRoundLimit):
		return session.RouteFailureRoundLimit
	case errors.Is(err, errBudgetUnavailable):
		return session.RouteFailureBudget
	case isContextWindowError(err):
		return session.RouteFailureContext
	case errors.Is(err, agent.ErrProviderCall):
		return session.RouteFailureProvider
	case errors.Is(err, provider.ErrStreamIncomplete):
		return session.RouteFailureProvider
	}
	var capability *provider.CapabilityError
	var protocol *provider.ProtocolError
	var api *provider.APIError
	if errors.As(err, &capability) || errors.As(err, &protocol) || errors.As(err, &api) {
		return session.RouteFailureProvider
	}
	return session.RouteFailureInternal
}

func isContextWindowError(err error) bool {
	var contextWindow *agent.ContextWindowError
	return errors.As(err, &contextWindow)
}

// command handles a slash command and reports whether the REPL should exit.
func (r *repl) command(ctx context.Context, input string) bool {
	name, rest, _ := strings.Cut(strings.TrimPrefix(input, "/"), " ")
	rest = strings.TrimSpace(rest)

	// A bare tier name permanently pins it. With a prompt it borrows that rung
	// for exactly one fully assembled turn, then restores the prior binding and
	// routing policy byte-for-byte at the behavioral level.
	if _, ok := r.config.Tier(name); ok {
		if rest == "" {
			r.switchTier(ctx, name)
		} else if prompt, authored, images, send := r.prepareInteractivePromptAuthored(rest); send {
			if err := r.turnOnTierAuthored(ctx, name, prompt, authored, images); err != nil {
				if errors.Is(err, context.Canceled) {
					r.out.Notice("warn", "turn cancelled; the session is intact and can continue")
				} else if !errors.Is(err, agent.ErrRoundLimit) {
					r.out.Notice("error", err.Error())
				}
			}
		}
		r.out.flush()
		return false
	}

	switch name {
	case "exit", "quit":
		return true

	case "help":
		r.out.line("  /tN                                       permanently pin tier N, for example /t2")
		r.out.line("  /tN <prompt>                              run one prompt there, then restore")
		r.out.line("  /tier auto                                return to automatic per-turn routing")
		r.out.line("  /routing [on|off]                         show or change whether the policy may move the rung")
		r.out.line("  /tiers                                    show the configured ladder")
		r.out.line("  /mode [plan|default|acceptEdits|auto|yolo|bypass]  show or change permission mode")
		r.out.line("  /cost                                     tokens and cost for this session")
		r.out.line("  /session                                  session id, target, and message count")
		r.out.line("  /sandbox [off|on|auto|status]             show or change command confinement")
		r.out.line("  /compact [current objective]              summarize this session; an objective safely anchors legacy history")
		r.out.line("  /every <interval> <prompt>                fire a prompt on an interval while sb runs")
		r.out.line("  /at <HH:MM> <prompt>                      fire a prompt once at a local clock time")
		r.out.line("  /schedule [cancel <id>]                   the workspace's scheduled prompts")
		r.out.line("  /doctor extensions                        every startup extension diagnostic")
		r.out.line("  /exit                                     leave")

	case "compact":
		r.compact(ctx, rest)
		if r.restartRequired != nil {
			return true
		}

	case "every":
		r.scheduleEvery(rest)

	case "at":
		r.scheduleAt(rest)

	case "schedule":
		r.scheduleCommand(rest)

	case "tier":
		if rest == "" {
			r.out.line("  " + r.tierLine())
			break
		}
		if rest == "auto" {
			if err := persistAutomaticPosture(r.loop.Session, r.tier); err != nil {
				r.out.Notice("error", "automatic routing was not enabled: "+err.Error())
				break
			}
			if r.sticky != nil {
				r.sticky.Unpin()
			}
			if r.config.RouteAutoOn() {
				r.out.line("  automatic per-turn routing resumed from " + cliText(r.tier.ID))
			} else {
				r.out.line("  pin removed; routing is off, so the rung still changes only when you change it (/routing on resumes)")
			}
			break
		}
		r.switchTier(ctx, rest)

	case "routing":
		switch strings.ToLower(rest) {
		case "on", "off":
			on := strings.ToLower(rest) == "on"
			if err := r.config.SetRouteAutoAndSave(on); err != nil {
				r.out.Notice("error", "saving the routing setting failed: "+err.Error())
				break
			}
			if r.watcher != nil {
				r.watcher.setPaused(!on)
			}
			if on {
				r.out.line("  routing on: the policy may move the primary on its own signals")
			} else {
				r.out.line("  routing off: the rung changes only when you change it")
			}
		default:
			r.out.Notice("error", "routing takes on, off, or status")
		case "", "status":
			if r.config.RouteAutoOn() {
				r.out.line("  routing is on; /routing off holds the current rung")
			} else {
				r.out.line("  routing is off; /routing on lets the policy move the primary again")
			}
		}

	case "tiers":
		if len(r.config.Tiers) == 0 {
			r.out.line("  no tiers configured in " + cliText(r.config.Path))
			break
		}
		for _, t := range r.config.Tiers {
			marker := "  "
			if t.ID == r.tier.ID {
				marker = "* "
			}
			r.out.line(marker + cliText(t.String()))
		}

	case "mode":
		if rest == "" {
			r.out.line("  " + string(r.loop.Perms.Mode()))
			break
		}
		mode, err := permission.ParseMode(rest)
		if err != nil {
			r.out.Notice("error", err.Error())
			break
		}
		r.loop.Perms.SetMode(mode)
		r.out.line("  mode is now " + string(mode))
		if mode == permission.ModeYOLO {
			warning := "FULL HOST ACCESS: edits, commands, and external tools run without asking; commands are unsandboxed with host filesystem and network reach. Only an explicit deny rule or a no you gave this session still refuses"
			if r.capability.Platform == "windows" {
				warning += ". Windows descendant processes may survive cancellation"
			}
			r.out.Notice("warn", warning)
		}
		if mode == permission.ModeAuto {
			r.out.line(r.out.style(dim, "  ordinary workspace edits apply; with an active verified sandbox, ordinary non-sensitive commands go to the cheap approver; host-direct, external, sensitive, uncertain, and host-loopback-sandbox actions ask you"))
		}
		if mode == permission.ModeBypass && !r.loop.Perms.Execution().SandboxActive() {
			// Saying this once, plainly, beats letting the user discover it by
			// being prompted anyway and reading it as a bug (§19.3).
			r.out.line(r.out.style(dim, "  commands will still be approved one at a time: bypass needs an active verified sandbox"))
		} else if mode == permission.ModeBypass {
			policy := r.loop.Perms.Execution().CommandPolicy(false)
			if policy.HostLoopbackShared || policy.HostIPCShared {
				r.out.line(r.out.style(dim, "  commands will still ask: host-local network or IPC services retain authority outside this sandbox; bypass is promptless only when both are isolated"))
			}
		}

	case "cost":
		r.summary()

	case "session":
		state := r.loop.Session.State()
		health := session.ResumeHealthForState(state, r.loop.Session.TruncatedBytes() > 0)
		r.out.line("  " + cliText(state.ID))
		r.out.line("  target   " + cliText(r.loop.Binding().Target.Display()))
		r.out.line("  catalog  " + cliText(state.CatalogRevision))
		r.out.line("  messages " + fmt.Sprint(len(state.Messages)))
		r.out.line("  health   " + cliText(resumeHealthChips(health, false)))
		r.out.line("  log      " + cliText(r.loop.Session.Path()))

	case "sandbox":
		controller := r.loop.Perms.Execution()
		if rest == "" || rest == "status" {
			r.out.line("  platform  " + cliText(r.capability.Platform))
			r.out.line("  mechanism " + string(r.capability.Mechanism))
			r.out.line("  requested " + string(controller.SandboxMode()))
			r.out.line("  " + cliText(controller.Summary()))
			break
		}
		mode, err := execution.ParseSandboxMode(rest)
		if err != nil {
			r.out.Notice("error", err.Error())
			break
		}
		if r.loop.Perms.Mode() == permission.ModeYOLO && mode != execution.SandboxOff {
			r.out.Notice("error", "yolo mode requires the sandbox to stay off; leave yolo before enabling confinement")
			break
		}
		if err := controller.SetSandbox(mode); err != nil {
			r.out.Notice("error", err.Error())
			break
		}
		r.config.Sandbox = mode
		r.out.line("  " + cliText(controller.Summary()))
		if err := r.config.Save(); err != nil {
			r.out.Notice("warn", "sandbox changed for this process, but the config was not saved: "+err.Error())
		}

	case "doctor":
		if rest != "extensions" {
			r.out.Notice("error", "usage: /doctor extensions")
			break
		}
		writeStartupNoteDetails(r.out, r.startupNotes)

	default:
		r.out.Notice("error", "unknown command "+name+"; try /help")
	}

	r.out.flush()
	return false
}

func (r *repl) turnOnTier(ctx context.Context, id, prompt string, images []provider.Image) error {
	return r.turnOnTierAuthored(ctx, id, prompt, prompt, images)
}

func (r *repl) turnOnTierAuthored(ctx context.Context, id, prompt, authored string, images []provider.Image) error {
	requested, ok := r.config.Tier(id)
	if !ok {
		return fmt.Errorf("no tier %s is configured; try /tiers", id)
	}
	opening, err := stampTurnOpening(r.loop.Session, turnOpeningAuthored(prompt, authored, images))
	if err != nil {
		return err
	}
	plan := prospectiveTurnPlan(r.loop, r.sticky, opening, r.workspace)
	rank := slices.IndexFunc(r.config.Tiers, func(candidate config.Tier) bool { return candidate.ID == requested.ID })
	probed, client, note, err := r.providers.probeTierFallbackFeasible(ctx, requested, func(candidate config.Tier) error {
		return checkTurnFeasible(r.loop, r.catalog, r.providers, r.budget, r.config.Destinations, candidate, rank, plan, opening)
	})
	if err != nil {
		return fmt.Errorf("the requested tier %s cannot serve the turn: %w", id, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	retargetTurnPlan(&plan, r.loop, r.catalog, r.caches, probed, rank, opening)
	if note != "" {
		if err := r.loop.Session.AppendNote("warn", note); err != nil {
			return fmt.Errorf("the fallback substitution was not recorded, so the turn was not sent: %w", err)
		}
		r.out.Notice("warn", note)
	}

	priorTier := r.tier
	priorBinding := r.loop.Binding()
	var stickyState route.StickySnapshot
	if r.sticky != nil {
		stickyState = r.sticky.Snapshot()
		r.sticky.Pin(rank)
	}
	r.tier = probed
	r.loop.Bind(agent.Binding{Provider: client, Target: probed.Target, Cache: r.caches.For(probed.Target, r.catalog)})
	defer func() {
		r.tier = priorTier
		r.loop.Bind(priorBinding)
		if r.sticky != nil {
			r.sticky.Restore(stickyState)
		}
	}()

	r.route = &route.Decision{
		Tier: probed.ID, Target: probed.Target.ID(), Confidence: 1,
		Source: route.SourceUserPin, Rationale: "one-turn tier override requested by you",
		PolicyRevision: route.PolicyRevision, EstimatedCost: plan.Decision.EstimatedCost,
	}
	r.routeFeatures = plan.Features
	r.out.line(r.out.style(dim, "  running this turn on "+r.tierLine()))
	return r.turnPreparedMessage(ctx, opening, true)
}

func (r *repl) switchTier(ctx context.Context, id string) {
	tier, ok := r.config.Tier(id)
	if !ok {
		r.out.Notice("error", "no tier "+id+" is configured; try /tiers")
		return
	}
	if tier.ID == r.tier.ID && tier.Target.ID() == r.tier.Target.ID() {
		if err := persistRuntimeBinding(r.loop.Session, r.tier, true); err != nil {
			r.out.Notice("error", "tier pin was not saved: "+err.Error())
			return
		}
		if r.sticky != nil {
			if rank := slices.IndexFunc(r.config.Tiers, func(candidate config.Tier) bool { return candidate.ID == r.tier.ID }); rank >= 0 {
				r.sticky.Pin(rank)
			}
		}
		r.out.line("  already on " + r.tierLine())
		return
	}

	probed, client, note, err := r.providers.probeTierFallback(ctx, tier)
	if err != nil {
		r.out.Notice("error", err.Error())
		return
	}
	// The runtime binding is the state transition; make it durable before
	// publishing ancillary fallback notes or changing the live binding.
	if err := persistRuntimeBindingFallback(r.loop.Session, probed, true, note); err != nil {
		r.out.Notice("error", "tier switch was not saved: "+err.Error())
		return
	}
	if note != "" {
		r.out.Notice("warn", note)
	}

	oldBinding := r.loop.Binding()
	targetChanged := oldBinding.Target.ID() != probed.Target.ID()
	r.tier = probed
	// A tier may cross providers, so the adapter moves with the target. So does
	// the cache: markers, minimums, and observed state all belong to a target,
	// and carrying one target's tracker onto another would attribute its cache
	// to a server that never held it.
	abandoned := ""
	if targetChanged {
		abandoned = abandonedCacheNote(oldBinding.Cache, r.catalog, time.Now())
	}
	r.loop.Bind(agent.Binding{Provider: client, Target: probed.Target, Cache: r.caches.For(probed.Target, r.catalog)})
	if r.sticky != nil {
		rank := slices.IndexFunc(r.config.Tiers, func(candidate config.Tier) bool { return candidate.ID == probed.ID })
		if rank >= 0 {
			r.sticky.Pin(rank)
		}
	}
	r.out.line("  now on " + r.tierLine())

	// Cache state is scoped to a target, so a switch abandons whatever was
	// warm on the old one. When that warmth can be priced honestly the
	// modeled number is the note; otherwise the fact is stated without one.
	if abandoned != "" {
		r.out.line(r.out.style(dim, "  "+cliText(abandoned)))
	} else if targetChanged {
		if info, _, ok := r.catalog.Lookup(probed.Target); ok && !info.Free() {
			r.out.line(r.out.style(dim, "  a target switch leaves the previous target's cache behind"))
		}
	}
}

// summary reports tokens and, where the catalog can price them, dollars. The
// figure is an estimate against a named catalog revision and a reconciliation
// aid, never a substitute for the provider's invoice (§15).
func (r *repl) summary() {
	for _, line := range summaryLines(r.loop.Session.State(), r.catalog, r.loop.Binding().Target) {
		r.out.line(r.out.style(dim, cliText(line)))
	}
	r.out.flush()
}

// summaryLines is the UI-independent half of the cost report, shared with the
// TUI's /cost.
func summaryLines(state session.State, cat *catalog.Catalog, target provider.RouteTarget) []string {
	line := fmt.Sprintf("  %d model calls, %d tokens in, %d tokens out",
		state.Calls, state.Usage.InputTokens, state.Usage.OutputTokens)
	if state.Usage.CacheReadTokens > 0 || state.Usage.CacheWriteTokens > 0 {
		line += fmt.Sprintf(", %d cache read, %d cache write",
			state.Usage.CacheReadTokens, state.Usage.CacheWriteTokens)
	}
	lines := []string{line}

	// The three zero-dollar meterings stay distinct here for the same reason
	// they are distinct in the catalog (§4): a local model consumed nothing
	// scarce, a plan target consumed quota, and reporting either as the other
	// tells the user the wrong thing about what just ran out.
	info, _, ok := cat.Lookup(target)
	accounted := catalog.Money(state.AccountedCostMicroUSD())
	routedKinds, routedKindsKnown := routedMeteringKinds(cat, state)
	switch {
	case state.ExternalCostMicroUSD > 0:
		lines = append(lines, fmt.Sprintf("  estimated %s including delegate/race work against recorded catalog data",
			accounted))
	case accounted > 0:
		lines = append(lines, fmt.Sprintf("  estimated %s across routed provider work against catalog %s",
			accounted, state.CatalogRevision))
	case routedKindsKnown:
		if len(routedKinds) > 1 {
			lines = append(lines, "  mixed metering across routed calls: "+strings.Join(routedKinds, " + "))
			break
		}
		switch routedKinds[0] {
		case "local":
			lines = append(lines, "  runs locally, so there is nothing to bill")
		case "plan":
			lines = append(lines, "  billed as a plan; quota, not dollars, is what this consumed")
		case "dollar-metered":
			lines = append(lines, "  dollar-metered routed calls rounded to "+accounted.String())
		case "no per-token cost":
			lines = append(lines, "  no per-token cost recorded for the routed calls")
		default:
			lines = append(lines, "  routed calls include a target the catalog could not price")
		}
	case !ok:
		lines = append(lines, "  no catalog entry for this target, so nothing was priced")
	case info.Metering == catalog.Local:
		lines = append(lines, "  runs locally, so there is nothing to bill")
	case info.Metering == catalog.Plan:
		lines = append(lines, "  billed as a plan; quota, not dollars, is what this consumed")
	case info.Free():
		lines = append(lines, "  no per-token cost recorded for this target")
	default:
		lines = append(lines, fmt.Sprintf("  estimated %s against catalog %s",
			catalog.Money(state.AccountedCostMicroUSD()), state.CatalogRevision))
	}
	if state.RetryReserveMicroUSD > 0 {
		lines = append(lines, fmt.Sprintf("  retry reserve %s for failed or unsettled provider attempts (not observed cost)",
			catalog.Money(state.RetryReserveMicroUSD)))
	}
	return lines
}
