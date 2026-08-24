package main

// The one line a session listing shows for a log, shared by the /resume
// picker and `sb -sessions`: an id names a file, the opening names a
// conversation.

import (
	"strconv"
	"strings"

	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/terminaltext"
)

const legacyTurnPromptUnavailable = "(authored wording unavailable for this legacy turn)"

// recordedTurnPrompt renders provenance before content. Unknown legacy
// Content is never a display fallback because it may contain expanded files,
// shell output, or harness injections rather than words the user typed.
func recordedTurnPrompt(prompt string, authoredKnown, synthetic bool, limit int) string {
	switch {
	case synthetic:
		return "(Switchboard automatic continuation)"
	case !authoredKnown:
		return legacyTurnPromptUnavailable
	case strings.TrimSpace(prompt) == "":
		return "(authored turn carried no text)"
	default:
		return strconv.Quote(redactCredentialTextBeforeTruncate(prompt, limit))
	}
}

// openingLabel is the first words the user sent, collapsed to one line and
// cut to listing width. A compacted session's first user message is the seed
// — a preamble every compacted session shares — so the label skips to the
// summary's own first words; auto-compaction means the users with the most
// sessions to tell apart are exactly the ones whose logs open this way.
// Empty means the caller keeps whatever it was already showing: a log with
// no user turn yet, or one that cannot be read, is not worth a label that
// lies about it.
func openingLabel(path string) string {
	summary, err := session.ReadOpeningSummary(path)
	if err != nil {
		return ""
	}
	opening := safeOpeningText(summary)
	if opening == "" {
		return ""
	}
	// Scan the complete opening before the listing cap, then keep every
	// terminal control and embedded newline visible rather than executable.
	opening = redactCredentialText(opening)
	opening = terminaltext.Escape(strings.Join(strings.Fields(opening), " "))
	return truncate(opening, 56)
}

func safeOpeningText(opening session.OpeningSummary) string {
	if opening.AuthoredKnown {
		return opening.Text
	}
	if !opening.Synthetic || !strings.HasPrefix(opening.Text, compactSeedHead) {
		return ""
	}
	_, handoff, ok := strings.Cut(opening.Text, "\n\n")
	if !ok {
		return ""
	}
	return compactOpeningLabel(handoff)
}

// New compact handoffs are structured, but a session list should name the
// work rather than render the common "## Objective" heading. Older free-form
// summaries keep their original first words.
func compactOpeningLabel(summary string) string {
	const objectiveHeading = "## Objective"
	summary = strings.TrimSpace(summary)
	if !strings.HasPrefix(summary, objectiveHeading) {
		return summary
	}
	objective := strings.TrimSpace(strings.TrimPrefix(summary, objectiveHeading))
	if end := strings.Index(objective, "\n## "); end >= 0 {
		objective = strings.TrimSpace(objective[:end])
	}
	if objective == "" {
		return summary
	}
	return objective
}
