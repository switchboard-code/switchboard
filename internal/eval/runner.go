package eval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/breakpoint"
	"github.com/switchboard-code/switchboard/internal/cachestate"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// Arm is one thing being measured: a fixed target used as a baseline, or the
// router. §7.1 compares against the best fixed-target baseline, so the two have
// to stay distinguishable through the whole pipeline.
type Arm struct {
	Name     string
	Target   provider.RouteTarget
	Provider provider.Provider

	// ContextWindow is the user-declared limit for this target's serving
	// surface. It has the same precedence as production configuration: an
	// enforced live limit wins, this declaration beats an unenforced metadata
	// hint, and the catalog is the final fallback.
	ContextWindow int

	// Fallbacks are availability substitutes for the routed arm only, in user
	// policy order. Fixed baselines deliberately ignore them: a baseline must
	// remain the one pinned target it claims to measure.
	Fallbacks []Fallback

	// CacheAware places cache markers. Off is the control arm §7.1 compares
	// against when it asks whether the interval against an otherwise identical
	// cache-unaware router excludes zero: same model, same corpus, same tools,
	// and the one difference is whether §6 runs at all.
	CacheAware bool

	// resolvedContextWindow is immutable evidence captured by the probe that
	// admitted this concrete target. It travels with a routed primary/fallback
	// so the loop enforces the same number the opening or move scored.
	resolvedContextWindow int
}

// Fallback is one concrete target/provider binding inside a routed tier.
type Fallback struct {
	Target     provider.RouteTarget
	Provider   provider.Provider
	CacheAware bool

	// ContextWindow has the same meaning and precedence as Arm.ContextWindow.
	ContextWindow int
}

// Runner executes tasks.
//
// Each attempt gets its own copy of the repository, its own session, and its own
// sandbox capability check. Sharing any of those would let one attempt see
// another's edits, which turns a corpus into a single long conversation.
type Runner struct {
	Catalog *catalog.Catalog

	// MaxRounds bounds a single attempt. A task that has not converged in this
	// many tool rounds counts as unsolved rather than running forever, and the
	// bound is recorded so a corpus of timeouts is visible as one.
	MaxRounds int

	// Timeout bounds wall time per attempt for the same reason.
	Timeout time.Duration
}

const (
	DefaultMaxRounds = 40
	DefaultTimeout   = 10 * time.Minute
)

func (r Runner) rounds() int {
	if r.MaxRounds > 0 {
		return r.MaxRounds
	}
	return DefaultMaxRounds
}

func (r Runner) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return DefaultTimeout
}

// Run attempts one task on one arm with one seed.
//
// It reports a Run whatever happens. A crashed attempt is an unsolved attempt
// with a reason, because dropping it would quietly improve the arm's solve rate.
func (r Runner) Run(ctx context.Context, task Task, arm Arm, seed int) Run {
	return r.run(ctx, task, arm, seed, nil)
}

// escalation is the hook a routed arm supplies so the primary can move
// mid-task. A fixed baseline passes nil and never moves, which is what makes it
// a baseline.
type escalation interface {
	attach(*agent.Loop)
	fidelityError() error
}

// preparedAttempt is the workspace-bound request assembly shared by opening
// routing and execution. Keeping one registry and one system prompt here is
// what proves the router scored the request the selected provider receives;
// rebuilding either side separately would let project instructions, tool
// schemas, or even the temporary workspace path drift.
type preparedAttempt struct {
	dir        string
	capability execution.Capability
	mode       permission.Mode
	registry   *tools.Registry
	system     []provider.Block
	opening    provider.Message
}

func prepareAttempt(task Task, dir string) (*preparedAttempt, error) {
	capability := execution.Detect()
	registry, err := tools.NewRegistry(dir, capability)
	if err != nil {
		return nil, err
	}
	mode := permission.ModeBypass
	return &preparedAttempt{
		dir:        dir,
		capability: capability,
		mode:       mode,
		registry:   registry,
		system:     agent.SystemPrompt(dir, mode, capability),
		opening:    provider.UserText(task.Prompt),
	}, nil
}

func (p *preparedAttempt) openingRequest() provider.Request {
	return provider.Request{
		System:   p.system,
		Tools:    p.registry.Definitions(),
		Messages: []provider.Message{p.opening},
	}
}

type armSelection struct {
	arm             Arm
	escalation      escalation
	estimatedCost   catalog.Money
	estimatedTarget provider.RouteTargetID
}

