// Package agent runs the request/tool-call loop.
//
// It knows nothing about terminals. Output reaches the user through an
// Observer and permission prompts through an Asker, so the same loop drives the
// REPL, a headless run, and eventually an SDK consumer (design principle 1).
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/continuity"
	"github.com/switchboard-code/switchboard/internal/hooks"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/prefix"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

const (
	// DefaultMaxAttempts bounds retries of a single model call.
	DefaultMaxAttempts = 3

	retryBaseDelay = 500 * time.Millisecond
)

// ErrRoundLimit reports that a turn hit its tool-round cap with the model still
// asking for more calls.
var ErrRoundLimit = errors.New("turn exceeded its tool-round limit")

// ErrProviderCall marks errors that came from the selected provider transport
// or stream. Its wrapper preserves the original error for errors.Is/As while
// letting route telemetry distinguish availability from internal failures.
var ErrProviderCall = errors.New("provider call failed")

// ContextWindowError is a local pre-stream refusal. It is typed so a surface
// can compact or transactionally rebind to a roomier target without parsing
// provider error text; no request has left the process when it is returned.
type ContextWindowError struct {
	Target         provider.RouteTargetID
	Window         int
	InputTokens    int
	ReservedOutput int
}

// AvailabilityError marks a call whose retries are spent against an error the
// target might simply not be able to serve right now: a rate limit, a timeout,
// a server fault. It is separated from an ordinary provider failure because
// another target may not share the condition, and only a typed error lets a
// surface tell "this one is busy" from "this request is wrong" without reading
// error prose.
type AvailabilityError struct {
	Target   provider.RouteTargetID
	Attempts int
	Err      error
}

func (e *AvailabilityError) Error() string {
	return fmt.Sprintf("target %s did not answer in %d attempts: %v",
		provider.DisplayRouteTargetID(e.Target), e.Attempts, e.Err)
}

func (e *AvailabilityError) Unwrap() error { return e.Err }

// ReliefReason says which refusal a surface is being asked to answer. The two
// are not interchangeable: one is a fact about the request that any target
// would state, the other is a fact about one target at one moment, and they
// are recorded differently for exactly that reason.
type ReliefReason string

const (
	// ReliefContext is a request the bound target cannot hold. Nothing has
	// left the process; a roomier rung would take the same bytes.
	ReliefContext ReliefReason = "context"

	// ReliefAvailability is a target that did not answer. The request is fine.
	ReliefAvailability ReliefReason = "availability"
)

// maxReliefsPerTurn bounds how many times one turn may be handed a new
// binding. Past a couple the ladder is not solving this, and a turn that kept
// walking down it would spend the budget discovering that slowly.
const maxReliefsPerTurn = 2

func (e *ContextWindowError) Error() string {
	if e.ReservedOutput == math.MaxInt {
		if target, err := provider.ParseRouteTargetID(e.Target); err == nil && target.Params.MaxOutputTokens > 0 {
			return fmt.Sprintf("target %s has no valid finite output allowance for its %d-token context window: configured max_output %d conflicts with the reasoning settings; raise max_output or lower or disable reasoning",
				provider.DisplayRouteTargetID(e.Target), e.Window, target.Params.MaxOutputTokens)
		}
		return fmt.Sprintf("target %s has no finite output bound for its %d-token context window; set a positive tier max_output with /models or in config",
			provider.DisplayRouteTargetID(e.Target), e.Window)
	}
	return fmt.Sprintf("target %s holds %d tokens, but this call may need up to %d input plus %d reserved output tokens",
		provider.DisplayRouteTargetID(e.Target), e.Window, e.InputTokens, e.ReservedOutput)
}

