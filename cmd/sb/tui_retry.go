package main

// /retry: take the last turn back and run it again, optionally on another
// rung. It is a composition of things the session model already guarantees,
// not a new mechanism: the turn's file changes revert through the checkpoint
// recorder (a restored file forces a re-read, the /undo contract), the
// conversation goes back by forking at the turn's opening message (the
// original log is read, never written, and stays resumable), and the same
// opening replays byte-for-byte — the recorded message, not a re-expansion,
// so the retried rung reads exactly what the first one read, files as they
// were, gate already passed. That is what makes the rerun a controlled
// comparison instead of a similar question asked twice.
//
// What it does not take back is said out loud: side effects of commands the
// discarded turn ran are outside the checkpoint boundary, the same limit
// /undo states.
//
// The set-aside answer is labelled only after the fork is adopted and the
// staged file inverse commits: §8.4's user_corrected is exactly this event,
// recorded on the source log where the answer lives. A failed pause, fork,
// stale checkpoint, cancelled operation, or failed adoption leaves that source
// and the post-turn workspace untouched and unlabelled.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

type retryStartMsg struct {
	generation uint64
	opening    provider.Message
	prompt     string // display-only projection; replay always uses opening
	tier       string // empty reruns on the sitting rung
	// openingRecorded means a crash landed the opening WAL record before the
	// execution-start seam. The agent resumes after it without appending twice.
	openingRecorded bool
}

type retryAdoption struct {
	source          *session.Session
	undo            *checkpoint.PreparedUndo
	checkpointKnown bool
	destination     string
}

const retryRecoveryGuardText = "an interrupted /retry owns this session; automatic replay is withheld to avoid duplicating provider or tool work. Use /retry abandon to keep the child without replay; then /resume can return to the set-aside source"

func (m *tuiModel) retryRecoveryExists() bool {
	if m == nil || m.app == nil || m.app.loop == nil || m.app.loop.Session == nil {
		return false
	}
	return m.app.loop.Session.State().RetryIntent != nil
}

func (m *tuiModel) retryRecoveryBlocksNewWork() bool {
	return m.retryRecoveryExists() && !m.busy && !m.turnPlanning
}

// retryRecoveryCommandAllowed keeps an unresolved execution handoff inert.
// Inspection and the two explicit recovery exits remain available; commands
// that append, mutate the workspace, launch external work, or enqueue another
// provider turn stay closed until the handoff is durably resolved.
func retryRecoveryCommandAllowed(name, args string) bool {
	if name == "retry" {
		return strings.TrimSpace(args) == "abandon"
	}
	switch name {
	case "exit", "quit", "help", "recap", "session",
		"tiers", "ladder", "why", "races", "cost", "usage", "stats", "find", "cache",
		"hooks", "permissions", "agents", "skills", "files", "search", "lsp", "outline",
		"symbols", "problems", "definition", "references", "diff", "review", "changes",
		"blame", "mistakes":
		return true
	default:
		return false
	}
}

func (r *retryAdoption) abortPrepared() error {
	if r == nil || r.undo == nil {
		return nil
	}
	return r.undo.AbortDurable()
}