type selectArm func(context.Context, Task, *preparedAttempt) (armSelection, error)

func (r Runner) run(ctx context.Context, task Task, arm Arm, seed int, esc escalation) Run {
	return r.runSelected(ctx, task, arm.Name, arm.Target.ID(), seed,
		func(ctx context.Context, _ Task, _ *preparedAttempt) (armSelection, error) {
			if arm.Provider == nil {
				return armSelection{arm: arm, escalation: esc}, nil
			}
			resolved, _, err := resolveArmEvidence(ctx, r.Catalog, arm)
			if err != nil {
				return armSelection{}, fmt.Errorf("probing fixed target %s: %w", arm.Target.Display(), err)
			}
			return armSelection{arm: resolved, escalation: esc}, nil
		})
}

func (r Runner) runSelected(
	ctx context.Context,
	task Task,
	reportArm string,
	initialTarget provider.RouteTargetID,
	seed int,
	selectTarget selectArm,
) Run {
	out := Run{
		TaskID:     task.ID,
		Provenance: task.Provenance,
		Target:     initialTarget,
		Arm:        reportArm,
		Seed:       seed,
	}

	dir, err := os.MkdirTemp("", "sb-eval-")
	if err != nil {
		out.Detail = "could not create a workspace: " + err.Error()
		out.Failure = FailureWorkspace
		return out
	}
	defer os.RemoveAll(dir)

	if err := task.Setup(dir); err != nil {
		out.Detail = "setup failed: " + err.Error()
		out.Failure = FailureSetup
		return out
	}

	started := time.Now()
	attemptCtx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()

	prepared, err := prepareAttempt(task, dir)
	if err != nil {
		out.Duration = time.Since(started)
		out.Detail = "the turn failed: assembling the evaluation request: " + err.Error()
		out.Failure = FailureTurn
		return out
	}
	selection, err := selectTarget(attemptCtx, task, prepared)
	if err != nil {
		out.Duration = time.Since(started)
		if reportArm == RoutedArm {
			out.Detail = "routed evaluation fidelity failed: " + err.Error()
			out.Failure = FailureFidelity
		} else {
			out.Detail = "the turn failed: " + err.Error()
			out.Failure = FailureTurn
		}
		return out
	}
	if selection.arm.Provider == nil {
		out.Duration = time.Since(started)
		if reportArm == RoutedArm {
			out.Detail = "routed evaluation fidelity failed: selected target has no provider"
			out.Failure = FailureFidelity
		} else {
			out.Detail = "the turn failed: fixed target has no provider"
			out.Failure = FailureTurn
		}
		return out
	}
	out.Target = selection.arm.Target.ID()
	out.EstimatedCost = selection.estimatedCost
	out.EstimatedTarget = selection.estimatedTarget

	stats, runErr := r.attempt(attemptCtx, selection.arm, prepared, selection.escalation)
	perTarget := stats.perTarget
	out.Duration = time.Since(started)
	out.Denials = stats.denials
	out.Rounds = stats.rounds
	out.ToolErrors = stats.toolErrors
	if routed, ok := selection.escalation.(*escalator); ok {
		out.Target = routed.finalTarget(selection.arm)
		out.Escalations = routed.moves
	}

	// Cost follows the tokens, not the arm. A routed run that escalates spends
	// on the target it moved to, and pricing the whole run against the rung it
	// started on reports an escalation to a paid target as free, which would let
	// any cost gate pass trivially and wrongly.
	for target, usage := range perTarget {
		out.Usage = out.Usage.Add(usage)
		if r.Catalog == nil {
			continue
		}
		info, _, ok := r.Catalog.Lookup(targetOf(r.Catalog, target))
		if !ok {
			continue
		}
		if cost, _, priced := info.Cost(usage); priced {
			out.Cost += cost
		}
	}
	if selection.escalation != nil {
		if fidelityErr := selection.escalation.fidelityError(); fidelityErr != nil {
			out.Detail = "routed evaluation fidelity failed: " + fidelityErr.Error()
			out.Failure = FailureFidelity
			out.Solved = false
			return out
		}
	}

	if runErr != nil {
		// A failed turn is still a data point. §8.4 treats provider failure as
		// something other than a bad routing decision, and the distinction is
		// only possible if the failure is recorded rather than discarded.
		out.Detail = "the turn failed: " + runErr.Error()
		switch {
		case errors.Is(runErr, context.DeadlineExceeded):
			out.Failure = FailureTimeout
		case errors.Is(runErr, context.Canceled):
			out.Failure = FailureCancelled
		case errors.Is(runErr, agent.ErrRoundLimit):
			out.Failure = FailureRoundLimit
		case errors.Is(runErr, errRoutedFidelity):
			out.Failure = FailureFidelity
		default:
			out.Failure = FailureTurn
		}
		return out
	}

	solved, detail, verifyErr := task.Verify(attemptCtx, dir)
	out.Duration = time.Since(started)
	if verifyErr != nil {
		switch {
		case errors.Is(verifyErr, context.DeadlineExceeded):
			out.Detail = "the verifier timed out: " + verifyErr.Error()
			out.Failure = FailureTimeout
		case errors.Is(verifyErr, context.Canceled):
			out.Detail = "the verifier was cancelled: " + verifyErr.Error()
			out.Failure = FailureCancelled
		default:
			out.Detail = "the verifier failed to run: " + verifyErr.Error()
			out.Failure = FailureVerifier
		}
		return out
	}
	out.Solved = solved
	out.Detail = detail
	if !solved {
		out.Failure = FailureVerification
	}
	return out
}

