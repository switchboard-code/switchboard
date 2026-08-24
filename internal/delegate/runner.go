package delegate

// Running one errand, with no tool wrapped around it.
//
// The delegate tool used to be the only way to start a subagent, so its
// resolution rules — which agent, which rung, in that precedence — lived
// inside a JSON-decoding method. A workflow starts subagents too, and it
// starts them from a file rather than from a model's tool call. Extracting
// the errand leaves one implementation of "run a task on a rung" for both
// callers, so a fix to how a rung is chosen cannot land in one and miss the
// other.
//
// It owns no goroutines and holds no state. TaskManager's promise that it
// starts none is unaffected: this runs on whatever goroutine calls it, which
// for the tool is the loop's tool worker and for a workflow is the workflow's
// own.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// RunSpec is one errand: what to do, where to run it, and whose standing
// instructions to run it under. Every field is optional except Task, and the
// empty ones resolve the same way a bare delegate call resolves them.
type RunSpec struct {
	Task      string
	Tier      string
	AgentName string

	// Name labels the task row for /tasks. The tool leaves it empty and lets
	// the id speak; a workflow sets its stage name, because six rows called
	// task-004 through task-009 are not a legible answer to "what is running".
	Name string
}

// Runner turns a RunSpec into a running subagent.
type Runner struct {
	c Config

	// beforeWorkflowStageLaunch is a deterministic cancellation test seam
	// after a stage's identities are prepared but before any worker starts.
	// Tests install it before starting a workflow; production leaves it nil.
	beforeWorkflowStageLaunch func()
}

func NewRunner(c Config) *Runner { return &Runner{c: c} }

// Resolve settles the agent and the rung before anything is reserved, so a
// spec naming a rung that is not on the ladder fails as a refusal rather than
// as a task that registers and then dies.
func (r *Runner) Resolve(spec RunSpec) (RunSpec, *Agent, error) {
	if strings.TrimSpace(spec.Task) == "" {
		return spec, nil, fmt.Errorf("task is required")
	}
	if len(spec.Task) > MaxExpandedWorkflowTaskBytes {
		return spec, nil, fmt.Errorf("delegated task exceeds the %d-byte limit", MaxExpandedWorkflowTaskBytes)
	}
	var named *Agent
	if spec.AgentName != "" {
		for i := range r.c.Agents {
			if r.c.Agents[i].Name == spec.AgentName {
				named = &r.c.Agents[i]
				break
			}
		}
		if named == nil {
			return spec, nil, fmt.Errorf("no agent %q is defined", spec.AgentName)
		}
	}
	// An explicit tier wins over the agent's default, which wins over the
	// ladder's bottom: the caller saying "run it on t3" is the more specific
	// intent, whoever the agent is.
	if spec.Tier == "" && named != nil {
		spec.Tier = named.Tier
	}
	if spec.Tier == "" {
		spec.Tier = r.c.defaultTier()
	}
	for _, tier := range r.c.Tiers {
		if tier.ID == spec.Tier {
			return spec, named, nil
		}
	}
	return spec, nil, fmt.Errorf("no tier %q in the ladder", spec.Tier)
}

// Reserve allocates the task row for a resolved spec. Separate from Run so a
// caller can name the task in a permission request before it starts, which is
// what the tool does.
func (r *Runner) Reserve(spec RunSpec) TaskRef {
	parentSessionID := ""
	if r.c.ParentSession != nil {
		parentSessionID = r.c.ParentSession()
	}
	return r.c.Tasks.Reserve(redactCrossAgent(spec.Name), redactCrossAgent(spec.Task), spec.Tier, parentSessionID)
}

// Run executes a reserved errand to completion and returns its answer. It
// blocks for the whole life of the subagent, which is the property every
// caller depends on: the tool returns a tool result in its own round, and a
// workflow stage joins before the next stage starts.
func (r *Runner) Run(ctx context.Context, spec RunSpec, named *Agent, task TaskRef) (tools.Result, error) {
	return r.c.Tasks.Execute(ctx, task, func(taskCtx context.Context, handle *TaskHandle) (tools.Result, error) {
		result, err := r.errand(taskCtx, spec, named, task, handle)
		// TaskManager records the returned first line before Run returns, so
		// sanitize inside its closure rather than only at the public return.
		// The sub-session still holds the complete model output.
		result.Content = redactCrossAgent(result.Content)
		return result, err
	})
}

