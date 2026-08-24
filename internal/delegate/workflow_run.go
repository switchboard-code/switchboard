package delegate

// Executing a workflow.
//
// Stages run in order and their tasks run together, which is the whole
// contract: a stage is the barrier. The runner owns the goroutines and joins
// every stage before starting the next, so nothing outlives the call and
// TaskManager's promise that it starts no goroutines of its own stays true.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// MaxExpandedWorkflowTaskBytes is the exact post-substitution and post-carry
// ceiling for one delegated task. Definition files have a smaller source cap,
// but a repeated $ARGUMENTS token can otherwise multiply one bounded paste
// into an unbounded allocation before routing or context admission sees it.
const MaxExpandedWorkflowTaskBytes = 1 << 20

// StageResult is what one stage produced, in task order.
type StageResult struct {
	Stage   string
	Answers []string
	Failed  []string
}

// WorkflowResult is the whole run, including a run that stopped early. A
// cancelled workflow returns the stages that finished rather than nothing:
// the work was done and paid for, and discarding it would make ctrl+c more
// expensive than waiting.
type WorkflowResult struct {
	Stages   []StageResult
	Canceled bool
	Err      error
}

// ExpandWorkflowArguments returns a deep task copy with every user-supplied
// argument substitution resolved. Keeping this projection explicit lets an
// attended or unattended surface apply its credential policy to the exact
// prompts that would cross the delegated-model boundary, including a token
// formed by a static template prefix plus an argument suffix.
func ExpandWorkflowArguments(wf Workflow, arguments string) (Workflow, error) {
	if wf.argumentsExpanded {
		return Workflow{}, fmt.Errorf("workflow arguments were already expanded")
	}
	out := wf
	out.argumentsExpanded = true
	out.Stages = make([]Stage, len(wf.Stages))
	for i, stage := range wf.Stages {
		out.Stages[i] = stage
		out.Stages[i].Tasks = make([]WorkflowTask, len(stage.Tasks))
		copy(out.Stages[i].Tasks, stage.Tasks)
		for j := range out.Stages[i].Tasks {
			expanded, err := expandArguments(out.Stages[i].Tasks[j].Task, arguments)
			if err != nil {
				return Workflow{}, fmt.Errorf("stage %s task %d: %w", stage.Name, j+1, err)
			}
			out.Stages[i].Tasks[j].Task = expanded
		}
	}
	return out, nil
}

// PreflightWorkflow resolves every task exactly as RunWorkflow will before a
// headless surface publishes its primary session. Resolution is pure: it
// expands arguments and checks the task, named agent, tier, and runner
// constraints without reserving a task, probing a provider, or opening a log.
func (r *Runner) PreflightWorkflow(wf Workflow, arguments string) error {
	expanded, err := ExpandWorkflowArguments(wf, arguments)
	if err != nil {
		return err
	}
	return r.PreflightExpandedWorkflow(expanded)
}

// PreflightExpandedWorkflow validates the exact task bytes returned by
// ExpandWorkflowArguments without interpreting template syntax again.
func (r *Runner) PreflightExpandedWorkflow(wf Workflow) error {
	if !wf.argumentsExpanded {
		return fmt.Errorf("workflow arguments have not been expanded")
	}
	return r.preflightExpandedWorkflow(wf)
}

func (r *Runner) preflightExpandedWorkflow(wf Workflow) error {
	for stageIndex, stage := range wf.Stages {
		if stage.Carry && stageIndex == 0 {
			return fmt.Errorf("stage %s carries, but nothing ran before it", stage.Name)
		}
		for i, task := range stage.Tasks {
			text := task.Task
			if stage.Carry {
				previousTasks := len(wf.Stages[stageIndex-1].Tasks)
				allowance, ok := maxWorkflowCarryEnvelopeBytes(previousTasks)
				if !ok || len(text) > MaxExpandedWorkflowTaskBytes-allowance {
					return fmt.Errorf("stage %s task %d: expanded task plus carried evidence exceeds the %d-byte limit",
						stage.Name, i+1, MaxExpandedWorkflowTaskBytes)
				}
				// Runtime carry is guaranteed nonempty when this stage can run. Use
				// only its fixed frame here; the worst-case answer bytes were reserved
				// above and no model output is invented during pure preflight.
				text = workflowCarryHead + workflowCarryTail + text
			}
			_, _, err := r.Resolve(RunSpec{
				Task: text, Tier: task.Tier, AgentName: task.Agent, Name: stage.Name,
			})
			if err != nil {
				return fmt.Errorf("stage %s task %d: %w", stage.Name, i+1, err)
			}
		}
	}
	return nil
}

// RunWorkflow executes every stage in order. progress is called as tasks
// start and finish so a surface can say what is happening; it may be nil.
func (r *Runner) RunWorkflow(ctx context.Context, wf Workflow, arguments string, progress func(string)) WorkflowResult {
	expanded, err := ExpandWorkflowArguments(wf, arguments)
	if err != nil {
		return WorkflowResult{Err: fmt.Errorf("workflow preflight: %w", err)}
	}
	return r.RunExpandedWorkflow(ctx, expanded, progress)
}