func cmdRetry(m *tuiModel, args string) tea.Cmd {
	args = strings.TrimSpace(args)
	if args == "abandon" {
		intent := m.app.loop.Session.State().RetryIntent
		if intent == nil {
			return noticeCmd("", "no interrupted retry handoff is waiting for a recovery decision")
		}
		if err := m.app.loop.Session.AbandonRetryIntent(intent.ID); err != nil {
			return m.stopAfterRetryIntentResolutionFailure(err, errors.New("explicit /retry abandon was requested"))
		}
		notice := noticeCmd("warn", "interrupted retry handoff abandoned; this child stays as recorded and no work was replayed")
		return m.releaseDeferredStartup(func() tea.Cmd { return notice })
	}
	if m.retryRecoveryExists() {
		return noticeCmd("warn", retryRecoveryGuardText)
	}
	var dest config.Tier
	if args != "" {
		t, ok := m.app.config.Tier(args)
		if !ok {
			return noticeCmd("error", "no tier "+args+" is configured; try /tiers")
		}
		dest = t
	}

	state := m.app.loop.Session.State()
	last := lastTurnOpening(state.Messages)
	if last < 0 {
		return noticeCmd("", "nothing to retry; the session has no completed turn")
	}
	opening := provider.CloneMessage(state.Messages[last])
	prompt, authoredKnown := opening.AuthoredProjection()
	if !authoredKnown {
		return noticeCmd("error", "retry refused: this legacy turn does not record its exact authored wording; its provider-expanded opening will not be shown or replayed as user-authored")
	}
	if prompt == "" {
		return noticeCmd("error", "the last turn's opening carries no text to replay")
	}

	// Bind the only checkpoint scope /retry is allowed to consume before any
	// asynchronous work. An empty current scope is meaningful: this turn changed
	// no files and must not fall through to an older same-label mutation.
	checkpointTurn := checkpoint.TurnIdentity{SessionID: state.ID, OpeningMessage: last}
	recorder := m.app.undo
	checkpointKnown := false
	if recorder != nil {
		if current, ok := recorder.CurrentTurn(checkpointTurn); ok {
			checkpointKnown = true
			if current.Partial {
				which := ""
				if len(current.Skipped) > 0 {
					which = " (no bounded pre-image for " + boundedRetryPaths(current.Skipped) + ")"
				}
				text := "retry stopped before changing files or running another model: the turn's exact checkpoint is partial" + which + "; use /undo to review and explicitly consume that partial restore"
				_ = m.app.loop.Session.AppendNote("warn", text)
				return noticeCmd("error", text)
			}
		}
	}

	ctx, generation, sourceID, err := m.startOperation("retry")
	if err != nil {
		return noticeCmd("warn", err.Error())
	}

	destID := m.app.tier.ID
	intentTier := m.app.tier
	if dest.ID != "" {
		destID = dest.ID
		intentTier = dest
	}
	intentTarget, intentTierSet := retryTierIdentity(intentTier)
	sess := m.app.loop.Session
	id := sess.ID()
	app := m.app
	start := func() tea.Msg {
		return retryStartMsg{opening: provider.CloneMessage(opening), prompt: prompt, tier: dest.ID}
	}
	return m.ownOperationCmd(generation, func() tea.Msg {
		pause := pauseAdvisorLedger
		if app.retryPause != nil {
			pause = app.retryPause
		}
		release, err := pause(ctx, app)
		if err != nil {
			return sessionSwapMsg{err: fmt.Errorf("waiting for the advisor ledger before retry: %w", err), operation: generation, sourceID: sourceID}
		}
		if err := ctx.Err(); err != nil {
			return sessionSwapMsg{err: err, release: release, operation: generation, sourceID: sourceID}
		}
		// A retry-specific fork can keep an empty conversation for the first
		// turn, but it always carries the source's budget ledger. Repeated retry
		// therefore cannot make already-sent requests disappear from a ceiling.
		// It stays undiscoverable until the staged file inverse and session
		// publication commit together in onSessionSwap.
		fork := func(store *session.Store, source *session.Session, keep int) (*session.Session, error) {
			return store.ForkSessionForRetryStaged(source, keep)
		}
		if app.retryFork != nil {
			fork = app.retryFork
		}
		forked, err := fork(app.store, sess, last)
		if err != nil {
			return sessionSwapMsg{err: err, release: release, operation: generation, sourceID: sourceID}
		}
		var prepared *checkpoint.PreparedUndo
		failFork := func(err error) tea.Msg {
			if prepared != nil {
				err = errors.Join(err, prepared.AbortDurable())
			}
			if closeErr := forked.CloseDiscardingStaged(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("discarding staged retry child: %w", closeErr))
			}
			return sessionSwapMsg{err: err, release: release, operation: generation, sourceID: sourceID}
		}
		if err := ctx.Err(); err != nil {
			return failFork(err)
		}
		if _, err := forked.AppendRetryIntent(id, last, opening, destID, intentTarget, intentTierSet); err != nil {
			return failFork(fmt.Errorf("saving the retry replay handoff before publication: %w", err))
		}
		journalDir, dirErr := app.store.WorkspaceDir(app.workspace)
		if dirErr != nil {
			return failFork(fmt.Errorf("opening the retry recovery directory: %w", dirErr))
		}
		childIdentity, identityErr := forked.PublicationRecoveryIdentity()
		if identityErr != nil {
			return failFork(fmt.Errorf("binding the staged retry child to recovery: %w", identityErr))
		}
		transactionRecorder := recorder
		if !checkpointKnown {
			// Publication needs the same write-ahead decision even when no exact
			// file checkpoint exists. An empty private recorder contributes no
			// restore claims; it only keeps the child/journal commit atomic.
			transactionRecorder = checkpoint.NewRecorder()
			transactionRecorder.BeginTurn(checkpointTurn.SessionID, checkpointTurn.OpeningMessage, prompt)
		}
		prepared, err = transactionRecorder.PrepareDurableUndoCurrent(
			checkpointTurn, journalDir, app.workspace, forked.Path(), childIdentity,
		)
		if err != nil {
			return failFork(fmt.Errorf("preparing the retry publication transaction durably: %w", err))
		}
		if err := ctx.Err(); err != nil {
			return failFork(err)
		}
		return sessionSwapMsg{sess: forked, tier: app.tier, client: app.loop.Binding().Provider, fresh: last == 0,
			note: fmt.Sprintf("retrying the last turn on %s; the set-aside answer stays in %s, /resume %s returns to it", destID, id, id), andThen: start, release: release,
			continueTurn: true, operation: generation, sourceID: sourceID, preserveRuntimeTarget: true,
			retry: &retryAdoption{source: sess, undo: prepared, checkpointKnown: checkpointKnown, destination: destID}}
	})
}

