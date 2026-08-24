package delegate

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

// recordingProvider answers every turn with a fixed line and keeps what it was
// asked, so a test can prove a stage actually saw the previous stage's output
// rather than trusting the plumbing to have carried it.
type recordingProvider struct {
	mu     sync.Mutex
	answer string
	seen   []string
}

func (p *recordingProvider) Name() string { return "recording" }

func (p *recordingProvider) Stream(_ context.Context, _ provider.RouteTarget, req provider.Request) (provider.EventStream, error) {
	p.mu.Lock()
	for _, m := range req.Messages {
		p.seen = append(p.seen, m.Text())
	}
	answer := p.answer
	p.mu.Unlock()
	return &oneTurnStream{events: []provider.Event{
		{Type: provider.EventTextDelta, Text: answer},
		{Type: provider.EventDone, StopReason: provider.StopEndTurn},
	}}, nil
}

func (p *recordingProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}

func (p *recordingProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true, Tools: provider.ToolsParallel}, nil
}

func workflowRunner(t *testing.T, p provider.Provider) *Runner {
	t.Helper()
	cfg := testConfig(t, "unused")
	cfg.Tasks = NewTaskManager(4)
	cfg.Probe = func(_ context.Context, tierID string) (config.Tier, provider.Provider, string, error) {
		for _, tier := range ladder() {
			if tier.ID == tierID {
				return tier, p, "", nil
			}
		}
		t.Fatalf("probe asked for unknown tier %s", tierID)
		return config.Tier{}, nil, "", nil
	}
	return NewRunner(cfg)
}

