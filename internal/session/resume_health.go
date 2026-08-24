package session

import "github.com/switchboard-code/switchboard/internal/provider"

// ContinuityHealth says whether the latest live continuity capsule is pending,
// delivered, or stale for generic resume. Cleared continuity deliberately
// reads as none: a tombstone carries lineage, not work for a resumed model.
type ContinuityHealth string

const (
	ContinuityNone    ContinuityHealth = ""
	ContinuityPending ContinuityHealth = "pending"
	ContinuityCurrent ContinuityHealth = "current"
	// ContinuityStale means the capsule predates authoritative user input.
	// It remains in the append-only record for audit and fork lineage, but a
	// generic resume must not restore its task state over that later input.
	ContinuityStale ContinuityHealth = "stale"
)

// ResumeHealth is the bounded, read-only part of replay that helps a person
// choose a session. It carries no message text or tool input, and computing it
// never adopts, repairs, or truncates the candidate.
type ResumeHealth struct {
	Messages             int
	Turns                int
	EffectiveTarget      provider.RouteTargetID
	IncompleteAssistants int
	PendingToolRepairs   int
	Continuity           ContinuityHealth
	RetryIntent          RetryIntentStatus
	RecoveredCorruptTail bool
	// CorruptRecord means identity and a valid prefix were readable, but a
	// later complete frame failed integrity validation. The log is listed so it
	// cannot look lost, while writable resume and full-history readers refuse it.
	CorruptRecord bool
	// ReplayLimit means identity and a bounded valid prefix were readable, but
	// fully adopting the log would exceed a cumulative record/byte/message/block
	// limit. Inventory reports it; resume and full-history projections refuse it.
	ReplayLimit bool
}

// ResumeHealthForState derives the same health summary for an already-open
// session. recoveredCorruptTail is supplied by the caller because State is a
// replay result and intentionally does not retain file-recovery mechanics.
func ResumeHealthForState(state State, recoveredCorruptTail bool) ResumeHealth {
	health := ResumeHealth{
		Messages:             len(state.Messages),
		EffectiveTarget:      provider.RouteTargetID(state.Target),
		RecoveredCorruptTail: recoveredCorruptTail,
	}
	if state.RuntimeBinding.Target != "" {
		health.EffectiveTarget = state.RuntimeBinding.Target
	}
	for _, message := range state.Messages {
		if OpensUserTurn(message) {
			health.Turns++
		}
		if message.Role == provider.RoleAssistant && message.Incomplete {
			health.IncompleteAssistants++
		}
	}
	health.PendingToolRepairs = len(interruptedToolCallsAtTail(state.Messages))
	if state.RetryIntent != nil {
		health.RetryIntent = state.RetryIntent.Status
	}
	if state.Continuity != nil && !state.Continuity.Cleared {
		if continuityStaleForResume(state) {
			health.Continuity = ContinuityStale
		} else if state.Continuity.ID != "" && state.Continuity.ID == state.ContinuityRef {
			health.Continuity = ContinuityCurrent
		} else {
			health.Continuity = ContinuityPending
		}
	}
	return health
}

// continuityStaleForResume applies authority rather than role alone. Ordinary
// user openings and durable user steers can cancel or replace prior work;
// harness injections and synthetic continuation openings cannot. Legacy user
// openings without an Authored projection remain authoritative: treating an
// old record cautiously is safer than reviving work after its cancellation.
func continuityStaleForResume(state State) bool {
	if state.Continuity == nil || state.Continuity.Cleared {
		return false
	}
	basis := state.Continuity.BasisMessages
	if basis < 0 || basis > len(state.Messages) {
		return true
	}
	for _, message := range state.Messages[basis:] {
		if authoritativeUserInput(message) {
			return true
		}
	}
	return false
}

func authoritativeUserInput(message provider.Message) bool {
	if message.Role != provider.RoleUser {
		return false
	}
	if message.UserSteer {
		return true
	}
	return OpensUserTurn(message)
}