// retryStart launches the replay once the swap has landed. A named rung goes
// through the /tN machinery — probe first, one turn there, then restore —
// because a retry elsewhere is exactly a one-shot override with a recorded
// prompt.
func (m *tuiModel) retryStart(msg retryStartMsg) tea.Cmd {
	// Production continuations arrive with planning ownership claimed by
	// onSessionSwap. The zero-generation branch keeps direct legacy/test calls
	// fail-closed without weakening that owned boundary.
	if msg.generation == 0 {
		if !m.busy {
			return noticeCmd("warn", "retry continuation arrived without turn ownership")
		}
		return noticeCmd("warn", "a turn started before the retry could; /retry again when it finishes")
	}
	if msg.generation != m.turnGeneration || !m.turnPlanning || m.turnCtx == nil {
		return nil
	}
	refuse := func(text string) tea.Cmd {
		refusal := errors.New(text)
		// Validation can refuse a recovered pending handoff before this process
		// claims execution ownership. Keep that expected recovery state available
		// for /retry abandon. Once activeRetryIntent is set, however, refusal owns
		// a durable abandonment append; failure at that seam must stop the process.
		if m.activeRetryIntent != "" {
			if err := m.resolveActiveRetryIntent(); err != nil {
				return m.stopAfterRetryIntentResolutionFailure(err, refusal)
			}
		}
		m.finishPlanning()
		m.addNotice("error", text)
		if m.retryRecoveryExists() {
			return nil
		}
		return m.releaseDeferredStartup(m.nextQueuedTurn)
	}
	opening := provider.CloneMessage(msg.opening)
	if !msg.openingRecorded {
		var err error
		opening, err = stampRecordedTurnOpening(m.app.loop.Session, opening)
		if err != nil {
			return refuse("retry refused: " + err.Error())
		}
	}
	prompt, authoredKnown := opening.AuthoredProjection()
	if !authoredKnown {
		return refuse("retry refused: the recorded opening does not carry an exact authored projection; provider-expanded content will not be painted as user-authored")
	}
	if prompt == "" {
		return refuse("retry refused: the recorded opening carries no authored text")
	}
	intent := m.app.loop.Session.State().RetryIntent
	if intent == nil || intent.Status != session.RetryIntentPending {
		return refuse("retry refused: the published child has no pending durable replay handoff")
	}
	matches, err := session.RetryIntentOpeningMatches(*intent, opening)
	if err != nil || !matches {
		return refuse("retry refused: the source opening no longer matches its durable replay handoff")
	}
	if msg.openingRecorded && opening.RetryIntentID != intent.ID {
		return refuse("retry refused: the recorded child opening is not bound to its durable replay handoff")
	}
	destination := m.app.tier.ID
	destinationTier := m.app.tier
	if msg.tier != "" {
		destination = msg.tier
		var ok bool
		destinationTier, ok = m.app.config.Tier(msg.tier)
		if !ok {
			return refuse("no tier " + msg.tier + " is configured; try /tiers")
		}
	}
	if intent.Tier != destination {
		return refuse("retry refused: the requested tier no longer matches its durable replay handoff")
	}
	target, tierSet := retryTierIdentity(destinationTier)
	if intent.TierTarget != target || intent.TierSetSHA256 != tierSet {
		return refuse("retry refused: the destination tier or its ordered fallbacks changed after the durable replay handoff")
	}
	m.activeRetryIntent = intent.ID
	m.resumeRetryOpening = msg.openingRecorded
	if !msg.openingRecorded {
		m.addUser(prompt)
	}
	if msg.tier != "" && msg.tier != m.app.tier.ID {
		tier, ok := m.app.config.Tier(msg.tier)
		if !ok {
			return refuse("no tier " + msg.tier + " is configured; try /tiers")
		}
		app := m.app
		ctx, generation := m.turnCtx, msg.generation
		sticky := app.sticky
		return func() tea.Msg {
			result := overrideProbeMsg{generation: generation, opening: opening, prompt: prompt}
			plan := prospectiveTurnPlan(app.loop, sticky, opening, app.workspace)
			result.plan = plan
			rank := app.rankOf(tier)
			if rank < 0 {
				result.err = fmt.Errorf("the requested tier %s is not on the configured ladder", tier.ID)
				return result
			}
			probed, client, note, err := app.providers.probeTierFallbackFeasible(ctx, tier, func(candidate config.Tier) error {
				return checkTurnFeasible(app.loop, app.catalog, app.providers, app.budget, app.config.Destinations, candidate, rank, plan, opening)
			})
			if err != nil {
				result.err = fmt.Errorf("the requested tier %s cannot serve the turn: %w", tier.ID, err)
				return result
			}
			if err := ctx.Err(); err != nil {
				result.err = err
				return result
			}
			retargetTurnPlan(&plan, app.loop, app.catalog, app.caches, probed, rank, opening)
			result.plan = plan
			result.tier, result.client, result.note = probed, client, note
			return result
		}
	}
	m.turnPlanning = false
	m.beginTurn(prompt)
	m.launchModelTurn(opening)
	return m.spin.Tick
}