type Loop struct {
	Provider provider.Provider
	Target   provider.RouteTarget
	Tools    *tools.Registry
	Perms    *permission.Engine
	Asker    permission.Asker
	Session  *session.Session
	Observer Observer

	// Catalog prices what each call cost. Nil means costs are not recorded,
	// which is different from recording them as zero.
	Catalog *catalog.Catalog

	// Cache places cache markers and records what the provider reported. A nil
	// Cache is a cache-unaware loop, which is the control arm §7.1 compares
	// against.
	Cache *Cache

	System []provider.Block

	// MaxToolRounds, when positive, caps one turn's tool rounds as a backstop
	// against a retry cycle burning the budget unseen. Zero means no cap: the
	// escalation watcher and the budget ceiling are the brakes, and the turn
	// runs until the model stops asking for tools.
	MaxToolRounds int
	MaxAttempts   int

	// Relief, when set, is asked for a replacement binding when a round cannot
	// be issued to the one in hand: a request the target cannot hold, or a
	// target that did not answer. It returns the binding to use and a note to
	// render, or an error to give up with.
	//
	// It is a surface's job, not the loop's, for the reason routing is: the
	// checks that make a destination legitimate — probe, capability, context,
	// budget, the user's own pin — live where the ladder does. The loop only
	// knows a round refused and that something upstream may be able to answer.
	Relief func(ctx context.Context, reason ReliefReason, err error) (Binding, string, error)

	// Inject, when set, is drained at the top of every round. What it returns
	// is appended to the session before the request is built, which is how
	// advice, a watch verdict, or the user's own steer reaches a turn already
	// in flight. The round boundary is the only safe seam: tool results are
	// recorded, no call is outstanding, and a user-role message is legal in
	// every wire format this program speaks.
	Inject func() []provider.Message

	// Hooks, when non-nil, runs the user's commands around each tool call: a
	// pre_tool hook can block the call after permission has resolved, and a
	// post_tool hook's output rides back on the result. Hooks are the user's
	// standing policy, so they run without prompting.
	Hooks *hooks.Set

	// ToolExecutionGate, when non-nil, serializes the complete pre-hook, tool,
	// post-hook transaction across loops that share it. Parallel delegates use
	// one when hooks are active: hooks are arbitrary commands, and locking each
	// hook separately would still let another task run between pre and post.
	ToolExecutionGate *ExecutionGate

	// Checkpoints, when non-nil, opens an undo scope per turn; the registry
	// captures into it before each mutation. The interface keeps the
	// recorder's package out of the loop's imports.
	Checkpoints interface {
		BeginTurn(sessionID string, openingMessage int, label string)
	}

	// Budget, when non-nil, is asked before each model call whether the call
	// may go out, given the request's conservative token ceiling. An error ends
	// the turn right there — before the call, which is what §15 means by a
	// preflight bound rather than a spending brake — and everything earlier
	// rounds earned is already recorded. The ceiling itself lives with the
	// surface, which knows what the session has spent and what a dollar is.
	// Budget runs before every provider attempt. attempt starts at one for each
	// model call, allowing a hard ceiling to reserve for failed attempts that a
	// provider may still bill even when no usage record comes back.
	Budget func(contextTokens, attempt int) error

	// BudgetResult reports the outcome of every provider attempt that passed
	// Budget. A failed request may still be billed even when the provider did not
	// return usage, so a hard session ceiling needs a durable pessimistic reserve
	// before a later model call is admitted. On success, usage is the exact
	// priced record already appended to the session; on failure it is empty.
	// It is deliberately separate from Budget so embedders that only need a
	// preflight gate can omit settlement handling.
	BudgetResult func(promptTokens, attempt int, usage session.Usage, err error) error

	// ContextWindow resolves the enforced input+output limit for a concrete
	// target. The surface owns live probes and user declarations, so the core
	// receives the settled number rather than importing configuration or
	// provider registries. Nil preserves the catalog-only fallback.
	ContextWindow func(provider.RouteTarget) int

	// OutputAllowance resolves the exact generation limit the bound adapter
	// will send for a target and catalog maximum. Surfaces set it when bindings
	// are assembled through a provider registry; nil asks the bound adapter
	// directly through provider.OutputTokenAllower.
	OutputAllowance func(provider.RouteTarget, int) int

	// runtimeMu guards the pieces of a loop that a surface may replace between
	// turns or at an explicit model-call boundary. A turn snapshots its observer
	// once, while a model call snapshots provider, target, and cache as one unit.
	runtimeMu sync.RWMutex

	// sessionMu makes a session/tools context swap indivisible with respect to
	// a turn. A bind may be requested concurrently, but it waits until the
	// active turn has finished before clearing read state, restoring todos, and
	// publishing the new session together.
	sessionMu sync.Mutex
}

// Binding is the provider state that must move as one unit. Cache state is
// target-scoped, so independent setters could otherwise send to one target
// while warming or charging another target's cache.
type Binding struct {
	Provider provider.Provider
	Target   provider.RouteTarget
	Cache    *Cache
}

// Binding returns a coherent snapshot of the loop's current provider state.
func (l *Loop) Binding() Binding {
	l.runtimeMu.RLock()
	defer l.runtimeMu.RUnlock()
	return Binding{Provider: l.Provider, Target: l.Target, Cache: l.Cache}
}

// Request constructs the exact provider-visible request shape shared by turn
// routing, feasibility checks, cache planning, and the model call itself.
// Session state stays append-only; ReplayRequest is the projection boundary
// that withholds interrupted assistant output from every downstream consumer.
func (l *Loop) Request(messages []provider.Message) provider.Request {
	return provider.ReplayRequest(provider.Request{
		System:   redactSystemBlocks(l.System),
		Tools:    l.Tools.Definitions(),
		Messages: redactHistoricalToolResults(messages),
	})
}

// Bind replaces provider, target, and cache atomically. It is safe while idle
// or from ToolBatchEnd, after the preceding model call and tools are durable.
func (l *Loop) Bind(binding Binding) {
	l.runtimeMu.Lock()
	l.Provider = binding.Provider
	l.Target = binding.Target
	l.Cache = binding.Cache
	l.runtimeMu.Unlock()
}

