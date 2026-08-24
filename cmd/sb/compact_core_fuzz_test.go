package main

import (
	"reflect"
	"strings"
	"testing"
)

func FuzzParseCompactHandoff(f *testing.F) {
	valid := testCompactHandoff("Continue the verified objective.")
	f.Add(valid)
	f.Add(strings.ReplaceAll(valid, "\n", "\r\n"))
	f.Add(strings.Replace(valid, "## Workspace", "## Workspace ##", 1))
	f.Add(strings.Replace(valid, "Relevant files remain in the workspace.", "```md\n## Objective\nsource data only\n```", 1))
	f.Add("## Objective\nN/A")
	f.Add("`````md\n## Objective\nnot structure\n`````")
	f.Add("")

	f.Fuzz(func(t *testing.T, input string) {
		// Provider collection applies this same bound before parsing. Keeping the
		// fuzz target at the production envelope avoids teaching the fuzzer that
		// giant allocations are useful coverage.
		if len(input) > compactMaxSummaryBytes {
			return
		}
		handoff, err := parseCompactHandoff(input)
		if err != nil {
			return
		}
		if err := validateCompactHandoff(input); err != nil {
			t.Fatalf("parse accepted input that validation rejected: %v", err)
		}
		if handoff.Objective != handoff.Sections[0] || !compactObjectiveSubstantive(handoff.Objective) {
			t.Fatalf("accepted handoff has an invalid objective: %+v", handoff)
		}
		for i, section := range handoff.Sections {
			if section != strings.TrimSpace(section) {
				t.Fatalf("accepted section %q was not normalized", compactHandoffHeadings[i])
			}
		}
		for name, value := range map[string]string{
			"Done": handoff.Frontier.Done, "In progress": handoff.Frontier.InProgress,
			"Next": handoff.Frontier.Next, "Blocked/pending": handoff.Frontier.BlockedPending,
		} {
			if strings.TrimSpace(value) == "" {
				t.Fatalf("accepted handoff has an empty %s frontier", name)
			}
		}

		// Accepted output is a semantic record, not merely a one-shot parse.
		// Rendering its normalized sections back under the required headings must
		// produce the same record and must remain valid.
		var canonical strings.Builder
		for i, heading := range compactHandoffHeadings {
			if i > 0 {
				canonical.WriteString("\n\n")
			}
			canonical.WriteString("## ")
			canonical.WriteString(heading)
			canonical.WriteByte('\n')
			canonical.WriteString(handoff.Sections[i])
		}
		roundTrip, err := parseCompactHandoff(canonical.String())
		if err != nil {
			t.Fatalf("normalized accepted handoff did not round-trip: %v\n%s", err, canonical.String())
		}
		if !reflect.DeepEqual(roundTrip, handoff) {
			t.Fatalf("normalized handoff changed on round-trip:\nfirst:  %+v\nsecond: %+v", handoff, roundTrip)
		}
	})
}