// lastTurnOpening finds the user message that opened the final turn. Not
// every user-role message opens one: advice and watch reports inject as
// user-role mid-turn, and the log marks them Injected for exactly this
// reader. A user message behind a tool-result tail is otherwise a real
// opening — a cancelled or round-limited turn ends on its results, and the
// next prompt follows them — except in a log written before the marker
// existed, where an injection is only recognizable by the label it leads
// with.
func lastTurnOpening(messages []provider.Message) int {
	last := -1
	for idx, msg := range messages {
		if msg.Role != provider.RoleUser || msg.Injected {
			continue
		}
		if idx > 0 && messages[idx-1].Role == provider.RoleTool && injectionShaped(msg) {
			continue
		}
		last = idx
	}
	return last
}

// injectionShaped recognizes an unmarked injection by the label its text
// leads with. Watch folds ride behind the typed prompt, so an opening never
// leads with "[watch]"; an advice fold does lead with "[advisor]", and an
// opening carrying one behind a tool-result tail — an interrupted turn,
// then a prompt typed over pending advice, in a log too old to carry the
// marker — is misread as an injection. That corner falls back to an earlier
// opening, and the source session /retry never writes is the recovery.
func injectionShaped(msg provider.Message) bool {
	text := msg.AuthoredText()
	return strings.HasPrefix(text, "[advisor]") || strings.HasPrefix(text, "[watch]") || strings.HasPrefix(text, "[steer]")
}

func boundedRetryPaths(paths []string) string {
	const limit = 4
	shown := paths
	if len(shown) > limit {
		shown = shown[:limit]
	}
	text := strings.Join(shown, ", ")
	if hidden := len(paths) - len(shown); hidden > 0 {
		text += fmt.Sprintf(" (+%d more)", hidden)
	}
	return text
}

