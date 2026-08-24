// Package delegate lets the primary model hand a scoped task to a subagent
// on a rung of its choosing.
//
// This is the ladder's idea applied to orchestration: a search, a survey, or
// a mechanical edit does not need the primary's rung, and a subagent on t1
// with its own fresh context is often cheaper than the primary doing the
// work inside a context it then drags forward forever. The tier parameter is
// the visible, priced version of that decision — the same bet the router
// makes, made available to the model. §19.2 phase 6 expects delegation to be
// evaluated against sticky single-primary baselines; that eval has not run,
// so this ships as a tool the user can watch, not as a claim it wins.
//
// Depth is one: a subagent's registry has no delegate tool, because an agent
// that can recurse is an agent whose cost has no ceiling.
package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// MaxRounds bounds a subagent's turn tighter than the primary's, because a
// subagent stuck in a retry cycle has no user watching to interrupt it.
const MaxRounds = 25

// Preamble is appended to the subagent's system blocks during assembly. The
// Runner adds RuntimeContract after every assembled block; keeping the short
// role description separate lets the final contract remain last even when a
// named agent contributes standing instructions.
const Preamble = "You are a delegated subagent. Complete the task you are given and reply " +
	"with your findings. Your final message becomes the evidence report returned to " +
	"the agent that delegated the task after credential redaction and trust-boundary " +
	"framing. Only that report normally returns, so put everything that matters in it."

// Config wires a delegate tool to the session's machinery. Every closure is
// supplied by the surface (cmd/sb), because building a provider client, a
// session, and a loop is assembly, and assembly lives where the credentials
// and the catalog do.
type Config struct {
	// Tiers is the ladder, for validation and for the default rung.
	Tiers []config.Tier

	// Probe resolves a tier to a live client, the same way a manual /t2
	// switch does — including the tier's own fallback list (§5.4). A
	// non-empty note reports that a fallback is serving, and it is shown
	// before the errand's content goes anywhere.
	Probe func(ctx context.Context, tierID string) (config.Tier, provider.Provider, string, error)

	// NewSession creates the subagent's own session record. Sub-sessions are
	// real logs — crash-safe, auditable — kept out of the primary store so
	// /resume never offers a context that was never the user's.
	NewSession func(target provider.RouteTargetID) (*session.Session, error)

	// NewLoop assembles a loop for the subagent: fresh registry without
	// delegate, the shared permission engine and asker, the parent's hooks.
	// A non-nil named agent carries the definition's prompt and tool grant
	// for the assembly to apply.
	NewLoop func(tier config.Tier, client provider.Provider, sess *session.Session, obs agent.Observer, named *Agent, task TaskRef) (*agent.Loop, error)

	// Finish runs after the subagent loop stops and before its result returns to
	// the primary. Surfaces use it to reconcile the sub-session's priced calls
	// into the primary's authoritative budget ledger without copying Usage.
	Finish func(sess *session.Session) error

	// Forward receives the subagent's tool activity so the user watches the
	// work as it happens. Nil means unobserved.
	Forward func() agent.Observer

	// Agents are the named definitions discovered at session assembly,
	// sorted by name. Empty leaves the tool exactly as it is without them —
	// same schema, same description — so a session with no definitions
	// renders byte-identical requests.
	Agents []Agent

	// Tasks owns bounded parallelism, task-local cancellation, status, and
	// approval serialization. Nil gets a private manager with the default
	// bound; product assembly supplies the manager its /tasks surface reads.
	Tasks *TaskManager

	// ParentSession returns the primary session owning a task. It is read at
	// Plan time, when provider call order also allocates task identity.
	ParentSession func() string
}

func (c Config) defaultTier() string {
	if len(c.Tiers) == 0 {
		return ""
	}
	return c.Tiers[0].ID
}

// New builds the delegate tool. It returns an error when the ladder is too
// small to delegate on, so the tool is absent rather than broken.
func New(c Config) (tools.Tool, error) {
	if len(c.Tiers) == 0 {
		return nil, fmt.Errorf("delegate needs a configured ladder")
	}
	if c.Probe == nil || c.NewSession == nil || c.NewLoop == nil {
		return nil, fmt.Errorf("delegate is missing assembly wiring")
	}
	agents, err := prepareConfiguredAgents(c.Agents)
	if err != nil {
		return nil, err
	}
	c.Agents = agents
	manager := c.Tasks
	if manager == nil {
		manager = NewTaskManager(DefaultMaxParallel)
	}
	c.Tasks = manager
	return &delegateTool{c: c, tasks: manager, runner: NewRunner(c)}, nil
}