// BindSession switches to a context that did not inherit the current
// transcript byte-for-byte: resume, clear, compaction, or an ordinary session
// swap. It drops all file-read evidence before exposing the new session, and a
// session with no live capsule explicitly clears the previous task and working
// context.
func (l *Loop) BindSession(sess *session.Session) error {
	l.sessionMu.Lock()
	defer l.sessionMu.Unlock()
	if sess == nil {
		return fmt.Errorf("bind session: nil session")
	}
	if l.Tools == nil {
		return fmt.Errorf("bind session: nil tool registry")
	}
	if _, err := sess.ReconcileInterruptedToolCalls(); err != nil {
		return fmt.Errorf("bind session: reconcile interrupted tool calls: %w", err)
	}
	items, working, err := todoStateFromContinuity(sess.ResumableContinuity())
	if err != nil {
		return fmt.Errorf("bind session: %w", err)
	}
	// File-read evidence belongs to the exact durable context that observed
	// those bytes, not merely to a registry or workspace. No generic session
	// binding can prove that relationship, so every bind drops it. Branch also
	// starts empty to close the pre-ToolResult interval.
	l.Tools.ForgetAllVersions()
	// Pictures waiting for a round boundary answered a question this session
	// never asked, and delivering them into it would be evidence from a
	// context that is gone.
	l.Tools.ForgetToolImages()
	if err := l.Tools.RestoreContinuity(items, working); err != nil {
		return fmt.Errorf("bind session: restore continuity: %w", err)
	}
	l.Session = sess
	return nil
}

// SetObserver installs the observer graph used by the next turn. A turn keeps
// the snapshot it started with through completion.
func (l *Loop) SetObserver(observer Observer) {
	l.runtimeMu.Lock()
	l.Observer = observer
	l.runtimeMu.Unlock()
}

// price attaches what the catalog says this call cost, along with the revision
// and confidence that produced the number. A cost with neither is not
// reproducible, and one derived from a surface default is shape rather than
// fact (§4, §15).
func (l *Loop) price(target provider.RouteTarget, record session.Usage) session.Usage {
	if l.Catalog == nil {
		return record
	}
	info, confidence, ok := l.Catalog.Lookup(target)
	if !ok {
		return record
	}
	cost, _, priced := info.Cost(record.Usage)
	if !priced {
		return record
	}
	record.CostMicroUSD = int64(cost)
	record.CatalogRevision = l.Catalog.Revision
	record.PriceConfidence = string(confidence)
	return record
}

func (l *Loop) checkContext(binding Binding, inputTokens int) error {
	outputTokens, err := l.resolveOutputAllowance(binding)
	if err != nil {
		return err
	}
	return l.checkContextWithAllowance(binding, inputTokens, outputTokens)
}

func (l *Loop) resolveOutputAllowance(binding Binding) (int, error) {
	target := binding.Target
	var info catalog.ModelInfo
	if l.Catalog != nil {
		info, _, _ = l.Catalog.Lookup(target)
	}
	outputTokens, err := provider.ResolveOutputTokenAllowance(binding.Provider, target, info.MaxOutput)
	if err != nil {
		return 0, err
	}
	if l.OutputAllowance != nil {
		outputTokens = l.OutputAllowance(target, info.MaxOutput)
	}
	return outputTokens, nil
}

func (l *Loop) checkContextWithAllowance(binding Binding, inputTokens, outputTokens int) error {
	target := binding.Target
	var info catalog.ModelInfo
	if l.Catalog != nil {
		info, _, _ = l.Catalog.Lookup(target)
	}
	window := info.ContextWindow
	if l.ContextWindow != nil {
		window = l.ContextWindow(target)
	}
	if window <= 0 {
		return nil
	}
	if inputTokens < 0 || outputTokens < 0 || outputTokens > window ||
		inputTokens > window-outputTokens {
		return &ContextWindowError{
			Target: target.ID(), Window: window,
			InputTokens: inputTokens, ReservedOutput: outputTokens,
		}
	}
	return nil
}

func (l *Loop) observer() Observer {
	l.runtimeMu.RLock()
	observer := l.Observer
	l.runtimeMu.RUnlock()
	if observer == nil {
		return NopObserver{}
	}
	return observer
}

// Turn runs one user message to completion.
//
// It returns an error only when the turn could not be carried out: a protocol
// failure, an exhausted retry budget, or cancellation. A tool that fails, times
// out, or is denied is not an error here, because the model is expected to see
// that result and decide what to do about it.
func (l *Loop) Turn(ctx context.Context, input string) error {
	return l.TurnMessage(ctx, provider.UserText(input))
}

// TurnMessage is Turn for a caller that built the opening message itself —
// a prompt carrying image attachments, say. The message must be user-role
// and complete: it opens the turn, so it is the boundary /fork cuts on and
// the message every later request replays.
func (l *Loop) TurnMessage(ctx context.Context, opening provider.Message) error {
	return l.turnMessage(ctx, opening, "", false)
}

// RetryTurnMessage is TurnMessage with a durable execution handoff. The
// opening is appended normally, but the intent stays pending until the exact
// seam immediately before the first Provider.Stream invocation.
func (l *Loop) RetryTurnMessage(ctx context.Context, opening provider.Message, retryIntentID string) error {
	if retryIntentID == "" {
		return errors.New("retry turn has no durable intent id")
	}
	return l.turnMessage(ctx, opening, retryIntentID, false)
}

// ResumeRetryTurn continues a pending retry whose opening record was durable
// before a crash but whose execution-start record was not. It never stamps or
// appends that opening twice.
func (l *Loop) ResumeRetryTurn(ctx context.Context, retryIntentID string) error {
	if retryIntentID == "" {
		return errors.New("resumed retry turn has no durable intent id")
	}
	return l.turnMessage(ctx, provider.Message{}, retryIntentID, true)
}

