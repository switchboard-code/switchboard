package main

import (
	"fmt"
	"strings"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

// resumeHealthChips is deliberately plain text: it is shared by the dimmed
// picker metadata and /session's transcript output, and remains searchable in
// either surface. The target stays visible because two serving surfaces for
// the same model are different resumable bindings.
func resumeHealthChips(health session.ResumeHealth, includeTarget bool) string {
	return formatResumeHealthChips(health, includeTarget, false)
}

// The picker gives metadata half a row on ordinary terminals. Compact counts
// leave enough of that row for the exact serving surface instead of spending
// it on words the adjacent numbers already explain.
func resumePickerHealthChips(health session.ResumeHealth) string {
	return formatResumeHealthChips(health, true, true)
}

func formatResumeHealthChips(health session.ResumeHealth, includeTarget, compactCounts bool) string {
	chips := make([]string, 0, 7)
	if compactCounts {
		chips = append(chips, fmt.Sprintf("%dt", health.Turns), fmt.Sprintf("%dm", health.Messages))
	} else {
		chips = append(chips,
			countLabel(health.Turns, "turn", "turns"),
			countLabel(health.Messages, "msg", "msgs"))
	}
	hasState := false
	if health.IncompleteAssistants > 0 {
		chips = append(chips, fmt.Sprintf("interrupted ×%d", health.IncompleteAssistants))
		hasState = true
	}
	if health.PendingToolRepairs > 0 {
		chips = append(chips, fmt.Sprintf("repair pending ×%d", health.PendingToolRepairs))
		hasState = true
	}
	if health.Continuity != session.ContinuityNone {
		chips = append(chips, "continuity "+string(health.Continuity))
		hasState = true
	}
	if health.RetryIntent != "" {
		chips = append(chips, "retry "+string(health.RetryIntent)+" · recovery decision required")
		hasState = true
	}
	if health.RecoveredCorruptTail {
		chips = append(chips, "tail recovery")
		hasState = true
	}
	if health.CorruptRecord {
		chips = append(chips, "corrupt · resume blocked")
		hasState = true
	}
	if health.ReplayLimit {
		chips = append(chips, "replay limit · resume blocked")
		hasState = true
	}
	if !hasState {
		chips = append(chips, "ready")
	}
	if includeTarget && health.EffectiveTarget != "" {
		chips = append(chips, provider.DisplayRouteTargetID(health.EffectiveTarget))
	}
	return strings.Join(chips, " · ")
}

func countLabel(n int, singular, plural string) string {
	label := plural
	if n == 1 {
		label = singular
	}
	return fmt.Sprintf("%d %s", n, label)
}
