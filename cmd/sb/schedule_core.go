package main

// The UI-independent half of the schedule commands, shared by the TUI and the
// REPL: argument splitting and the listing's one-line rendering. The store
// itself is internal/schedule; what lives here is only how a surface talks
// about it.

import (
	"fmt"
	"strings"
	"time"

	"github.com/switchboard-code/switchboard/internal/schedule"
)

// splitScheduleSpec separates the leading time spec (an interval or a clock
// reading) from the prompt, which keeps the rest of the line whole: a prompt
// that arrives with its first word eaten is worse than one that does not
// arrive.
func splitScheduleSpec(args string) (spec, prompt string, ok bool) {
	spec, prompt, cut := strings.Cut(strings.TrimSpace(args), " ")
	prompt = strings.TrimSpace(prompt)
	if !cut || prompt == "" {
		return "", "", false
	}
	return spec, prompt, true
}

// scheduleLine is one /schedule row: id, kind, the next fire both relative
// and on the wall clock, and the prompt truncated to what a row can hold.
// Both clocks ride every row because "in 12m" answers when and "14:32" is
// what the user checks against their watch.
func scheduleLine(e schedule.Entry, now time.Time) string {
	kind := "at " + e.At
	if e.Recurring() {
		kind = "every " + e.Every.String()
	}
	prompt := workspaceSanitize(redactCredentialTextBeforeTruncate(e.Prompt, 40))
	return fmt.Sprintf("%-4s %-13s %-22s %s",
		e.ID, kind, relativeFire(now, e.NextFire)+" ("+wallClock(now, e.NextFire)+")", prompt)
}

// relativeFire renders the gap to the next fire in whole minutes. A gap under
// a minute says so rather than printing a zero, and an entry whose moment has
// passed reads as due — the poller takes those, so listing one means the
// clock has not ticked yet.
func relativeFire(now, t time.Time) string {
	d := t.Sub(now)
	if d <= 0 {
		return "due now"
	}
	if d < time.Minute {
		return "in <1m"
	}
	return "in " + d.Round(time.Minute).String()
}

// wallClock renders the fire's clock reading, naming the day only when it is
// not today: "14:30" already reads as today, and anything else without its
// day is a riddle.
func wallClock(now, t time.Time) string {
	local := t.Local()
	if here := now.Local(); local.Year() == here.Year() && local.YearDay() == here.YearDay() {
		return local.Format("15:04")
	}
	return local.Format("Jan 2 15:04")
}