func (l *Loop) turnMessage(ctx context.Context, opening provider.Message, retryIntentID string, openingRecorded bool) error {
	l.sessionMu.Lock()
	defer l.sessionMu.Unlock()
	observer := l.observer()
	state := l.Session.State()
	openingMessage := len(state.Messages)
	if openingRecorded {
		intent := state.RetryIntent
		if intent == nil || intent.ID != retryIntentID || intent.Status != session.RetryIntentPending ||
			intent.OpeningMessage < 0 || intent.OpeningMessage != len(state.Messages)-1 ||
			state.Messages[intent.OpeningMessage].RetryIntentID != retryIntentID {
			return errors.New("recorded retry opening does not match a pending durable handoff")
		}
		opening = provider.CloneMessage(state.Messages[intent.OpeningMessage])
		matches, err := session.RetryIntentOpeningMatches(*intent, opening)
		if err != nil || !matches {
			return errors.New("recorded retry opening does not match its durable digest")
		}
		openingMessage = intent.OpeningMessage
	} else {
		// Caller-supplied metadata never grants recovery authority. A retry gets
		// the exact active capability below; every ordinary opening is stripped.
		opening.RetryIntentID = ""
		var err error
		opening, _, err = l.Session.StampContinuityOpening(opening)
		if err != nil {
			return err
		}
		if retryIntentID != "" {
			intent := state.RetryIntent
			if intent == nil || intent.ID != retryIntentID || intent.Status != session.RetryIntentPending ||
				intent.OpeningMessage != openingMessage {
				return errors.New("retry opening does not match its pending durable handoff")
			}
			matches, matchErr := session.RetryIntentOpeningMatches(*intent, opening)
			if matchErr != nil || !matches {
				return errors.New("retry opening does not match its pending durable handoff")
			}
			opening.RetryIntentID = retryIntentID
		}
	}
	// The turn is the undo unit: everything this input causes the tools to
	// change restores together. A subagent's loop leaves this nil and its
	// registry shares the primary recorder, so a delegate's edits file under
	// the turn that delegated.
	if l.Checkpoints != nil {
		l.Checkpoints.BeginTurn(l.Session.ID(), openingMessage, messageLabel(opening))
	}
	if !openingRecorded {
		if err := l.Session.AppendMessage(opening); err != nil {
			return err
		}
	}

	reliefs := 0
	for round := 0; ; round++ {
		// The cap is opt-in. With none set the turn runs until the model
		// stops asking for tools; the watcher and the budget are the brakes.
		if l.MaxToolRounds > 0 && round >= l.MaxToolRounds {
			msg := fmt.Sprintf("turn stopped at the %d tool-round limit", l.MaxToolRounds)
			observer.Notice("warn", msg)
			l.Session.AppendNote("warn", msg)
			return ErrRoundLimit
		}
		// The opening round skips injection: anything pending at turn start
		// is the caller's to fold into the prompt itself, so a request never
		// carries two adjacent user messages. Mid-turn the previous message
		// is a tool result, after which a user-role message is legal in every
		// format this program speaks.
		if l.Inject != nil && round > 0 {
			for _, m := range l.Inject() {
				if err := l.Session.AppendMessage(m); err != nil {
					return err
				}
			}
		}
		msg, stop, usage, attempts, promptTokens, servedTarget, err := l.callModel(ctx, observer, &retryIntentID)
		if err != nil {
			// A round that refused for a reason the ladder can answer is
			// offered to the surface before it becomes the turn's failure.
			// Only a round that produced nothing qualifies: half a streamed
			// message finished by a second target is a turn nobody can
			// attribute, so content on the wire ends the offer.
			if reason, ok := reliefReasonFor(err); ok && len(msg.Content) == 0 && reliefs < maxReliefsPerTurn {
				if note, reliefErr := l.relieve(ctx, reason, err); reliefErr == nil {
					reliefs++
					if note != "" {
						observer.Notice("warn", note)
					}
					continue
				}
			}
			// Content that did arrive is recorded as an interrupted turn, so the
			// session shows what happened instead of a gap. The provider-level
			// replay projection withholds incomplete assistant messages before
			// estimation, cache planning, and adapter translation, which is what
			// makes re-issuing safe.
			if len(msg.Content) > 0 {
				msg.Incomplete = true
				if appendErr := l.Session.AppendMessage(msg); appendErr != nil {
					return appendErr
				}
			}
			return err
		}

		if err := l.Session.AppendMessage(msg); err != nil {
			return err
		}
		record := l.price(servedTarget, session.Usage{
			Target:   string(servedTarget.ID()),
			Usage:    usage,
			Attempts: attempts,
			Purpose:  session.UsagePurposeTurn,
		})
		storedRecord, err := l.Session.AppendUsageRecord(record)
		if err != nil {
			return err
		}
		record = storedRecord
		// A successful attempt remains atomically reserved until its observed
		// usage is durable. Releasing it before AppendUsage would let a
		// concurrent race/delegate call spend the same headroom in that gap.
		if l.BudgetResult != nil {
			if err := l.BudgetResult(promptTokens, attempts, record, nil); err != nil {
				return err
			}
		}
		observer.TurnUsage(record)

		uses := msg.ToolUses()
		if stop != provider.StopToolUse || len(uses) == 0 {
			return nil
		}

		results, runErr := l.runTools(ctx, uses, observer)
		// Results are appended even when the turn is being abandoned. An
		// assistant message whose tool calls have no matching results is a
		// malformed conversation, and every later request built from this
		// session would carry that damage forward.
		if len(results) > 0 {
			resultMessage := provider.Message{
				Role:    provider.RoleTool,
				Content: results,
			}
			if successfulTodoResult(results) {
				if _, err := l.Session.AppendToolResultsWithWorking(
					resultMessage, continuityTasks(l.Tools.Todos()), l.Tools.Working()); err != nil {
					return err
				}
			} else if err := l.Session.AppendMessage(resultMessage); err != nil {
				return err
			}
		}
		if runErr != nil {
			return runErr
		}
		// All parallel calls completed successfully and their ordered results are
		// durable. An aborted or cancelled batch is not a routing boundary: its
		// partial evidence must not rebind a turn that is ending.
		observer.ToolBatchEnd(ctx)
	}
}

