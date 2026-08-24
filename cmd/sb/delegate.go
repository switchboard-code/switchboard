package main

// Delegate assembly. The tool itself lives in internal/delegate; what
// belongs here is the wiring only a surface has: provider probing, session
// stores, and where the subagent's rails render.

import (
	"context"
	"fmt"
	"math"
	"sync"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/delegate"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/hooks"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/skills"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// delegateForward late-binds where subagent activity renders. The delegate
// tool is registered before either surface exists, and the forwarding target
// is the surface's raw observer, deliberately not the watcher: a subagent's
// error spikes are its own, and feeding them to the primary's escalation
// policy would move the primary on evidence from a different context.
type delegateForward struct {
	mu  sync.Mutex
	obs agent.Observer
}

func (d *delegateForward) set(obs agent.Observer) {
	d.mu.Lock()
	d.obs = obs
	d.mu.Unlock()
}

func (d *delegateForward) get() agent.Observer {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.obs
}

var subagentForward = &delegateForward{}
var subagentTasks = delegate.NewTaskManager(delegate.DefaultMaxParallel)

// subagentRunner is the assembled errand runner, stashed here for /workflow.
// registerDelegate builds the Config thirty lines before main decides which
// surface to open, so the Config literal cannot close over anything the TUI
// owns and the runner has to be reachable rather than passed.
var subagentRunner = &delegateRunnerHolder{}

// subagentWorkflows are the definitions found at assembly, discovered once
// for the same frozen-zone reason agents are: a file added mid-process is
// picked up by the next run.
var subagentWorkflows []delegate.Workflow
var subagentWorkflowNotes []string

type delegateRunnerHolder struct {
	mu sync.Mutex
	r  *delegate.Runner
}

func (h *delegateRunnerHolder) set(r *delegate.Runner) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.r = r
}

func (h *delegateRunnerHolder) get() *delegate.Runner {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.r
}

// delegateLedgerTracker records successful primary-ledger settlements per
// sub-session. A shared primary-cost baseline cannot attribute overlapping
// delegates: one task's charge would look like another task's settlement.
// Reconcile adds only the task-local gap left when a settlement append failed
// after Usage became durable in the sub-session.
type delegateLedgerTracker struct {
	mu      sync.Mutex
	settled map[string]int64
}

func (d *delegateLedgerTracker) mark(sub *session.Session) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.settled == nil {
		d.settled = make(map[string]int64)
	}
	d.settled[sub.ID()] = 0
}

func (d *delegateLedgerTracker) settle(primary, sub *session.Session, id, outcome string, charge catalog.Money) error {
	if err := primary.SettleBudgetAttempt(id, outcome, int64(charge)); err != nil {
		return err
	}
	if charge <= 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	current, ok := d.settled[sub.ID()]
	if !ok {
		return fmt.Errorf("delegate %s has no budget ledger", sub.ID())
	}
	if int64(charge) > math.MaxInt64-current {
		return fmt.Errorf("delegate %s budget accounting overflow", sub.ID())
	}
	d.settled[sub.ID()] = current + int64(charge)
	return nil
}

func (d *delegateLedgerTracker) reconcile(primary, sub *session.Session) error {
	d.mu.Lock()
	recorded, ok := d.settled[sub.ID()]
	delete(d.settled, sub.ID())
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("delegate %s has no budget ledger", sub.ID())
	}
	missing := sub.State().CostMicroUSD - recorded
	if missing <= 0 {
		return nil
	}
	return primary.AppendBudgetTransfer("delegate-reconcile:"+sub.ID(), missing, 0)
}