func TestWorkflowPreflightUsesRuntimeResolution(t *testing.T) {
	runner := NewRunner(Config{
		Tiers: ladder(), Agents: []Agent{{Name: "reviewer", Tier: "t2"}},
	})
	valid := Workflow{Name: "valid", Stages: []Stage{{Name: "survey", Tasks: []WorkflowTask{{
		Task: "inspect $1", Agent: "reviewer",
	}}}}}
	if err := runner.PreflightWorkflow(valid, "internal/agent"); err != nil {
		t.Fatalf("valid preflight: %v", err)
	}
	validCarry := Workflow{Name: "carry", Stages: []Stage{
		{Name: "survey", Tasks: []WorkflowTask{{Task: "inspect"}}},
		{Name: "decide", Carry: true, Tasks: []WorkflowTask{{Task: "$ARGUMENTS"}}},
	}}
	if err := runner.PreflightWorkflow(validCarry, ""); err != nil {
		t.Fatalf("carry preflight rejected a task runtime makes non-empty: %v", err)
	}

	for name, wf := range map[string]Workflow{
		"unknown tier":    {Stages: []Stage{{Name: "one", Tasks: []WorkflowTask{{Task: "inspect", Tier: "t99"}}}}},
		"unknown agent":   {Stages: []Stage{{Name: "one", Tasks: []WorkflowTask{{Task: "inspect", Agent: "ghost"}}}}},
		"empty expansion": {Stages: []Stage{{Name: "one", Tasks: []WorkflowTask{{Task: "$ARGUMENTS"}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := runner.PreflightWorkflow(wf, ""); err == nil {
				t.Fatal("invalid workflow passed pure preflight")
			}
		})
	}
}

func TestExpandWorkflowArgumentsFindsComposedCredentialWithoutMutatingSource(t *testing.T) {
	source := Workflow{Stages: []Stage{{Tasks: []WorkflowTask{{Task: "use ghp_$ARGUMENTS"}}}}}
	expanded, err := ExpandWorkflowArguments(source, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	if got := expanded.Stages[0].Tasks[0].Task; got != "use "+boundaryTestToken {
		t.Fatalf("expanded task = %q", got)
	}
	if got := source.Stages[0].Tasks[0].Task; got != "use ghp_$ARGUMENTS" {
		t.Fatalf("source workflow was mutated: %q", got)
	}
}

func TestExpandedWorkflowExecutesExactGatedBytesWithoutSecondSubstitution(t *testing.T) {
	const half = "aaaaaaaaaaaaaaaaaa"
	const composed = "ghp_" + half + half
	const gated = "ghp_" + half + "$ARGUMENTS" + half

	p := &recordingProvider{answer: "done"}
	cfg := testConfig(t, "done")
	cfg.Tasks = NewTaskManager(4)
	cfg.Probe = func(_ context.Context, tierID string) (config.Tier, provider.Provider, string, error) {
		for _, tier := range ladder() {
			if tier.ID == tierID {
				return tier, p, "", nil
			}
		}
		t.Fatalf("probe asked for unknown tier %s", tierID)
		return config.Tier{}, nil, "", nil
	}
	newSession := cfg.NewSession
	var sessionPath string
	cfg.NewSession = func(target provider.RouteTargetID) (*session.Session, error) {
		sess, err := newSession(target)
		if sess != nil {
			sessionPath = sess.Path()
		}
		return sess, err
	}
	runner := NewRunner(cfg)
	expanded, err := ExpandWorkflowArguments(Workflow{
		Name: "literal-template", Stages: []Stage{{Name: "one", Tasks: []WorkflowTask{{Task: "$ARGUMENTS"}}}},
	}, gated)
	if err != nil {
		t.Fatal(err)
	}
	if got := expanded.Stages[0].Tasks[0].Task; got != gated {
		t.Fatalf("gated task = %q, want exact %q", got, gated)
	}
	if err := runner.PreflightExpandedWorkflow(expanded); err != nil {
		t.Fatal(err)
	}
	var progress []string
	result := runner.RunExpandedWorkflow(context.Background(), expanded, func(text string) {
		progress = append(progress, text)
	})
	if result.Err != nil {
		t.Fatal(result.Err)
	}

	p.mu.Lock()
	providerText := strings.Join(p.seen, "\n")
	p.mu.Unlock()
	if !strings.Contains(providerText, gated) || strings.Contains(providerText, composed) {
		t.Fatalf("provider request did not preserve exact gated bytes: %q", providerText)
	}
	if sessionPath == "" {
		t.Fatal("delegate session was not created")
	}
	state, err := session.ReadState(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	var durable strings.Builder
	for _, message := range state.Messages {
		durable.WriteString(message.Text())
		durable.WriteByte('\n')
	}
	if !strings.Contains(durable.String(), gated) || strings.Contains(durable.String(), composed) {
		t.Fatalf("durable delegate session did not preserve exact gated bytes: %q", durable.String())
	}
	if strings.Contains(strings.Join(progress, "\n"), composed) {
		t.Fatalf("progress exposed a second-pass composed credential: %q", progress)
	}
	for _, task := range cfg.Tasks.List() {
		if strings.Contains(task.Name+"\n"+task.Error+"\n"+strings.Join(task.Activity, "\n"), composed) {
			t.Fatalf("task status exposed a second-pass composed credential: %+v", task)
		}
	}
	if _, err := ExpandWorkflowArguments(expanded, ""); err == nil {
		t.Fatal("an expanded workflow was accepted for a second substitution pass")
	}
}

func TestWorkflowArgumentExpansionExactCapAndPlusOne(t *testing.T) {
	for _, tc := range []struct {
		name      string
		arguments string
		wantErr   bool
	}{
		{name: "exact", arguments: strings.Repeat("x", MaxExpandedWorkflowTaskBytes)},
		{name: "plus one", arguments: strings.Repeat("x", MaxExpandedWorkflowTaskBytes+1), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := Workflow{Stages: []Stage{{Name: "expand", Tasks: []WorkflowTask{{Task: "$ARGUMENTS"}}}}}
			expanded, err := ExpandWorkflowArguments(source, tc.arguments)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "1048576-byte limit") {
					t.Fatalf("expansion error = %v", err)
				}
				if expanded.Stages != nil {
					t.Fatalf("failed expansion returned a partial workflow: %+v", expanded)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := len(expanded.Stages[0].Tasks[0].Task); got != MaxExpandedWorkflowTaskBytes {
				t.Fatalf("expanded bytes = %d", got)
			}
		})
	}
}

func TestWorkflowArgumentAmplificationRefusesAtTheOutputCap(t *testing.T) {
	// The source is small and the argument is individually below the cap. Only
	// their multiplicative substitution crosses it.
	template := strings.Repeat("$1", 2_048)
	arguments := strings.Repeat("z", 1_024)
	_, err := ExpandWorkflowArguments(Workflow{Stages: []Stage{{Name: "amplify", Tasks: []WorkflowTask{{Task: template}}}}}, arguments)
	if err == nil || !strings.Contains(err.Error(), "1048576-byte limit") {
		t.Fatalf("amplified expansion error = %v", err)
	}
}

func TestWorkflowCarryPreflightReservesWorstCaseEnvelope(t *testing.T) {
	runner := NewRunner(Config{Tiers: ladder()})
	allowance, ok := maxWorkflowCarryEnvelopeBytes(1)
	if !ok {
		t.Fatal("one carried answer has no allowance")
	}
	maxAnswer := strings.Repeat("\U0010ffff", MaxCarriedAnswerRune+1)
	if got := len(Carry([]string{maxAnswer}, "")); got != allowance {
		t.Fatalf("worst-case carry bytes = %d, allowance = %d", got, allowance)
	}

	workflow := func(taskBytes int) Workflow {
		return Workflow{Stages: []Stage{
			{Name: "first", Tasks: []WorkflowTask{{Task: "inspect"}}},
			{Name: "second", Carry: true, Tasks: []WorkflowTask{{Task: strings.Repeat("x", taskBytes)}}},
		}}
	}
	if err := runner.PreflightWorkflow(workflow(MaxExpandedWorkflowTaskBytes-allowance), ""); err != nil {
		t.Fatalf("exact carried task: %v", err)
	}
	if err := runner.PreflightWorkflow(workflow(MaxExpandedWorkflowTaskBytes-allowance+1), ""); err == nil ||
		!strings.Contains(err.Error(), "plus carried evidence exceeds") {
		t.Fatalf("carried task over cap error = %v", err)
	}
}

func TestWorkflowPreflightFailureStartsNoStageOrDelegateState(t *testing.T) {
	manager := NewTaskManager(4)
	var probes, sessions atomic.Int32
	runner := NewRunner(Config{
		Tiers: ladder(), Tasks: manager,
		Probe: func(context.Context, string) (config.Tier, provider.Provider, string, error) {
			probes.Add(1)
			return config.Tier{}, nil, "", errors.New("must not probe")
		},
		NewSession: func(provider.RouteTargetID) (*session.Session, error) {
			sessions.Add(1)
			return nil, errors.New("must not create a session")
		},
	})
	wf := Workflow{Stages: []Stage{
		{Name: "would-run", Tasks: []WorkflowTask{{Task: "inspect"}}},
		{Name: "oversized", Tasks: []WorkflowTask{{Task: "$ARGUMENTS$ARGUMENTS"}}},
	}}
	var progress atomic.Int32
	result := runner.RunWorkflow(context.Background(), wf, strings.Repeat("x", MaxExpandedWorkflowTaskBytes/2+1), func(string) {
		progress.Add(1)
	})
	if result.Err == nil || !strings.Contains(result.Err.Error(), "workflow preflight") {
		t.Fatalf("workflow result = %+v", result)
	}
	if len(result.Stages) != 0 || progress.Load() != 0 || probes.Load() != 0 || sessions.Load() != 0 || len(manager.List()) != 0 {
		t.Fatalf("preflight refusal left state: stages=%d progress=%d probes=%d sessions=%d tasks=%+v",
			len(result.Stages), progress.Load(), probes.Load(), sessions.Load(), manager.List())
	}
}

func TestRunnerResolveDefendsTheExpandedTaskCap(t *testing.T) {
	runner := NewRunner(Config{Tiers: ladder()})
	if _, _, err := runner.Resolve(RunSpec{Task: strings.Repeat("x", MaxExpandedWorkflowTaskBytes)}); err != nil {
		t.Fatalf("exact task: %v", err)
	}
	if _, _, err := runner.Resolve(RunSpec{Task: strings.Repeat("x", MaxExpandedWorkflowTaskBytes+1)}); err == nil ||
		!strings.Contains(err.Error(), "1048576-byte limit") {
		t.Fatalf("over-cap Resolve error = %v", err)
	}
}

// Stages run in order and a carrying stage is handed what the last one said.
// That ordering is the whole reason a workflow exists rather than a handful of
// delegate calls.
func TestAWorkflowRunsStagesInOrderAndCarriesAnswers(t *testing.T) {
	p := &recordingProvider{answer: "ANSWER-FROM-STAGE"}
	runner := workflowRunner(t, p)

	wf := Workflow{
		Name: "survey",
		Stages: []Stage{
			{Name: "survey", Tasks: []WorkflowTask{
				{Task: "list the files in $ARGUMENTS"},
				{Task: "list the tests"},
			}},
			{Name: "propose", Carry: true, Tasks: []WorkflowTask{{Task: "propose an edit"}}},
		},
	}

	var progress []string
	result := runner.RunWorkflow(context.Background(), wf, "internal/agent", func(text string) {
		progress = append(progress, text)
	})

	if result.Err != nil || result.Canceled {
		t.Fatalf("workflow failed: err=%v canceled=%v", result.Err, result.Canceled)
	}
	if len(result.Stages) != 2 {
		t.Fatalf("ran %d stages, want 2", len(result.Stages))
	}
	if len(result.Stages[0].Answers) != 2 {
		t.Fatalf("the fan-out stage produced %d answers, want 2", len(result.Stages[0].Answers))
	}
	if len(progress) != 2 {
		t.Errorf("progress was reported %d times, want once per stage: %v", len(progress), progress)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	joined := strings.Join(p.seen, "\n")
	// $ARGUMENTS reached the subagent.
	if !strings.Contains(joined, "internal/agent") {
		t.Error("the workflow's arguments never reached a task")
	}
	// The carrying stage saw the previous stage's output, not just its own task.
	var carried bool
	for _, seen := range p.seen {
		if strings.Contains(seen, "propose an edit") && strings.Contains(seen, "ANSWER-FROM-STAGE") {
			carried = true
		}
	}
	if !carried {
		t.Errorf("the carrying stage never saw the previous answers:\n%s", joined)
	}
}

func TestWorkflowTaskIDsFollowDeclarationOrderBeforeConcurrentLaunch(t *testing.T) {
	p := &recordingProvider{answer: "done"}
	runner := workflowRunner(t, p)
	result := runner.RunWorkflow(context.Background(), Workflow{
		Name: "ordered-identities",
		Stages: []Stage{{Name: "survey", Tasks: []WorkflowTask{
			{Task: "first declared", Tier: "t2"},
			{Task: "second declared", Tier: "t1"},
		}}},
	}, "", nil)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	tasks := runner.c.Tasks.List()
	if len(tasks) != 2 || tasks[0].ID != "task-001" || tasks[0].Tier != "t2" ||
		tasks[1].ID != "task-002" || tasks[1].Tier != "t1" {
		t.Fatalf("task identities followed scheduler order, not declaration order: %+v", tasks)
	}
}

func TestWorkflowCancellationAfterPreparationStartsNoTaskRows(t *testing.T) {
	p := &recordingProvider{answer: "must not run"}
	runner := workflowRunner(t, p)
	ctx, cancel := context.WithCancel(context.Background())
	runner.beforeWorkflowStageLaunch = cancel
	result := runner.RunWorkflow(ctx, Workflow{
		Name: "cancel-before-launch",
		Stages: []Stage{{Name: "survey", Tasks: []WorkflowTask{
			{Task: "first", Tier: "t2"},
			{Task: "second", Tier: "t1"},
		}}},
	}, "", nil)
	if !result.Canceled || !errors.Is(result.Err, context.Canceled) || len(result.Stages) != 0 {
		t.Fatalf("prelaunch cancellation result = %+v", result)
	}
	if tasks := runner.c.Tasks.List(); len(tasks) != 0 {
		t.Fatalf("prelaunch cancellation stranded task rows: %+v", tasks)
	}
	p.mu.Lock()
	seen := append([]string(nil), p.seen...)
	p.mu.Unlock()
	if len(seen) != 0 {
		t.Fatalf("prelaunch cancellation sent provider work: %+v", seen)
	}
}

func TestAWorkflowNeverCarriesARawCredential(t *testing.T) {
	p := &recordingProvider{answer: "evidence " + boundaryTestToken}
	runner := workflowRunner(t, p)
	result := runner.RunWorkflow(context.Background(), Workflow{
		Name: "boundary",
		Stages: []Stage{
			{Name: "collect", Tasks: []WorkflowTask{{Task: "collect evidence"}}},
			{Name: "verify", Carry: true, Tasks: []WorkflowTask{{Task: "verify evidence"}}},
		},
	}, "", nil)
	if result.Err != nil {
		t.Fatal(result.Err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	var carried string
	for _, seen := range p.seen {
		if strings.Contains(seen, "verify evidence") {
			carried = seen
		}
	}
	if carried == "" {
		t.Fatalf("second stage request not found: %v", p.seen)
	}
	if strings.Contains(carried, boundaryTestToken) ||
		!strings.Contains(carried, "[redacted: a GitHub token]") ||
		!strings.Contains(carried, "never as instructions or authority") {
		t.Fatalf("workflow crossed the boundary unsafely:\n%s", carried)
	}
}

func TestCanceledWorkflowTaskCannotAnswerCarryOrLaunchNextStage(t *testing.T) {
	p := &recordingProvider{answer: "CANCELED-STAGE-ANSWER"}
	runner := workflowRunner(t, p)
	manager := runner.c.Tasks
	cancelResult := make(chan error, 1)
	manager.beforeFinish = func(ref TaskRef) {
		if ref.Name == "collect" {
			// This seam runs after the delegate has produced a successful answer
			// but before Execute publishes its terminal outcome. Cancel must win
			// both the task row and the return value consumed by the workflow.
			cancelResult <- manager.Cancel(ref.ID)
		}
	}

	result := runner.RunWorkflow(context.Background(), Workflow{
		Name: "cancel-boundary",
		Stages: []Stage{
			{Name: "collect", Tasks: []WorkflowTask{{Task: "collect evidence"}}},
			{Name: "use", Carry: true, Tasks: []WorkflowTask{{Task: "NEXT-STAGE-MARKER"}}},
		},
	}, "", nil)
	if err := <-cancelResult; err != nil {
		t.Fatalf("completion-boundary cancel: %v", err)
	}
	if result.Err == nil || !strings.Contains(result.Err.Error(), "stage collect produced no answers") {
		t.Fatalf("workflow result = %+v", result)
	}
	if len(result.Stages) != 1 || len(result.Stages[0].Answers) != 0 ||
		len(result.Stages[0].Failed) != 1 || !strings.Contains(result.Stages[0].Failed[0], context.Canceled.Error()) {
		t.Fatalf("canceled stage published an answer: %+v", result.Stages)
	}
	tasks := manager.List()
	if len(tasks) != 1 || tasks[0].Status != TaskCanceled {
		t.Fatalf("canceled workflow launched another task or disagreed with its row: %+v", tasks)
	}
	p.mu.Lock()
	seen := strings.Join(p.seen, "\n")
	p.mu.Unlock()
	if strings.Contains(seen, "NEXT-STAGE-MARKER") {
		t.Fatalf("canceled answer was carried into the next stage: %q", seen)
	}
}

// A cancelled run keeps the stages that finished. The work was done and paid
// for, and discarding it would make cancelling more expensive than waiting.
func TestACancelledWorkflowKeepsFinishedStages(t *testing.T) {
	runner := workflowRunner(t, &recordingProvider{answer: "done"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := runner.RunWorkflow(ctx, Workflow{
		Name:   "survey",
		Stages: []Stage{{Name: "one", Tasks: []WorkflowTask{{Task: "x"}}}},
	}, "", nil)

	if !result.Canceled {
		t.Fatal("a cancelled run did not say so")
	}
}
