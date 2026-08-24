package main

// The REPL's half of /every, /at, and /schedule. What the TUI's poller checks
// on a clock, a line reader can check only at its seams: before each prompt
// is read, which is also after every turn and every command. An entry that
// comes due while the user sits at the prompt waits for the next line —
// there is no event loop to wake, and inventing one would interleave a fired
// turn's output with half-typed input.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/schedule"
)

func (r *repl) scheduleEvery(args string) {
	spec, prompt, ok := splitScheduleSpec(args)
	if !ok {
		r.out.Notice("warn", "usage: /every <interval> <prompt>, e.g. /every 30m run the tests")
		return
	}
	d, err := time.ParseDuration(spec)
	if err != nil {
		r.out.Notice("warn", "/every takes an interval first, like 30m or 2h")
		return
	}
	if d < schedule.MinEvery {
		r.out.Notice("warn", "the shortest interval is "+schedule.MinEvery.String()+"; anything tighter is a loop, not a reminder")
		return
	}
	r.addSchedule(schedule.Entry{Every: d, Prompt: prompt})
}

func (r *repl) scheduleAt(args string) {
	spec, prompt, ok := splitScheduleSpec(args)
	if !ok {
		r.out.Notice("warn", "usage: /at <HH:MM> <prompt>, e.g. /at 14:30 check the deploy")
		return
	}
	if _, err := time.Parse("15:04", spec); err != nil {
		r.out.Notice("warn", "/at takes a 24-hour local clock time first, like 14:30")
		return
	}
	r.addSchedule(schedule.Entry{At: spec, Prompt: prompt})
}

// addSchedule refuses a key-shaped prompt outright, naming the kinds and
// never the values. An armed entry is the secret kept at rest in the ledger,
// not only sent once, so the REPL's inline redact/send/drop question — built
// for a prompt about to leave — does not fit what is being asked for here.
// Rephrasing without the key is the offered path.
func (r *repl) addSchedule(e schedule.Entry) {
	if r.schedules == nil {
		r.out.Notice("warn", "schedules are unavailable"+r.schedulesErr)
		return
	}
	if leaks := credential.ScanPrompt(e.Prompt); len(leaks) > 0 {
		kinds := make([]string, len(leaks))
		for i, l := range leaks {
			kinds[i] = l.String()
		}
		r.out.Notice("warn", "not scheduled: the prompt contains "+strings.Join(kinds, ", "))
		return
	}
	added, err := r.schedules.Add(e)
	if err != nil {
		r.out.Notice("warn", err.Error())
		return
	}
	r.out.line("  armed " + cliText(added.ID) + ": " + cliText(scheduleLine(added, time.Now())))
}

func (r *repl) scheduleCommand(args string) {
	if r.schedules == nil {
		r.out.Notice("warn", "schedules are unavailable"+r.schedulesErr)
		return
	}
	fields := strings.Fields(args)
	if len(fields) > 0 {
		if len(fields) != 2 || fields[0] != "cancel" {
			r.out.Notice("warn", "usage: /schedule [cancel <id>]")
			return
		}
		if r.schedules.Cancel(fields[1]) {
			r.out.line("  cancelled " + cliText(fields[1]) + "; the rest of the schedule is untouched")
		} else {
			r.out.Notice("warn", "no scheduled entry "+fields[1]+"; /schedule lists them")
		}
		return
	}
	entries := r.schedules.List()
	if len(entries) == 0 {
		r.out.line("  nothing scheduled; /every <interval> <prompt> recurs, /at <HH:MM> <prompt> fires once")
		return
	}
	now := time.Now()
	for _, e := range entries {
		r.out.line("  " + cliText(scheduleLine(e, now)))
	}
}

// fireDueSchedules runs the soonest due entry as an ordinary turn through the
// same path a typed prompt takes — mention expansion and the outbound scan
// included. One per seam: a startup catch-up fires the rest at the next
// prompts rather than cascading turns the user has not seen begin. The prompt
// echoes with the prompt glyph and its [scheduled sN] lead, because a turn
// whose origin never renders is a turn the user cannot explain.
func (r *repl) fireDueSchedules(ctx context.Context) {
	if r.schedules == nil {
		return
	}
	due, err := r.schedules.TakeDue(time.Now(), 1)
	if err != nil {
		r.out.Notice("warn", "the schedule ledger did not save; a fired entry may repeat at next launch: "+err.Error())
	}
	for _, e := range due {
		prompt := "[scheduled " + e.ID + "] " + e.Prompt
		r.out.line(r.out.style(bold, "› ") + workspaceSanitize(prompt))
		expanded, authored, images, ok := r.prepareInteractivePromptAuthored(prompt)
		if !ok {
			continue
		}
		err := r.turnPreparedAuthored(ctx, expanded, authored, images, false)
		switch {
		case errors.Is(err, context.Canceled):
			r.out.Notice("warn", "turn cancelled; the session is intact and can continue")
		case errors.Is(err, agent.ErrRoundLimit):
		case err != nil:
			r.out.Notice("error", err.Error())
		}
	}
}
