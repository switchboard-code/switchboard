package main

// /mistakes and `sb mistakes`: the failures that keep coming back. One
// session meeting a failure is debugging; a second session meeting the same
// signature is a standing problem the workspace has not kept the lesson
// from. The ledger replays what the sessions recorded — failing runs of
// test-shaped commands, the escalation detector's own gate, reduced to the
// same signatures it compares live — so the ledger and the routing record
// can never disagree about what a failure was. Read-only over the logs, the
// open one included, the same posture as sb cost.

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/provider"
	route "github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/session"
)

const (
	mistakesMaxEntries  = 10
	mistakesMaxSessions = 4
)

// mistake is one failure signature's history across the workspace's record.
type mistake struct {
	line    string // the failing line as last printed, the human-readable face
	command string // the run that last produced it
	count   int
	first   time.Time
	last    time.Time

	// sessions is in gathering order, oldest log first; the render
	// reverses it, because the session worth reopening is the fresh one.
	sessions     []string
	seenSessions map[string]bool
}

// gatherMistakes replays every session the workspace recorded and sums
// failure signatures across them. An occurrence is one failing run of a
// test-shaped command — the first failing line's signature, one run one
// observation, exactly what the live detector counts — and a fork's copied
// prefix is deduplicated on the record's own timestamp, the Usage.At
// mechanism: a copied failure is the same observation carried over, never a
// second meeting.
func gatherMistakes(store *session.Store, workspace string) ([]*mistake, int, error) {
	infos, err := store.List(workspace)
	if err != nil {
		return nil, 0, err
	}
	// Oldest log first, so a deduplicated occurrence is attributed to the
	// lineage's origin deterministically: a fork's copy must not claim the
	// meeting its source made, and which log a shared record counts under
	// must not depend on which file was touched last.
	infos = slices.Clone(infos)
	slices.Reverse(infos)

	bysig := map[string]*mistake{}
	seenOccurrence := map[string]bool{}
	for _, info := range infos {
		tl, err := session.ReadTimeline(info.Path)
		if err != nil {
			continue
		}
		commands := map[string]string{}
		for _, rec := range tl {
			if rec.Message == nil {
				continue
			}
			for _, block := range rec.Message.Content {
				switch b := block.(type) {
				case provider.ToolUse:
					if b.Name != "exec" {
						continue
					}
					var in struct {
						Command []string `json:"command"`
					}
					if json.Unmarshal(b.Input, &in) == nil {
						commands[b.ID] = strings.Join(in.Command, " ")
					}
				case provider.ToolResult:
					if !b.IsError || b.Name != "exec" {
						continue
					}
					cmd, ok := commands[b.ToolUseID]
					if !ok || !route.LooksLikeTests(cmd) {
						continue
					}
					failures := route.ExtractFailures(b.Content)
					if len(failures) == 0 {
						continue
					}
					f := failures[0]
					occ := fmt.Sprintf("%d/%s", rec.At.UnixNano(), f.Signature)
					if seenOccurrence[occ] {
						continue
					}
					seenOccurrence[occ] = true

					m := bysig[f.Signature]
					if m == nil {
						m = &mistake{first: rec.At, seenSessions: map[string]bool{}}
						bysig[f.Signature] = m
					}
					m.count++
					m.line = f.Line
					m.command = cmd
					if rec.At.Before(m.first) {
						m.first = rec.At
					}
					if rec.At.After(m.last) {
						m.last = rec.At
					}
					if !m.seenSessions[info.ID] {
						m.seenSessions[info.ID] = true
						m.sessions = append(m.sessions, info.ID)
					}
				}
			}
		}
	}

	// The ledger's bar is a second session: recurrence across contexts is
	// what separates a standing problem from one afternoon's debugging.
	var recurring []*mistake
	for _, m := range bysig {
		if len(m.sessions) >= 2 {
			recurring = append(recurring, m)
		}
	}
	sort.Slice(recurring, func(i, j int) bool {
		if a, b := len(recurring[i].sessions), len(recurring[j].sessions); a != b {
			return a > b
		}
		return recurring[i].last.After(recurring[j].last)
	})
	return recurring, len(infos), nil
}

// mistakesScope is the boundary, stated wherever the ledger renders: what
// the recorder cannot see is absent, not guessed — the /changes rule
// applied to failures.
const mistakesScope = "read from failing test-shaped commands the sessions recorded; a failure printed outside the exec tool, or by a run the record never saw, is not here"

func mistakesLines(store *session.Store, workspace string) []string {
	recurring, scanned, err := gatherMistakes(store, workspace)
	if err != nil {
		return []string{"  " + err.Error()}
	}
	if scanned == 0 {
		return []string{"  no sessions recorded for this workspace yet"}
	}
	if len(recurring) == 0 {
		return []string{
			fmt.Sprintf("  no failure recurred across this workspace's %d sessions", scanned),
			"  " + mistakesScope,
		}
	}

	var lines []string
	shown := recurring
	if len(shown) > mistakesMaxEntries {
		shown = shown[:mistakesMaxEntries]
	}
	for _, m := range shown {
		runs := "runs"
		if m.count == 1 {
			runs = "run"
		}
		lines = append(lines,
			"  "+redactCredentialTextBeforeTruncate(m.line, 76),
			fmt.Sprintf("    %s  ·  %d failing %s across %d sessions  ·  first %s, last %s",
				redactCredentialTextBeforeTruncate(m.command, 40), m.count, runs, len(m.sessions),
				m.first.Local().Format("Jan 2"), m.last.Local().Format("Jan 2 15:04")))
		ids := slices.Clone(m.sessions)
		slices.Reverse(ids)
		suffix := ""
		if len(ids) > mistakesMaxSessions {
			suffix = fmt.Sprintf(" … %d more", len(ids)-mistakesMaxSessions)
			ids = ids[:mistakesMaxSessions]
		}
		lines = append(lines, "    sessions "+strings.Join(ids, " ")+suffix+"  ·  /resume <id> reopens one")
	}
	if len(recurring) > mistakesMaxEntries {
		lines = append(lines, fmt.Sprintf("  … %d more signatures recur; the ones above met the most sessions", len(recurring)-mistakesMaxEntries))
	}
	lines = append(lines,
		"",
		"  "+mistakesScope,
		"  a fix that had to be found twice is a method worth keeping: /learn distills it into a skill pack")
	return lines
}

func cmdMistakes(m *tuiModel, args string) tea.Cmd {
	if strings.TrimSpace(args) != "" {
		return noticeCmd("error", "/mistakes reads this workspace's record and takes no argument")
	}
	m.addInfo("the failures more than one session met\n" +
		strings.Join(mistakesLines(m.app.store, m.app.workspace), "\n"))
	return nil
}

func runMistakesCLI(w io.Writer, store *session.Store, workspace string) error {
	fmt.Fprintln(w, "the failures more than one session met")
	for _, line := range mistakesLines(store, workspace) {
		fmt.Fprintln(w, cliText(strings.TrimRight(line, " ")))
	}
	return nil
}