func retryTierIdentity(tier config.Tier) (string, string) {
	target := string(tier.Target.ID())
	digest := sha256.New()
	write := func(id string) {
		_, _ = digest.Write([]byte(fmt.Sprintf("%d:", len(id))))
		_, _ = digest.Write([]byte(id))
	}
	write(target)
	for _, fallback := range tier.Fallbacks {
		write(string(fallback.ID()))
	}
	return target, hex.EncodeToString(digest.Sum(nil))
}

// pendingRetryStartup reconstructs only a provably unstarted handoff. The
// opening bytes remain authoritative in the source log; the child stores their
// digest and coordinate, so it never creates a second secret-bearing prompt
// copy. A started handoff is intentionally left for explicit human action.
func pendingRetryStartup(m *tuiModel, store *session.Store) tea.Cmd {
	if m == nil || m.app == nil || m.app.loop == nil || store == nil {
		return nil
	}
	childState := m.app.loop.Session.State()
	intent := childState.RetryIntent
	if intent == nil {
		return nil
	}
	explicit := func(reason string) tea.Cmd {
		m.addNotice("warn", reason+" Automatic replay was withheld to avoid duplicating provider or tool work. Use /retry abandon to keep this child without replay; then /resume "+intent.SourceSessionID+" can return to the set-aside source, where /retry is the explicit rerun action.")
		return nil
	}
	if intent.Status != session.RetryIntentPending {
		return explicit("An interrupted /retry crossed its durable execution-start boundary.")
	}
	if len(childState.Messages) != intent.OpeningMessage && len(childState.Messages) != intent.OpeningMessage+1 {
		return explicit("The pending /retry child no longer ends at its recorded source cut.")
	}
	opening, err := store.ReadRetrySourceOpening(intent.SourceSessionID, m.app.workspace, intent.OpeningMessage)
	if err != nil {
		return explicit("The pending /retry source could not be read exactly: " + err.Error() + ".")
	}
	if opening.Role != provider.RoleUser || opening.Injected {
		return explicit("The pending /retry coordinate is not an authored user opening.")
	}
	matches, err := session.RetryIntentOpeningMatches(*intent, opening)
	if err != nil || !matches {
		return explicit("The pending /retry source opening does not match its durable digest.")
	}
	openingRecorded := len(childState.Messages) == intent.OpeningMessage+1
	if openingRecorded {
		childOpening := provider.CloneMessage(childState.Messages[intent.OpeningMessage])
		if childOpening.Role != provider.RoleUser || childOpening.Injected || childOpening.RetryIntentID != intent.ID {
			return explicit("The pending /retry child tail is not an authored user opening.")
		}
		matches, err := session.RetryIntentOpeningMatches(*intent, childOpening)
		if err != nil || !matches {
			return explicit("The pending /retry child tail does not match its durable digest.")
		}
		opening = childOpening
	}
	prompt, authoredKnown := opening.AuthoredProjection()
	if !authoredKnown || prompt == "" {
		return explicit("The pending /retry opening has no exact authored projection.")
	}
	destinationTier := m.app.tier
	if intent.Tier != m.app.tier.ID {
		var ok bool
		destinationTier, ok = m.app.config.Tier(intent.Tier)
		if !ok {
			return explicit("The pending /retry destination tier " + intent.Tier + " is no longer configured.")
		}
	}
	target, tierSet := retryTierIdentity(destinationTier)
	if target != intent.TierTarget || tierSet != intent.TierSetSHA256 {
		return explicit("The pending /retry destination tier or its ordered fallbacks changed after publication.")
	}
	_, generation := m.startPlanning()
	tier := ""
	if intent.Tier != m.app.tier.ID {
		tier = intent.Tier
	}
	m.addNotice("warn", "recovering a published /retry whose durable handoff proves no provider call began; replaying its exact source opening once on "+intent.Tier)
	return func() tea.Msg {
		return retryStartMsg{generation: generation, opening: opening, prompt: prompt, tier: tier, openingRecorded: openingRecorded}
	}
}