// registerDelegate adds the delegate tool to the primary registry. The
// subagent gets a fresh registry — core tools, no delegate (depth one), no
// MCP — the shared permission engine and asker, the same hooks, and its own
// session in a store /resume never lists. It returns the named agent
// definitions it discovered and any notes their loading produced, for
// /agents to show.
func registerDelegate(
	registry *tools.Registry,
	cfg *config.Config,
	cat *catalog.Catalog,
	reg *providers,
	primary *agent.Loop,
	hookSet *hooks.Set,
	capability execution.Capability,
	workspace string,
	undoRec *checkpoint.Recorder,
	budget *budgetState,
	skillList []skills.Skill,
) ([]delegate.Agent, []string, error) {
	subagentRunner.set(nil)
	subagentWorkflows = nil
	subagentWorkflowNotes = nil
	if len(cfg.Tiers) == 0 {
		return nil, nil, nil // no ladder, nothing to delegate on
	}

	subStore, err := delegateStore()
	if err != nil {
		return nil, nil, fmt.Errorf("delegate needs its session store: %w", err)
	}
	var ledger delegateLedgerTracker
	var hookExecutionGate *agent.ExecutionGate
	if !hookSet.Empty() {
		hookExecutionGate = agent.NewExecutionGate()
	}

	// A definition naming a rung this ladder does not have still loads — it
	// was probably written for a taller ladder — but runs on the default
	// rung, and the note says so rather than letting every call error.
	agents, agentNotes := delegate.LoadAgents(workspace, tools.CoreNames())
	for i := range agents {
		if agents[i].Tier == "" {
			continue
		}
		if _, ok := cfg.Tier(agents[i].Tier); !ok {
			agentNotes = append(agentNotes, fmt.Sprintf(
				"agent %s names tier %s, which is not on the ladder; it will run on the default rung",
				agents[i].Name, agents[i].Tier))
			agents[i].Tier = ""
		}
	}

	config := delegate.Config{
		Tiers:  cfg.Tiers,
		Agents: agents,
		Tasks:  subagentTasks,
		ParentSession: func() string {
			return primary.Session.ID()
		},
		Probe: func(ctx context.Context, tierID string) (config.Tier, provider.Provider, string, error) {
			return probeDelegateTier(ctx, cfg, reg, tierID)
		},
		NewSession: func(target provider.RouteTargetID) (*session.Session, error) {
			sess, err := subStore.Create(workspace, target, cat.Revision)
			if err != nil {
				return nil, err
			}
			ledger.mark(sess)
			return sess, nil
		},
		NewLoop: func(tier config.Tier, client provider.Provider, sess *session.Session, obs agent.Observer, named *delegate.Agent, task delegate.TaskRef) (*agent.Loop, error) {
			subRegistry, err := tools.NewRegistryWithExecution(workspace, primary.Tools.Execution())
			if err != nil {
				return nil, err
			}
			// A subagent searches more than anything else, so it gets the
			// structural tool too. Before Restrict on purpose: a named
			// agent's grant validates against the core suite, so a
			// restricted agent loses astgrep with everything else unnamed,
			// which is right — a grant written on one machine must not
			// depend on another machine's binaries.
			addStructuralSearch(subRegistry)
			// Computer use joins under the astgrep rule: a conditional tool
			// an unrestricted subagent keeps and a restricted one loses with
			// everything else unnamed. Its calls still carry the external
			// effect through the shared engine, so a delegated errand asks
			// the same user the primary would.
			addComputerUse(subRegistry)
			// Skills too, and for the same reason as astgrep's placement: a
			// named agent's grant validates against the core suite, so a
			// restricted agent loses skill with everything else unnamed.
			if modelSkills := skills.ModelVisible(skillList); len(modelSkills) > 0 {
				if err := subRegistry.AddExternal(skills.NewTool(modelSkills)); err != nil {
					return nil, err
				}
			}
			// A named agent's grant narrows the suite before the first
			// request; the grant was validated at load, so an error here is
			// wiring, not a typo.
			if named != nil && len(named.Tools) > 0 {
				if err := subRegistry.Restrict(named.Tools); err != nil {
					return nil, err
				}
			}
			// The sub-registry shares the primary recorder and the sub-loop
			// opens no scope of its own, so a delegate's edits file under
			// the turn that delegated and one /undo takes back both.
			subRegistry.SetCheckpoints(undoRec)
			// The mode is read at call time from the shared engine, so a
			// session switched to plan mode delegates plan-mode subagents.
			system := agent.SystemPrompt(workspace, primary.Perms.Mode(), capability)
			system = append(system, provider.Text{Text: delegate.Preamble})
			if named != nil {
				system = append(system, provider.Text{Text: named.Prompt})
			}
			sub := &agent.Loop{
				Provider:          client,
				Target:            tier.Target,
				Tools:             subRegistry,
				Perms:             primary.Perms,
				Asker:             subagentTasks.AttributedAsker(task, primary.Asker),
				Session:           sess,
				Catalog:           cat,
				Cache:             cacheFor(tier.Target, cat),
				System:            system,
				Observer:          obs,
				MaxToolRounds:     delegate.MaxRounds,
				Hooks:             hookSet,
				ToolExecutionGate: hookExecutionGate,
				ContextWindow:     primary.ContextWindow,
				OutputAllowance:   primary.OutputAllowance,
			}
			if err := sub.BindSession(sess); err != nil {
				_ = sess.Close()
				return nil, fmt.Errorf("restore delegated session context: %w", err)
			}
			// The errand runs under the same ceiling as the session that
			// spawned it, counting what both logs have priced so far. A
			// delegated task is not a way around /budget.
			wireBudget(sub, budgetGate(budget, cat,
				func() provider.RouteTarget { return sub.Binding().Target },
				func() catalog.Money {
					return catalog.Money(primary.Session.State().AccountedCostMicroUSD())
				},
				func() string { return primary.Session.ID() }).withLedger(
				func() catalog.Money { return catalog.Money(primary.Session.State().RetryReserveMicroUSD) },
				func(amount catalog.Money) (string, error) { return primary.Session.BeginBudgetAttempt(int64(amount)) },
				func(id, outcome string, charge catalog.Money) error {
					return ledger.settle(primary.Session, sess, id, outcome, charge)
				}, true).withOutputAllowance(sub.OutputAllowance))
			return sub, nil
		},
		Finish: func(sess *session.Session) error {
			return ledger.reconcile(primary.Session, sess)
		},
		Forward: subagentForward.get,
	}

	tool, err := delegate.New(config)
	if err != nil {
		return nil, agentNotes, err
	}
	// The same assembled Config drives /workflow, so a workflow's subagents
	// are the delegate tool's subagents in every respect that matters: the
	// same ladder, the same permission engine, the same budget ceiling, the
	// same sub-session store.
	config.Tasks = subagentTasks
	subagentRunner.set(delegate.NewRunner(config))

	workflows, workflowNotes := delegate.LoadWorkflows(workspace)
	subagentWorkflows = workflows
	subagentWorkflowNotes = append([]string(nil), workflowNotes...)
	agentNotes = append(agentNotes, workflowNotes...)

	return agents, agentNotes, registry.AddExternal(tool)
}

// probeDelegateTier applies destination policy to the concrete target that
// will receive the subagent prompt, including an availability fallback. The
// model chooses this rung, so checking only its configured primary would let a
// disallowed fallback carry workspace content around the router's hard gate.
func probeDelegateTier(ctx context.Context, cfg *config.Config, reg *providers, tierID string) (config.Tier, provider.Provider, string, error) {
	tier, ok := cfg.Tier(tierID)
	if !ok {
		return config.Tier{}, nil, "", fmt.Errorf("no tier %s", tierID)
	}
	if err := destinationAllowed(cfg, tier.Target); err != nil {
		return config.Tier{}, nil, "", err
	}
	return reg.probeTierFallbackFeasible(ctx, tier, func(concrete config.Tier) error {
		return destinationAllowed(cfg, concrete.Target)
	})
}