func prepareConfiguredAgents(input []Agent) ([]Agent, error) {
	agents := append([]Agent(nil), input...)
	seen := make(map[string]bool, len(agents))
	for i := range agents {
		agent := &agents[i]
		if agent.Name == "" {
			return nil, fmt.Errorf("delegate agent has an empty name")
		}
		if len(agent.Name) > maxAgentNameBytes {
			return nil, fmt.Errorf("delegate agent name exceeds the %d-byte limit", maxAgentNameBytes)
		}
		if err := validateAgentMetadata("name", agent.Name); err != nil {
			return nil, err
		}
		if redactCrossAgent(agent.Name) != agent.Name {
			return nil, fmt.Errorf("delegate agent name contains credential-like text")
		}
		if seen[agent.Name] {
			return nil, fmt.Errorf("delegate agent name %q is ambiguous", agent.Name)
		}
		seen[agent.Name] = true
		if redactCrossAgent(agent.Tier) != agent.Tier {
			return nil, fmt.Errorf("delegate agent tier contains credential-like text")
		}
		if len(agent.Tier) > maxAgentTierBytes {
			return nil, fmt.Errorf("delegate agent tier exceeds the %d-byte limit", maxAgentTierBytes)
		}
		if err := validateAgentMetadata("tier", agent.Tier); err != nil {
			return nil, err
		}
		if len(agent.Tools) > maxAgentTools {
			return nil, fmt.Errorf("delegate agent tool grant exceeds the %d-tool limit", maxAgentTools)
		}
		agent.Tools = append([]string(nil), agent.Tools...)
		for _, name := range agent.Tools {
			if redactCrossAgent(name) != name {
				return nil, fmt.Errorf("delegate agent tool grant contains credential-like text")
			}
			if err := validateAgentMetadata("tool grant", name); err != nil {
				return nil, err
			}
		}
		agent.Description = redactCrossAgent(agent.Description)
		if len(agent.Description) > maxAgentDescriptionBytes {
			return nil, fmt.Errorf("delegate agent description exceeds the %d-byte limit", maxAgentDescriptionBytes)
		}
		if err := validateAgentMetadata("description", agent.Description); err != nil {
			return nil, err
		}
		if int64(len(agent.Prompt)) > maxAgentDefinitionBytes {
			return nil, fmt.Errorf("delegate agent prompt exceeds the %d-byte limit", maxAgentDefinitionBytes)
		}
		if err := validateAgentDocument(agent.Prompt); err != nil {
			return nil, err
		}
		agent.Prompt = redactCrossAgent(agent.Prompt)
	}
	return agents, nil
}

type delegateTool struct {
	c      Config
	tasks  *TaskManager
	runner *Runner
}

func (t *delegateTool) Name() string { return "delegate" }

func (t *delegateTool) Description() string {
	var ids []string
	for _, tier := range t.c.Tiers {
		ids = append(ids, tier.ID)
	}
	desc := fmt.Sprintf("Hand a self-contained task to a subagent with a fresh context and return "+
		"its final answer. Independent delegate calls in one response can overlap (at most %d active) while results return in call order. "+
		"Each subagent has the core tools but cannot delegate further and starts with no knowledge of this conversation, so the task must carry "+
		"everything it needs: file paths, constraints, what to return. tier picks the "+
		"ladder rung it runs on (%s); the default %s is the cheap rung, right for "+
		"searches, surveys, and mechanical work. Use a higher tier only when the "+
		"subtask itself is hard.", t.tasks.MaxParallel(), strings.Join(ids, ", "), t.c.defaultTier())
	if len(t.c.Agents) == 0 {
		return desc
	}
	var b strings.Builder
	b.WriteString(desc)
	b.WriteString(" Named agents carry standing instructions and their own default rung and " +
		"tool grant; pass one as agent when its charter fits the subtask:")
	for _, ag := range t.c.Agents {
		fmt.Fprintf(&b, "\n- %s", redactCrossAgent(ag.Name))
		if ag.Description != "" {
			fmt.Fprintf(&b, ": %s", redactCrossAgent(ag.Description))
		}
		if ag.Tier != "" {
			fmt.Fprintf(&b, " (runs on %s)", redactCrossAgent(ag.Tier))
		}
	}
	return b.String()
}

// ParallelSafe remains false because a delegated loop may write. The agent
// scheduler uses ParallelBatchKey to overlap only delegate-with-delegate
// batches; mixed read/delegate batches retain provider call order.
func (t *delegateTool) ParallelSafe() bool { return false }

func (t *delegateTool) ParallelBatchKey() string { return "delegate" }

