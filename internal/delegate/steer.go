package delegate

// Steering a running subagent.
//
// A delegated task used to be a sealed envelope: instructions in, final answer
// out, and nothing in between. That is the right default — a subagent with a
// fresh context is cheap precisely because nobody is managing it — but it is
// the wrong only option. A task that has misread its instructions is visible
// to whoever is watching long before it finishes, and until now the only
// answers were to let it run or to cancel it.
//
// The channel is the loop's own injection seam, the one the advisor's advice
// already travels: a user-role message appended at a round boundary, where
// every wire format this program speaks accepts one. Nothing is interrupted
// mid-call and no tool result is displaced. A steer waits for the round the
// subagent is in to finish, which is also what makes it safe.

import (
	"fmt"
	"strings"

	"github.com/switchboard-code/switchboard/internal/provider"
)

// steerPrefix labels an injected message so the subagent can tell guidance
// that arrived mid-task from the instructions it started with. The two are
// not the same authority: the task is its charter, a steer is a correction.
const steerPrefix = "[steer]"

// maxPendingSteers bounds what one task can accumulate while it works. A
// person typing faster than a subagent finishes a round is a person who has
// changed their mind, and the last few messages are what they meant.
const maxPendingSteers = 8

// Steer queues guidance for a running task. It reports whether the task was
// in a state that can still receive it: a finished task is not steerable, and
// saying so beats accepting a message nothing will ever read.
func (m *TaskManager) Steer(id, message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		return fmt.Errorf("a steer needs something to say")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.tasks[id]
	if state == nil {
		return fmt.Errorf("no task %s", id)
	}
	if state.Status != TaskQueued && state.Status != TaskRunning {
		return fmt.Errorf("task %s already %s; nothing is listening", id, state.Status)
	}
	state.pendingSteers = append(state.pendingSteers, message)
	if len(state.pendingSteers) > maxPendingSteers {
		state.pendingSteers = state.pendingSteers[len(state.pendingSteers)-maxPendingSteers:]
	}
	state.SteersSent++
	return nil
}

// injectSteering is the loop's Inject hook for one task. The loop calls it at
// each round boundary after the first, and anything queued since the last
// round becomes one user-role message per steer.
func (h *TaskHandle) injectSteering() []provider.Message {
	if h == nil || h.manager == nil {
		return nil
	}
	h.manager.mu.Lock()
	state := h.manager.tasks[h.id]
	if state == nil || len(state.pendingSteers) == 0 {
		h.manager.mu.Unlock()
		return nil
	}
	pending := state.pendingSteers
	state.pendingSteers = nil
	state.SteersApplied += len(pending)
	h.manager.mu.Unlock()

	out := make([]provider.Message, 0, len(pending))
	for _, message := range pending {
		// Injected, like everything else that rides this seam. A user-role
		// message that does not say so reads as the opening of a turn to
		// session.OpensTurn, which is what /fork, /retry, and blame cut on:
		// an unmarked steer puts a phantom turn boundary in the middle of the
		// subagent's errand.
		steer := provider.UserText(steerPrefix + " " + redactCrossAgent(message))
		steer.Injected = true
		steer.UserSteer = true
		out = append(out, steer)
	}
	return out
}

// RecordActivity keeps a short tail of what the task is doing, so a caller can
// answer "what is it up to" without reading a whole sub-session. Bounded on
// purpose: this is a status line, not a transcript, and the sub-session on
// disk remains the record.
func (h *TaskHandle) RecordActivity(what string) {
	if h == nil || h.manager == nil {
		return
	}
	what = strings.TrimSpace(redactCrossAgent(what))
	if what == "" {
		return
	}
	if len(what) > maxActivityBytes {
		what = what[:maxActivityBytes] + "…"
	}
	h.manager.mu.Lock()
	defer h.manager.mu.Unlock()
	state := h.manager.tasks[h.id]
	if state == nil {
		return
	}
	state.Activity = append(state.Activity, what)
	if len(state.Activity) > maxActivityLines {
		state.Activity = state.Activity[len(state.Activity)-maxActivityLines:]
	}
}

const (
	maxActivityLines = 6
	maxActivityBytes = 200
)

// activityReport is what the subagent did, for the delegating agent to read
// when the final answer is missing or the run stopped early.
//
// Only then. A subagent that answered has answered, and its preamble promised
// that the final message is what survives; appending a log to a good answer
// would spend the delegator's context contradicting that. A failure is the
// case where the answer is not enough, because "it failed" and "it failed
// after three greps that all came back empty" send the delegator to different
// next moves.
func (h *TaskHandle) activityReport() string {
	if h == nil || h.manager == nil {
		return ""
	}
	h.manager.mu.Lock()
	defer h.manager.mu.Unlock()
	state := h.manager.tasks[h.id]
	if state == nil || len(state.Activity) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("what it did, most recent last:\n")
	for _, what := range state.Activity {
		b.WriteString("  " + what + "\n")
	}
	return b.String()
}