func successfulTodoResult(results []provider.Block) bool {
	for _, block := range results {
		result, ok := block.(provider.ToolResult)
		if ok && result.Name == "todo" && !result.IsError {
			return true
		}
	}
	return false
}

func continuityTasks(items []tools.TodoItem) []continuity.Task {
	tasks := make([]continuity.Task, len(items))
	for i, item := range items {
		tasks[i] = continuity.Task{Text: item.Text, Status: continuity.TaskStatus(item.Status)}
	}
	return tasks
}

func todoStateFromContinuity(capsule *continuity.Capsule) ([]tools.TodoItem, continuity.Working, error) {
	if capsule == nil || capsule.Cleared {
		return nil, continuity.Working{}, nil
	}
	if err := continuity.ValidateStored(*capsule); err != nil {
		return nil, continuity.Working{}, fmt.Errorf("invalid continuity capsule: %w", err)
	}
	items := make([]tools.TodoItem, len(capsule.Tasks))
	for i, task := range capsule.Tasks {
		items[i] = tools.TodoItem{Text: task.Text, Status: tools.TodoStatus(task.Status)}
	}
	working := continuity.Working{
		Objective:     capsule.Objective,
		NextAction:    capsule.NextAction,
		StopCondition: capsule.StopCondition,
	}
	return items, working, nil
}

// callModel issues one model call, retrying transient failures. It returns
// whatever content arrived even when it ends in an error, so the caller can
// record a partial turn.
func (l *Loop) callModel(ctx context.Context, observer Observer, retryIntentID *string) (provider.Message, provider.StopReason, provider.Usage, int, int, provider.RouteTarget, error) {
	maxAttempts := orDefault(l.MaxAttempts, DefaultMaxAttempts)
	binding := l.Binding()

	req := l.Request(l.Session.State().Messages)
	promptTokens := prefix.RequestTokens(req)
	contextTokens := prefix.RequestTokenCeiling(req)
	outputTokenAllowance, err := l.resolveOutputAllowance(binding)
	if err != nil {
		return provider.Message{}, "", provider.Usage{}, 0, promptTokens, binding.Target, err
	}
	if err := l.checkContextWithAllowance(binding, contextTokens, outputTokenAllowance); err != nil {
		return provider.Message{}, "", provider.Usage{}, 0, promptTokens, binding.Target, err
	}
	req.CachePlan = binding.Cache.plan(req.System, req.Tools, req.Messages)

	var lastMsg provider.Message
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Do not reserve a hard-budget attempt that cannot possibly be issued.
		// A cancellation after this point is handled by streamOnce's issued bit:
		// an invoked provider is conservatively chargeable; an uninvoked one is
		// released without becoming permanent retry debt.
		if err := ctx.Err(); err != nil {
			return lastMsg, "", provider.Usage{}, attempt - 1, promptTokens, binding.Target, err
		}
		if l.Budget != nil {
			if err := l.Budget(contextTokens, attempt); err != nil {
				return lastMsg, "", provider.Usage{}, attempt - 1, promptTokens, binding.Target, err
			}
		}
		var beforeStream func() error
		if retryIntentID != nil && *retryIntentID != "" {
			beforeStream = func() error {
				if err := l.Session.StartRetryIntent(*retryIntentID); err != nil {
					return fmt.Errorf("saving retry execution-start boundary: %w", err)
				}
				*retryIntentID = ""
				return nil
			}
		}
		msg, stop, usage, issued, err := l.streamOnce(ctx, binding, observer, req, outputTokenAllowance, beforeStream)
		if err == nil {
			err = ctx.Err()
		}
		if err != nil && l.BudgetResult != nil {
			settlementErr := err
			if !issued {
				settlementErr = nil
			}
			if budgetErr := l.BudgetResult(promptTokens, attempt, session.Usage{}, settlementErr); budgetErr != nil {
				return msg, stop, usage, attempt, promptTokens, binding.Target, budgetErr
			}
		}
		if err == nil {
			// Recorded from what came back, never from what was sent (§6.3).
			// A retried attempt is not recorded: its usage belongs to a request
			// that failed, and folding it in would report a cache miss for a
			// turn the provider never finished.
			binding.Cache.observe(usage, time.Now())
			return msg, stop, usage, attempt, promptTokens, binding.Target, nil
		}
		lastMsg, lastErr = msg, err

		if ctx.Err() != nil {
			return msg, stop, usage, attempt, promptTokens, binding.Target, ctx.Err()
		}
		if !retryable(err) {
			return msg, stop, usage, attempt, promptTokens, binding.Target, providerCallError(err)
		}
		if attempt == maxAttempts {
			// Retryable to the last attempt is the shape another target might
			// not share, so it is typed rather than flattened into the generic
			// provider failure. It still wraps ErrProviderCall, so every
			// existing reader of that keeps working.
			return msg, stop, usage, attempt, promptTokens, binding.Target,
				providerCallError(&AvailabilityError{Target: binding.Target.ID(), Attempts: attempt, Err: err})
		}

		// A retry starts a distinct provider attempt. Close this attempt's
		// durable draft as incomplete before issuing another one; otherwise the
		// next attempt would either rewrite the first attempt's evidence or leave
		// two active drafts at the conversation tail. Replay filters the closed
		// message, and req remains the immutable pre-attempt request.
		if len(msg.Content) > 0 {
			msg.Incomplete = true
			if appendErr := l.Session.AppendMessage(msg); appendErr != nil {
				return msg, stop, usage, attempt, promptTokens, binding.Target, appendErr
			}
		}

		// A dropped stream is re-issued from the last committed message rather
		// than resumed. Ollama exposes no continuation handle, and treating a
		// partial response as committed would mean guessing what the server
		// had already produced (§10.3).
		observer.Notice("warn", fmt.Sprintf("attempt %d of %d failed (%v), retrying", attempt, maxAttempts, err))
		if err := sleep(ctx, backoff(attempt)); err != nil {
			// Any content from this attempt is already durably closed above. Do
			// not ask TurnMessage to append the same DraftID a second time.
			return provider.Message{}, stop, usage, attempt, promptTokens, binding.Target, err
		}
	}
	return lastMsg, "", provider.Usage{}, maxAttempts, promptTokens, binding.Target, providerCallError(lastErr)
}