func (t *delegateTool) Schema() json.RawMessage {
	if len(t.c.Agents) == 0 {
		return json.RawMessage(`{
  "type": "object",
  "properties": {
    "task": {"type": "string", "description": "Complete instructions for the subagent, self-contained: it starts with no context from this conversation."},
    "tier": {"type": "string", "description": "Ladder rung to run on, e.g. t1. Defaults to the bottom rung."}
  },
  "required": ["task"]
}`)
	}
	var names []string
	for _, ag := range t.c.Agents {
		names = append(names, ag.Name)
	}
	quoted, _ := json.Marshal(names)
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "task": {"type": "string", "description": "Complete instructions for the subagent, self-contained: it starts with no context from this conversation."},
    "tier": {"type": "string", "description": "Ladder rung to run on, e.g. t1. Defaults to the agent's rung, then the bottom rung."},
    "agent": {"type": "string", "enum": ` + string(quoted) + `, "description": "Named agent to run as: its standing instructions, default rung, and tool grant apply."}
  },
  "required": ["task"]
}`)
}

type delegateInput struct {
	Task  string `json:"task"`
	Tier  string `json:"tier"`
	Agent string `json:"agent"`
}

func (t *delegateTool) Plan(input json.RawMessage) (tools.Plan, error) {
	var in delegateInput
	if err := json.Unmarshal(input, &in); err != nil {
		return tools.Plan{}, fmt.Errorf("delegate: %w", err)
	}
	// Resolution lives on the runner, because a workflow starts subagents too
	// and the rules for which agent and which rung must not fork.
	spec, named, err := t.runner.Resolve(RunSpec{Task: in.Task, Tier: in.Tier, AgentName: in.Agent})
	if err != nil {
		return tools.Plan{}, fmt.Errorf("delegate: %w", err)
	}
	in.Tier = spec.Tier
	task := t.runner.Reserve(spec)

	// Spawning is free and touches nothing; every call the subagent then
	// makes goes through the shared permission engine on its own merits, so
	// the spawn itself carries the read effect.
	// The permission detail and /tasks row are parent-facing renderings of a
	// child handoff. Keep a key-shaped value out of both even though the full
	// tool call remains in the primary session record.
	summary := redactCrossAgent(in.Task)
	if runes := []rune(summary); len(runes) > 80 {
		summary = string(runes[:80]) + "…"
	}
	who := in.Tier
	if named != nil {
		who = named.Name + " on " + in.Tier
	}
	return tools.Plan{
		Request: permission.Request{
			Tool:   t.Name(),
			Effect: permission.EffectRead,
			Detail: fmt.Sprintf("[%s] %s → %s", task.Label(), who, summary),
		},
		Run: func(ctx context.Context) (tools.Result, error) {
			return t.runner.Run(ctx, spec, named, task)
		},
	}, nil
}

// finalText is the last complete assistant message's text, which the preamble
// told the subagent becomes its report. The full text stays in this state;
// redaction and framing happen only on the copy handed back to the parent.
func finalText(state session.State) string {
	for i := len(state.Messages) - 1; i >= 0; i-- {
		m := state.Messages[i]
		if m.Role != provider.RoleAssistant || m.Incomplete {
			continue
		}
		if s := strings.TrimSpace(m.Text()); s != "" {
			return s
		}
	}
	return ""
}

// forwarding passes the subagent's tool activity to the parent's observer so
// the rails render live, and swallows the rest: streamed text would
// interleave with the primary's, usage is the sub-session's record, and a
// todo list would collide with the primary's in any surface that renders
// registry state.
type forwarding struct {
	parent       agent.Observer
	task         TaskRef
	handle       *TaskHandle
	calls        int
	costMicroUSD int64
}

func (f *forwarding) ThinkingDelta(string) {}
func (f *forwarding) TextDelta(string)     {}

// handle, when set, receives the same activity the parent observer sees, so a
// caller can ask what a running task is doing without opening its session.
func (f *forwarding) note(what string) {
	if f.handle != nil {
		f.handle.RecordActivity(what)
	}
}

func (f *forwarding) ToolStart(call provider.ToolUse, req permission.Request) {
	if call.Name == "todo" {
		return
	}
	call.ID = f.task.ID + "/" + call.ID
	req = attributeRequest(f.task, req)
	f.parent.ToolStart(call, req)
}

func (f *forwarding) ToolEnd(call provider.ToolUse, req permission.Request, res tools.Result, took time.Duration) {
	if call.Name == "todo" {
		return
	}
	// The completed call, not the started one: a status answer wants what the
	// task has done, and a call in flight is already visible as the last line
	// with no verdict beside it.
	verdict := "ok"
	if res.IsError {
		verdict = "failed"
	}
	// The duration is kept, not dropped. A verdict is a lagging measure: it
	// changes only once something has already gone wrong, while the time a
	// call took moves earlier and is sitting right here in the arguments.
	f.note(fmt.Sprintf("%s %s %s %s", call.Name, verdict,
		took.Round(time.Millisecond), describeCall(req)))
	call.ID = f.task.ID + "/" + call.ID
	req = attributeRequest(f.task, req)
	f.parent.ToolEnd(call, req, res, took)
}

// describeCall is the shortest true thing about what a call touched: the
// command it ran or the path it took, and nothing when it named neither.
func describeCall(req permission.Request) string {
	switch {
	case len(req.Argv) > 0:
		return strings.Join(req.Argv, " ")
	case req.Path != "":
		return req.Path
	}
	return ""
}

func (f *forwarding) ToolBatchEnd(ctx context.Context) { f.parent.ToolBatchEnd(ctx) }

func (f *forwarding) Notice(level, text string) {
	f.parent.Notice(level, "["+f.task.Label()+"] "+text)
}
func (f *forwarding) TurnUsage(usage session.Usage) {
	if f.handle == nil {
		return
	}
	// The loop emits TurnUsage only after this provider receipt is durable and
	// its budget attempt has settled. Callbacks are ordered on the delegated
	// loop's goroutine, so these totals mirror the fresh sub-session without
	// repeatedly cloning its growing conversation state.
	f.calls++
	f.costMicroUSD += usage.CostMicroUSD
	f.handle.RecordUsage(f.calls, f.costMicroUSD)
}
