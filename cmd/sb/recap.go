package main

// /recap and `sb recap`: where you left off. The morning question is not
// "which sessions exist" (/resume's picker) or "which one said X" (/find) —
// it is "what was I doing, and did it land": the last session's opening,
// its route, the files it wrote, its verdicts, and its bill, on one screen,
// before any model is dialed. Everything here replays the record; the scope
// boundary rides in the output, the /changes rule as always.

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

const recapMaxFiles = 8

// recapLines tells one session's story from its log. An empty id means the
// workspace's most recent session; skip names a log to pass over, which is
// how the in-session form avoids reciting the conversation it sits inside.
func recapLines(store *session.Store, workspace, id, skip string) []string {
	infos, err := store.List(workspace)
	if err != nil {
		return []string{"  " + err.Error()}
	}
	var info *session.Info
	for i := range infos {
		if id != "" && infos[i].ID == id {
			info = &infos[i]
			break
		}
		if id == "" && infos[i].ID != skip {
			info = &infos[i]
			break
		}
	}
	if info == nil {
		if id != "" {
			return []string{"  no session " + id + " recorded for this workspace; /find searches what was said"}
		}
		return []string{"  no earlier session recorded for this workspace yet"}
	}

	tl, err := session.ReadTimeline(info.Path)
	if err != nil {
		return []string{"  " + err.Error()}
	}
	state, err := session.ReadState(info.Path)
	if err != nil {
		return []string{"  " + err.Error()}
	}

	openingSummary, _ := session.ReadOpeningSummary(info.Path)
	opening := safeOpeningText(openingSummary)
	if opening == "" {
		if openingSummary.Found {
			opening = "(authored wording unavailable for this legacy session)"
		} else {
			opening = "(no prompt recorded)"
		}
	}

	var files []string
	seenFile := map[string]bool{}
	turns, moves := 0, 0
	firstTier, lastTier := "", ""
	oneTier := true
	raceWins := map[string]int{}
	races := 0
	for _, rec := range tl {
		switch {
		case rec.Message != nil:
			for _, block := range rec.Message.Content {
				use, ok := block.(provider.ToolUse)
				if !ok || (use.Name != "write" && use.Name != "edit") {
					continue
				}
				var in struct {
					Path string `json:"path"`
				}
				if json.Unmarshal(use.Input, &in) != nil || in.Path == "" || seenFile[in.Path] {
					continue
				}
				seenFile[in.Path] = true
				files = append(files, in.Path)
			}
		case rec.Route != nil:
			turns++
			routed := rec.Route
			if firstTier == "" {
				firstTier = routed.Tier
			}
			endedTier := routed.Tier
			if routed.EndedTier != "" {
				endedTier = routed.EndedTier
			}
			if routed.Tier != firstTier || endedTier != firstTier {
				oneTier = false
			}
			lastTier = endedTier
			routeMoves := routed.Escalations
			if routeMoves == 0 && ((routed.EndedTier != "" && routed.EndedTier != routed.Tier) ||
				(routed.EndedOn != "" && routed.EndedOn != routed.Target)) {
				// Legacy route records did not count moves explicitly. Retain their
				// one recoverable transition without pretending to know more.
				routeMoves = 1
			}
			moves += routeMoves
		case rec.Race != nil:
			races++
			if rec.Race.Kept != "" {
				raceWins[rec.Race.Kept]++
			}
		}
	}

	var lines []string
	span := info.Modified.Local().Format("Jan 2 15:04")
	if len(tl) > 0 {
		span = tl[0].At.Local().Format("Jan 2 15:04") + " – " + info.Modified.Local().Format("15:04")
	}
	bill := "nothing billed"
	if state.AccountedCostMicroUSD() > 0 {
		bill = catalog.Money(state.AccountedCostMicroUSD()).String()
	}
	turnWord := "turns"
	if turns == 1 {
		turnWord = "turn"
	}
	lines = append(lines,
		fmt.Sprintf("  %s  %s  ·  %d %s  ·  %s", info.ID, span, turns, turnWord, bill),
		"  "+redactCredentialTextBeforeTruncate(opening, 76))

	// The route line claims only what opening tiers and move counts can
	// carry: where a moved turn ended is a target id the record holds, but
	// naming it as a rung would need a ladder this reader does not take.
	switch {
	case turns == 0:
	case oneTier && moves == 0:
		lines = append(lines, fmt.Sprintf("  every turn ran on %s", firstTier))
	default:
		moveWord := "moves"
		if moves == 1 {
			moveWord = "move"
		}
		lines = append(lines, fmt.Sprintf("  first turn on %s, last on %s, %d mid-turn %s", firstTier, lastTier, moves, moveWord))
	}

	if len(files) > 0 {
		shown := files
		suffix := ""
		if len(shown) > recapMaxFiles {
			suffix = fmt.Sprintf(" … %d more", len(shown)-recapMaxFiles)
			shown = shown[:recapMaxFiles]
		}
		lines = append(lines, "  wrote "+strings.Join(shown, ", ")+suffix)
	}
	if races > 0 {
		kepts := make([]string, 0, len(raceWins))
		for kept := range raceWins {
			kepts = append(kepts, kept)
		}
		sort.Strings(kepts)
		parts := make([]string, 0, len(kepts))
		for _, kept := range kepts {
			parts = append(parts, fmt.Sprintf("%s kept ×%d", kept, raceWins[kept]))
		}
		raceWord := "races"
		if races == 1 {
			raceWord = "race"
		}
		line := fmt.Sprintf("  %d %s", races, raceWord)
		if len(parts) > 0 {
			line += ": " + strings.Join(parts, ", ")
		}
		lines = append(lines, line)
	}

	lines = append(lines,
		"  what write and edit touched is listed; a shell command's side effects are outside the record",
		"  /resume "+info.ID+" picks it up  ·  /blame says which of its lines survived")
	return lines
}

func cmdRecap(m *tuiModel, args string) tea.Cmd {
	id := strings.TrimSpace(args)
	// The session this command is typed into is not "where you left off";
	// bare /recap looks past it to the previous log.
	current := m.app.loop.Session.State().ID
	m.addInfo("where you left off\n" +
		strings.Join(recapLines(m.app.store, m.app.workspace, id, current), "\n"))
	return nil
}

func runRecapCLI(w io.Writer, store *session.Store, workspace, id string) error {
	fmt.Fprintln(w, "where you left off")
	for _, line := range recapLines(store, workspace, id, "") {
		fmt.Fprintln(w, cliText(strings.TrimRight(line, " ")))
	}
	return nil
}
