package main

// The boundary is announced before it arrives.
//
// Auto-compaction fires at turn end when the last request crossed its
// threshold, and the model finds out by waking up in a fresh context holding
// whatever the capsule carried. What it carried is what it thought to record
// while it still had everything, and nothing ever told it to. So the fields
// the capsule has always had — the objective, the next action, what would mean
// the job is done — went across empty, and a continuing model inherited a task
// list whose point had been left behind.
//
// This says so, once, while there is still room to answer: the window is
// filling, write down what has to survive. It rides the seam /watch and the
// drift notice use, and says it once per session rather than at every round,
// because a warning repeated at every boundary is one the model stops reading.

import (
	"fmt"

	"github.com/switchboard-code/switchboard/internal/provider"
)

// pressureAtPercent is how full the window gets before the warning. It sits
// below the compaction threshold on purpose: a warning that arrived at the
// same occupancy as the boundary would be advice with no turn left to take it.
const pressureAtPercent = 70

func (a *tuiApp) publishOccupancy(tokens, window int) {
	a.pressureMu.Lock()
	defer a.pressureMu.Unlock()
	a.pressureTokens, a.pressureWindow = tokens, window
}

// resetPressureSession drops the occupancy and one-shot warning owned by the
// session being left. Every adopted log has its own request history, including
// a compacted log: carrying the old token count can warn immediately in a
// fresh context, while carrying warned can suppress this session's warning.
func (a *tuiApp) resetPressureSession() {
	a.pressureMu.Lock()
	defer a.pressureMu.Unlock()
	a.pressureTokens = 0
	a.pressureWindow = 0
	a.pressureWarned = false
}

func (a *tuiApp) pressureRound() []provider.Message {
	a.pressureMu.Lock()
	tokens, window, warned := a.pressureTokens, a.pressureWindow, a.pressureWarned
	a.pressureMu.Unlock()

	if warned || window <= 0 || tokens <= 0 || !a.config.CompactAuto {
		return nil
	}
	at := a.config.CompactAtPercent
	if at == 0 {
		at = 85
	}
	// Below the warning line, or already past the point where compaction is
	// about to happen anyway and the advice would arrive too late to act on.
	if tokens < window*pressureAtPercent/100 {
		return nil
	}

	a.pressureMu.Lock()
	a.pressureWarned = true
	a.pressureMu.Unlock()

	return []provider.Message{provider.UserText(fmt.Sprintf(
		"This context is %d%% full and will be compacted automatically at %d%%. "+
			"What crosses that boundary is the task list and what you have told todo about "+
			"the job: its objective, the next action, and what would mean it is finished. "+
			"Everything else you are holding in this context goes into a summary. "+
			"If any of those three is unset or stale, set it now with todo, while you still "+
			"have the whole picture.", tokens*100/window, at))}
}