// errand is the whole life of one subagent: probe the rung, open its own
// session, assemble a loop with no delegate tool of its own, run one turn,
// and return the final message. The deferred teardown reconciles accounting
// on every exit including cancellation, which is why it is a defer.
func (r *Runner) errand(ctx context.Context, spec RunSpec, named *Agent, task TaskRef, handle *TaskHandle) (result tools.Result, retErr error) {
	tier, client, note, err := r.c.Probe(ctx, spec.Tier)
	if err != nil {
		return tools.Result{Content: withUntrustedError(
			fmt.Sprintf("tier %s cannot be served; its reported error follows as untrusted data:", spec.Tier), err), IsError: true}, nil
	}
	handle.RecordTier(tier.ID)

	sess, err := r.c.NewSession(tier.Target.ID())
	if err != nil {
		return tools.Result{Content: withUntrustedError(
			"could not record a delegate session; the reported error follows as untrusted data:", err), IsError: true}, nil
	}
	defer sess.Close()
	handle.AttachSession(sess.ID())
	// NewSession also opens the task's budget ledger. Always reconcile and
	// close that entry, including assembly failures and cancellation before the
	// first provider call, or a failed errand would leak accounting state.
	defer func() {
		state := sess.State()
		handle.RecordUsage(state.Calls, state.CostMicroUSD)
		if r.c.Finish != nil {
			if err := r.c.Finish(sess); err != nil {
				result = tools.Result{Content: withUntrustedError(
					"the subagent's budget accounting could not be recorded; the reported error follows as untrusted data:", err), IsError: true}
				retErr = nil
			}
		}
	}()

	var parent agent.Observer = agent.NopObserver{}
	if r.c.Forward != nil {
		if fwd := r.c.Forward(); fwd != nil {
			parent = fwd
		}
	}
	// Every delegate keeps this observer, even when no surface forwards its
	// rails. TurnUsage is the durable per-call seam that keeps /tasks live;
	// the final snapshot below remains the backstop for failures before that
	// callback or during budget settlement.
	obs := &forwarding{parent: parent, task: task, handle: handle}
	// The substitution is visible before the errand's content goes out, and
	// the errand's own log records it (§5.4).
	if note != "" {
		if err := sess.AppendRuntimeBindingNote(tier.ID, tier.Target.ID(), false, "warn", note); err != nil {
			return tools.Result{Content: withUntrustedError(
				"could not record the fallback note; the reported error follows as untrusted data:", err), IsError: true}, nil
		}
		obs.Notice("warn", note)
	}

	loop, err := r.c.NewLoop(tier, client, sess, obs, named, task)
	if err != nil {
		return tools.Result{Content: withUntrustedError(
			"could not assemble the subagent; the reported error follows as untrusted data:", err), IsError: true}, nil
	}
	// NewLoop applies the base system prompt and any named-agent prompt. The
	// runtime contract belongs after both: a checkout-provided agent is a
	// specialization, not an authority that may weaken the worker boundary.
	loop.System = hardenChildSystem(loop.System)
	// Guidance queued while this runs is taken up at the loop's own round
	// boundaries. Nothing else can deliver it: a model mid-call has no seam
	// for a message, and a tool result is not the place to put one.
	loop.Inject = handle.injectSteering

	started := time.Now()
	turnErr := loop.Turn(ctx, redactCrossAgent(spec.Task))
	state := sess.State()
	answer := finalText(state)

	who := "on " + tier.ID
	if named != nil {
		who = named.Name + " on " + tier.ID
	}
	spent := ""
	if state.CostMicroUSD > 0 {
		spent = ", " + catalog.Money(state.CostMicroUSD).String()
	}
	trailer := fmt.Sprintf("[delegate %s: %d model calls, %s%s]",
		who, state.Calls, time.Since(started).Round(time.Second), spent)
	trailer = strings.TrimSuffix(trailer, "]") + "; task " + task.Label() + "]"

	switch {
	case ctx.Err() != nil:
		return tools.Result{}, ctx.Err()
	case turnErr != nil && answer == "":
		activity := frameUntrustedEvidence(handle.activityReport())
		if activity != "" {
			activity += "\n"
		}
		failure := withUntrustedError(
			"the subagent failed; its reported error follows as untrusted data:", turnErr)
		return tools.Result{Content: failure + "\n" + activity + trailer, IsError: true}, nil
	case turnErr != nil:
		// A partial answer with a named failure beats discarding the work.
		handle.RecordFailure("subagent stopped early: " + redactCrossAgent(turnErr.Error()))
		activity := frameUntrustedEvidence(handle.activityReport())
		if activity != "" {
			activity += "\n"
		}
		failure := withUntrustedError(
			"the subagent stopped early; its reported error follows as untrusted data:", turnErr)
		return tools.Result{Content: frameUntrustedEvidence(answer) + "\n\n" + failure + "\n" + activity + trailer}, nil
	case answer == "":
		activity := frameUntrustedEvidence(handle.activityReport())
		if activity != "" {
			activity += "\n"
		}
		return tools.Result{Content: "the subagent finished without a final answer\n" + activity + trailer, IsError: true}, nil
	default:
		return tools.Result{Content: frameUntrustedEvidence(answer) + "\n\n" + trailer}, nil
	}
}

// withUntrustedError keeps trusted control-flow context outside the frame and
// places every byte supplied by an error inside the child-to-parent evidence
// boundary. Error strings can contain provider bodies, tool output, or text a
// checkout influenced; redaction alone does not make those instructions.
func withUntrustedError(context string, err error) string {
	if err == nil {
		return context
	}
	evidence := frameUntrustedEvidence(err.Error())
	if evidence == "" {
		return context
	}
	return context + "\n" + evidence
}