// RunExpandedWorkflow executes the exact task bytes returned by
// ExpandWorkflowArguments. It rechecks pure preflight but never substitutes
// template markers a second time.
func (r *Runner) RunExpandedWorkflow(ctx context.Context, wf Workflow, progress func(string)) WorkflowResult {
	var out WorkflowResult
	if !wf.argumentsExpanded {
		out.Err = fmt.Errorf("workflow preflight: workflow arguments have not been expanded")
		return out
	}
	if err := r.preflightExpandedWorkflow(wf); err != nil {
		out.Err = fmt.Errorf("workflow preflight: %w", err)
		return out
	}
	say := func(text string) {
		if progress != nil {
			progress(text)
		}
	}

	var previous []string
	for _, stage := range wf.Stages {
		if err := ctx.Err(); err != nil {
			out.Canceled = true
			out.Err = err
			return out
		}
		say(fmt.Sprintf("stage %s: %d task(s)", stage.Name, len(stage.Tasks)))

		answers := make([]string, len(stage.Tasks))
		failures := make([]string, len(stage.Tasks))
		type preparedTask struct {
			spec  RunSpec
			named *Agent
			ref   TaskRef
		}
		prepared := make([]preparedTask, len(stage.Tasks))
		for i, task := range stage.Tasks {
			text := task.Task
			if stage.Carry {
				text = Carry(previous, text)
			}
			spec, named, err := r.Resolve(RunSpec{
				Task: text, Tier: task.Tier, AgentName: task.Agent, Name: stage.Name,
			})
			if err != nil {
				failures[i] = err.Error()
				continue
			}
			// Identity is declaration ordered even though execution is concurrent.
			// /tasks steer and cancel refer to these IDs, so scheduler order must
			// never decide which source task a visible row controls.
			prepared[i] = preparedTask{spec: spec, named: named, ref: r.Reserve(spec)}
		}
		if r.beforeWorkflowStageLaunch != nil {
			r.beforeWorkflowStageLaunch()
		}
		if err := ctx.Err(); err != nil {
			out.Canceled = true
			out.Err = err
			return out
		}

		var wg sync.WaitGroup
		for i, task := range prepared {
			if failures[i] != "" {
				continue
			}
			wg.Add(1)
			go func(i int, task preparedTask) {
				defer wg.Done()
				res, err := r.Run(ctx, task.spec, task.named, task.ref)
				switch {
				case err != nil:
					failures[i] = err.Error()
				case res.IsError:
					failures[i] = res.Content
				default:
					answers[i] = res.Content
				}
			}(i, task)
		}
		wg.Wait()

		result := StageResult{Stage: stage.Name}
		for i := range stage.Tasks {
			if failures[i] != "" {
				result.Failed = append(result.Failed, failures[i])
				continue
			}
			result.Answers = append(result.Answers, answers[i])
		}
		out.Stages = append(out.Stages, result)

		if err := ctx.Err(); err != nil {
			out.Canceled = true
			out.Err = err
			return out
		}
		// A stage that produced nothing stops the run. Carrying an empty set
		// into the next stage would run it against instructions that promise
		// results and deliver none, which spends a rung to produce confusion.
		if len(result.Answers) == 0 {
			out.Err = fmt.Errorf("stage %s produced no answers", stage.Name)
			return out
		}
		previous = result.Answers
	}
	return out
}

// expandArguments performs the custom-command substitutions in one pass and
// refuses before writing a component that would cross the exact task cap. In
// particular it never materializes the repeated intermediate strings that a
// chain of ReplaceAll calls would create.
func expandArguments(task, arguments string) (string, error) {
	var out strings.Builder
	grow := len(task)
	if grow > MaxExpandedWorkflowTaskBytes {
		grow = MaxExpandedWorkflowTaskBytes
	}
	out.Grow(grow)

	var fields [9]string
	fieldsReady := false
	appendPart := func(part string) error {
		if len(part) > MaxExpandedWorkflowTaskBytes-out.Len() {
			return fmt.Errorf("expanded workflow task exceeds the %d-byte limit", MaxExpandedWorkflowTaskBytes)
		}
		out.WriteString(part)
		return nil
	}

	for at := 0; at < len(task); {
		next := strings.IndexByte(task[at:], '$')
		if next < 0 {
			if err := appendPart(task[at:]); err != nil {
				return "", err
			}
			break
		}
		next += at
		if err := appendPart(task[at:next]); err != nil {
			return "", err
		}
		switch {
		case strings.HasPrefix(task[next:], "$ARGUMENTS"):
			if err := appendPart(arguments); err != nil {
				return "", err
			}
			at = next + len("$ARGUMENTS")
		case next+1 < len(task) && task[next+1] >= '1' && task[next+1] <= '9':
			if !fieldsReady {
				fields = firstWorkflowArgumentFields(arguments)
				fieldsReady = true
			}
			if err := appendPart(fields[int(task[next+1]-'1')]); err != nil {
				return "", err
			}
			at = next + 2
		default:
			if err := appendPart("$"); err != nil {
				return "", err
			}
			at = next + 1
		}
	}
	return out.String(), nil
}

// firstWorkflowArgumentFields is strings.Fields limited to the only nine
// fields the template language can address. Bounding the scan's result avoids
// an allocation proportional to a large argument that uses only $1.
func firstWorkflowArgumentFields(arguments string) (fields [9]string) {
	count := 0
	for at := 0; at < len(arguments) && count < len(fields); {
		for at < len(arguments) {
			r, size := utf8.DecodeRuneInString(arguments[at:])
			if !unicode.IsSpace(r) {
				break
			}
			at += size
		}
		if at == len(arguments) {
			break
		}
		start := at
		for at < len(arguments) {
			r, size := utf8.DecodeRuneInString(arguments[at:])
			if unicode.IsSpace(r) {
				break
			}
			at += size
		}
		fields[count] = arguments[start:at]
		count++
	}
	return fields
}