// reliefReasonFor recognizes the two refusals a different target might not
// make. Everything else is the request being wrong, which no rung fixes.
func reliefReasonFor(err error) (ReliefReason, bool) {
	var window *ContextWindowError
	if errors.As(err, &window) {
		return ReliefContext, true
	}
	var availability *AvailabilityError
	if errors.As(err, &availability) {
		return ReliefAvailability, true
	}
	return "", false
}

// relieve asks the surface for a replacement binding and applies it.
//
// The binding is applied here rather than by the caller so the loop's next
// round is issued against it without a window in which the two disagree about
// which target is bound.
func (l *Loop) relieve(ctx context.Context, reason ReliefReason, cause error) (string, error) {
	if l.Relief == nil {
		return "", errors.New("no relief is configured for this surface")
	}
	binding, note, err := l.Relief(ctx, reason, cause)
	if err != nil {
		return "", err
	}
	if binding.Provider == nil {
		return "", errors.New("relief returned no provider")
	}
	l.Bind(binding)
	return note, nil
}

func providerCallError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrProviderCall, err)
}

func (l *Loop) streamOnce(ctx context.Context, binding Binding, observer Observer, req provider.Request, outputTokenAllowance int, beforeStream func() error) (provider.Message, provider.StopReason, provider.Usage, bool, error) {
	var b messageBuilder
	var stop provider.StopReason
	var usage provider.Usage
	draft := newStreamDraft(l.Session, observer, &b, outputTokenAllowance)

	if err := ctx.Err(); err != nil {
		return draft.message(), stop, usage, false, err
	}
	if beforeStream != nil {
		if err := beforeStream(); err != nil {
			return draft.message(), stop, usage, false, err
		}
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	stream, err := binding.Provider.Stream(streamCtx, binding.Target, req)
	if err != nil {
		cancelStream()
		return draft.message(), stop, usage, provider.RequestIssued(err), err
	}
	defer stream.Close()
	defer cancelStream()

	for {
		ev, err := stream.Next()
		// Cancellation is the owner's last word even when an adapter cannot
		// interrupt its read promptly and later hands back a successful terminal
		// event. Check before accepting either that event or EOF so a late result
		// cannot become a completed, billed turn after the user stopped it.
		if ctxErr := ctx.Err(); ctxErr != nil {
			if err := draft.flush(); err != nil {
				return draft.message(), stop, usage, true, err
			}
			return draft.message(), stop, usage, true, ctxErr
		}
		if errors.Is(err, io.EOF) {
			if err := draft.flush(); err != nil {
				return draft.message(), stop, usage, true, err
			}
			return draft.message(), stop, usage, true, nil
		}
		if err != nil {
			if flushErr := draft.flush(); flushErr != nil {
				return draft.message(), stop, usage, true, flushErr
			}
			return draft.message(), stop, usage, true, err
		}

		switch ev.Type {
		case provider.EventThinkingDelta:
			if err := draft.add(ev); err != nil {
				cancelStream()
				return draft.message(), stop, usage, true, err
			}
		case provider.EventTextDelta:
			if err := draft.add(ev); err != nil {
				cancelStream()
				return draft.message(), stop, usage, true, err
			}
		case provider.EventToolUse:
			if ev.ToolUse == nil {
				return draft.message(), stop, usage, true, &provider.ProtocolError{Provider: binding.Provider.Name(), Detail: "tool-use stream event has no call"}
			}
			if err := draft.add(ev); err != nil {
				cancelStream()
				return draft.message(), stop, usage, true, err
			}
		case provider.EventDone:
			if err := draft.admitDone(ev.Usage); err != nil {
				cancelStream()
				return draft.message(), stop, usage, true, err
			}
			if err := draft.flush(); err != nil {
				return draft.message(), stop, usage, true, err
			}
			stop = ev.StopReason
			usage = ev.Usage
		default:
			cancelStream()
			return draft.message(), stop, usage, true, &provider.ProtocolError{
				Provider: binding.Provider.Name(), Detail: fmt.Sprintf("unsupported stream event %q", ev.Type),
			}
		}
	}
}

type toolJob struct {
	use      provider.ToolUse
	plan     tools.Plan
	result   *tools.Result
	ready    bool
	resolved bool
	outcome  permission.Outcome
	audit    session.Permission
}

// runTools resolves permission for every call and then executes. Results come
// back in call order regardless of how they were scheduled.
func (l *Loop) runTools(ctx context.Context, uses []provider.ToolUse, observer Observer) ([]provider.Block, error) {
	jobs := make([]*toolJob, len(uses))
	for i, use := range uses {
		j := &toolJob{use: use}
		jobs[i] = j

		tool, ok := l.Tools.Get(use.Name)
		if !ok {
			j.fail("no tool named %q is available", use.Name)
			continue
		}

		plan, err := tool.Plan(use.Input)
		if err != nil {
			// Bad arguments go back to the model as a tool error so it can
			// correct them. Only protocol-level damage aborts a turn (§10.3).
			j.fail("%s", err.Error())
			continue
		}
		j.plan = plan

		approved, outcome, err := l.Perms.Resolve(ctx, l.Asker, plan.Request)
		if err != nil {
			return resultBlocks(jobs, "the permission prompt failed"), err
		}
		audit := session.Permission{
			Tool:       use.Name,
			Mode:       string(l.Perms.Mode()),
			Decision:   string(outcome.Decision),
			Reason:     outcome.Reason,
			Approved:   approved,
			ResolvedBy: string(outcome.ResolvedBy),
		}
		if outcome.Review != nil {
			audit.Reviewer = outcome.Review.Reviewer
			audit.ReviewDecision = string(outcome.Review.Decision)
			audit.ReviewReason = outcome.Review.Reason
			audit.ReviewError = outcome.Review.Error
		}
		j.resolved = true
		j.outcome = outcome
		j.audit = audit
		if !approved {
			continue
		}
		j.ready = true
	}

	var approvedOutcomes []permission.Outcome
	for _, j := range jobs {
		if j != nil && j.ready {
			approvedOutcomes = append(approvedOutcomes, j.outcome)
		}
	}
	var releasePermissions func()
	if len(approvedOutcomes) > 0 {
		var holdErr error
		releasePermissions, holdErr = l.Perms.HoldResolutions(approvedOutcomes)
		if holdErr != nil {
			for _, j := range jobs {
				if j == nil || !j.ready {
					continue
				}
				j.ready = false
				j.outcome.Decision = permission.Deny
				j.outcome.ResolvedBy = permission.ResolvedByPolicy
				j.outcome.Reason = holdErr.Error()
				j.audit.Decision = string(permission.Deny)
				j.audit.Approved = false
				j.audit.ResolvedBy = string(permission.ResolvedByPolicy)
				j.audit.Reason = holdErr.Error()
			}
		}
	}
	if releasePermissions != nil {
		defer releasePermissions()
	}

	// Permission is one durable batch boundary: every final resolution is
	// recorded before the first side effect, while the mode lease is held.
	for _, j := range jobs {
		if j == nil || !j.resolved {
			continue
		}
		if err := l.Session.AppendPermission(j.audit); err != nil {
			return resultBlocks(jobs, "the permission decision could not be recorded"),
				fmt.Errorf("recording permission for %s: %w", j.use.Name, err)
		}
		if !j.ready {
			j.fail("the request was not approved: %s", j.audit.Reason)
		}
	}

	var pending []*toolJob
	// Hooks are trusted commands with arbitrary side effects. Even read tools
	// serialize when hooks are present so two pre/post hooks cannot race each
	// other or the tool whose result they annotate.
	parallel := l.Hooks.Empty()
	parallelReads := false
	parallelKey := ""
	for _, j := range jobs {
		if !j.ready {
			continue
		}
		pending = append(pending, j)
		tool, ok := l.Tools.Get(j.use.Name)
		if !ok {
			parallel = false
			continue
		}
		if tool.ParallelSafe() {
			parallelReads = true
			if parallelKey != "" {
				parallel = false
			}
			continue
		}
		grouped, ok := tool.(tools.ParallelBatchTool)
		if !ok || grouped.ParallelBatchKey() == "" || parallelReads {
			parallel = false
			continue
		}
		key := grouped.ParallelBatchKey()
		if parallelKey == "" {
			parallelKey = key
		} else if parallelKey != key {
			parallel = false
		}
	}

	// A mixed batch runs serially rather than partitioned, because reordering
	// a write around a read changes what the read returns.
	if parallel && len(pending) > 1 {
		var wg sync.WaitGroup
		for _, j := range pending {
			wg.Add(1)
			go func(j *toolJob) {
				defer wg.Done()
				l.execute(ctx, j, observer)
			}(j)
		}
		wg.Wait()
	} else {
		for _, j := range pending {
			if ctx.Err() != nil {
				break
			}
			l.execute(ctx, j, observer)
		}
	}

	if ctx.Err() != nil {
		return resultBlocks(jobs, "cancelled before this call ran"), ctx.Err()
	}
	return resultBlocks(jobs, "did not run"), nil
}

func (l *Loop) execute(ctx context.Context, j *toolJob, observer Observer) {
	observer.ToolStart(j.use, j.plan.Request)
	started := time.Now()

	var res tools.Result
	if l.ToolExecutionGate != nil {
		release, err := l.ToolExecutionGate.Acquire(ctx)
		if err != nil {
			res = tools.Result{Content: err.Error(), IsError: true}
			j.result = &res
			observer.ToolEnd(j.use, j.plan.Request, res, time.Since(started))
			return
		}
		defer release()
	}
	if msg, blocked := l.Hooks.PreTool(ctx, j.plan.Request); blocked {
		// Blocked after approval, before effect: the hook's answer goes back
		// as a tool error so the model reads why instead of guessing.
		res = tools.Result{Content: msg, IsError: true}
	} else {
		var err error
		res, err = j.plan.Run(ctx)
		if err != nil {
			res = tools.Result{Content: err.Error(), IsError: true}
		}
		if note := l.Hooks.PostTool(ctx, j.plan.Request, res.Content, res.IsError); note != "" {
			res.Content = strings.TrimRight(res.Content, "\n") + "\n" + note
		}
	}
	j.result = &res
	observer.ToolEnd(j.use, j.plan.Request, res, time.Since(started))
}

func (j *toolJob) fail(format string, args ...any) {
	j.result = &tools.Result{Content: fmt.Sprintf(format, args...), IsError: true}
}

// resultBlocks assembles results in call order, filling any gap so that every
// tool call has exactly one result.
func resultBlocks(jobs []*toolJob, unfilled string) []provider.Block {
	out := make([]provider.Block, 0, len(jobs))
	for _, j := range jobs {
		res := tools.Result{Content: unfilled, IsError: true}
		if j.result != nil {
			res = *j.result
		}
		out = append(out, provider.ToolResult{
			ToolUseID: j.use.ID,
			Name:      j.use.Name,
			// Tool execution, hooks, and observers intentionally see the local
			// result verbatim. This is the provider/session boundary: redact the
			// complete component here, before request sizing or compaction can
			// cut a recognized credential below its detection floor.
			Content: redactCredentialText(res.Content),
			IsError: res.IsError,
		})
	}
	return out
}

// messageBuilder reassembles streamed events into canonical blocks, keyed by
// the block index the adapter assigned and emitted in arrival order.
type messageBuilder struct {
	byIndex map[int]*blockAccum
	order   []int
	draftID string
}

type blockAccum struct {
	kind      provider.BlockKind
	text      strings.Builder
	use       provider.ToolUse
	signature string
}

func (b *messageBuilder) accum(index int, kind provider.BlockKind) *blockAccum {
	if b.byIndex == nil {
		b.byIndex = map[int]*blockAccum{}
	}
	a, ok := b.byIndex[index]
	if !ok {
		a = &blockAccum{kind: kind}
		b.byIndex[index] = a
		b.order = append(b.order, index)
	}
	return a
}

func (b *messageBuilder) delta(index int, kind provider.BlockKind, text string) {
	b.accum(index, kind).text.WriteString(text)
}

// sign records the attestation a target issued over a thinking block. It
// arrives on its own event rather than with the text, because the signature
// covers the finished block and is only known once the block closes.
func (b *messageBuilder) sign(index int, signature string) {
	if signature == "" {
		return
	}
	b.accum(index, provider.KindThinking).signature = signature
}

func (b *messageBuilder) toolUse(index int, use provider.ToolUse) {
	b.accum(index, provider.KindToolUse).use = use
}

func (b *messageBuilder) message() provider.Message {
	msg := provider.Message{Role: provider.RoleAssistant, DraftID: b.draftID}
	for _, i := range b.order {
		a := b.byIndex[i]
		switch a.kind {
		case provider.KindThinking:
			msg.Content = append(msg.Content, provider.Thinking{Text: a.text.String(), Signature: a.signature})
		case provider.KindText:
			msg.Content = append(msg.Content, provider.Text{Text: a.text.String()})
		case provider.KindToolUse:
			msg.Content = append(msg.Content, a.use)
		}
	}
	return msg
}

func retryable(err error) bool {
	var apiErr *provider.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable()
	}
	// A dropped stream is worth another try. Malformed content is not, because
	// re-issuing produces the same shape and burns the attempt budget.
	return errors.Is(err, provider.ErrStreamIncomplete)
}

// backoff grows exponentially with jitter, so several clients failing against
// one server do not resynchronize their retries.
func backoff(attempt int) time.Duration {
	base := retryBaseDelay << (attempt - 1)
	return base + time.Duration(rand.Int64N(int64(base/2)))
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// messageLabel is what the checkpoint recorder files the turn under: the
// text the user typed, whatever else the message carries.
func messageLabel(msg provider.Message) string {
	return msg.AuthoredText()
}

func orDefault(v, fallback int) int {
	if v > 0 {
		return v
	}
	return fallback
}