// targetOf reconstructs a route target from its id, so usage recorded against
// a target the run moved to can still be priced.
func targetOf(cat *catalog.Catalog, id provider.RouteTargetID) provider.RouteTarget {
	target, err := provider.ParseRouteTargetID(id)
	if err != nil {
		return provider.RouteTarget{}
	}
	return target
}

// attemptStats is what an attempt reveals beyond whether it solved the task.
// Solve rate on a saturated corpus cannot distinguish a run that went straight
// there from one that wandered for thirty rounds, and the changes worth making
// to a prompt or a tool schema mostly move the second number.
type attemptStats struct {
	perTarget  map[provider.RouteTargetID]provider.Usage
	denials    int
	rounds     int
	toolErrors map[string]int
}

func (r Runner) attempt(ctx context.Context, arm Arm, prepared *preparedAttempt, esc escalation) (attemptStats, error) {
	if prepared == nil || prepared.registry == nil {
		return attemptStats{}, fmt.Errorf("evaluation request was not assembled")
	}
	controlDir, err := os.MkdirTemp("", "sb-eval-sessions-")
	if err != nil {
		return attemptStats{}, err
	}
	defer os.RemoveAll(controlDir)
	store, err := session.NewStore(controlDir)
	if err != nil {
		return attemptStats{}, err
	}
	checkpointDir, err := store.WorkspaceDir(prepared.dir)
	if err != nil {
		return attemptStats{}, err
	}
	recorder := checkpoint.NewRecorder()
	if err := recorder.ConfigureRestoreCleanup(checkpointDir, prepared.dir); err != nil {
		return attemptStats{}, err
	}
	prepared.registry.SetCheckpoints(recorder)
	revision := ""
	if r.Catalog != nil {
		revision = r.Catalog.Revision
	}
	sess, err := store.Create(prepared.dir, arm.Target.ID(), revision)
	if err != nil {
		return attemptStats{}, err
	}
	defer sess.Close()

	// Bypass, because every task has to run the test suite and acceptEdits
	// deliberately does not cover running commands. This is the one place that
	// is the right mode: the sandbox still governs what a command can reach, the
	// workspace is a throwaway copy of the repository, and a harness that stops
	// to ask is not a harness.
	//
	// It is also a real dependency on §11. Where confinement is unverified the
	// engine downgrades bypass back to asking, and this will fail loudly rather
	// than run unprotected, which is the behaviour design principle 4 wants.
	asker := &denyingAsker{}
	collector := &usageCollector{byTarget: map[provider.RouteTargetID]provider.Usage{}}

	loop := &agent.Loop{
		Provider:      arm.Provider,
		Target:        arm.Target,
		Tools:         prepared.registry,
		Perms:         permission.NewEngine(prepared.mode, prepared.capability),
		Asker:         asker,
		Session:       sess,
		Observer:      collector,
		Catalog:       r.Catalog,
		System:        prepared.system,
		MaxToolRounds: r.rounds(),
		Checkpoints:   recorder,
	}
	installEvalResolvers(loop, arm, r.Catalog)

	collector.loop = loop
	if arm.CacheAware {
		if info, _, ok := r.Catalog.Lookup(arm.Target); ok {
			loop.Cache = &agent.Cache{
				Manager: &breakpoint.Manager{Policy: info.Cache, Target: arm.Target.ID()},
				Tracker: cachestate.New(),
				Policy:  info.Cache,
				Target:  arm.Target.ID(),
			}
		}
	}
	if esc != nil {
		esc.attach(loop)
	}

	err = loop.TurnMessage(ctx, prepared.opening)
	return attemptStats{
		perTarget:  collector.byTarget,
		denials:    asker.denied,
		rounds:     collector.rounds,
		toolErrors: collector.toolErrors,
	}, err
}

// installEvalResolvers makes the execution guard consume the same concrete
// evidence as selection. Output allowance follows the live binding so a routed
// move cannot retain the previous adapter's wire policy; context starts with
// the probe-resolved opening target and the escalator extends it transactionally
// for each prepared destination.
func installEvalResolvers(loop *agent.Loop, arm Arm, cat *catalog.Catalog) {
	if loop == nil {
		return
	}
	openingWindow := arm.resolvedContextWindow
	if openingWindow <= 0 && cat != nil {
		if info, _, ok := cat.Lookup(arm.Target); ok {
			openingWindow = info.ContextWindow
		}
	}
	loop.ContextWindow = func(target provider.RouteTarget) int {
		if target.ID() == arm.Target.ID() {
			return openingWindow
		}
		if cat != nil {
			if info, _, ok := cat.Lookup(target); ok {
				return info.ContextWindow
			}
		}
		return 0
	}
	loop.OutputAllowance = func(target provider.RouteTarget, catalogMax int) int {
		binding := loop.Binding()
		if binding.Target.ID() == target.ID() {
			return provider.EffectiveOutputTokenAllowance(binding.Provider, target, catalogMax)
		}
		return provider.EffectiveOutputTokenAllowance(nil, target, catalogMax)
	}
}

// usageCollector attributes each turn's usage to the target that served it,
// which is the only way an escalating run can be priced correctly.
type usageCollector struct {
	agent.NopObserver
	loop     *agent.Loop
	byTarget map[provider.RouteTargetID]provider.Usage

	// rounds counts model calls, which is what a turn spends its budget on
	// and what a stopping change has to move.
	rounds int

	toolErrors map[string]int
}

func (c *usageCollector) TurnUsage(u session.Usage) {
	id := provider.RouteTargetID("unknown")
	if c.loop != nil {
		id = c.loop.Target.ID()
	}
	c.byTarget[id] = c.byTarget[id].Add(u.Usage)
	// One usage record per model call, so this is the round count without the
	// loop having to report it separately.
	c.rounds++
}

func (c *usageCollector) ToolEnd(call provider.ToolUse, _ permission.Request, res tools.Result, _ time.Duration) {
	if !res.IsError {
		return
	}
	if c.toolErrors == nil {
		c.toolErrors = map[string]int{}
	}
	c.toolErrors[call.Name+"/"+toolErrorClass(res.Content)]++
}

// toolErrorClass separates a call the model got wrong from a call that ran and
// failed. They are the same tool and opposite findings: the first is a schema
// or prompt problem, the second is the task being hard, and a single count
// per tool would average them into a number that moves for both reasons.
func toolErrorClass(content string) string {
	first := strings.ToLower(strings.TrimSpace(content))
	if i := strings.IndexAny(first, "\n"); i >= 0 {
		first = first[:i]
	}
	switch {
	case strings.Contains(first, "exactly one"), strings.Contains(first, "retired"),
		strings.Contains(first, "is not valid"), strings.Contains(first, "cannot unmarshal"),
		strings.Contains(first, "must be"), strings.Contains(first, "missing"):
		return "malformed"
	case strings.Contains(first, "not approved"), strings.Contains(first, "denied"):
		return "denied"
	}
	return "ran-and-failed"
}

// denyingAsker refuses a request and lets the turn continue, which is what an
// unattended session does. Erroring instead would end a run on its first
// network request, so a task the model could have finished another way would be
// recorded as unsolvable.
//
// The count matters: a corpus that trips approvals constantly is a corpus that
// needs looking at, and a silent denial hides that.
type denyingAsker struct{ denied int }

func (a *denyingAsker) Ask(_ context.Context, _ permission.Request, _ permission.Outcome) (permission.Response, error) {
	a.denied++
	return permission.Response{Approved: false}, nil
}
