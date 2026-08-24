package main

// The `sb races` subcommand: the paired evidence a workspace has gathered,
// summed from the command line. Each /race verdict is one human-judged
// comparison — same prompt, same prefix, two rungs — and this is where the
// corpus becomes visible: which pairs have been tried, which rung the user
// keeps, how often both sufficed. The tally is a reading aid, not a
// routing input; §8.4's caveats travel with the numbers, and nothing here
// feeds a decision, because acting on this corpus is gated behind the
// phase 7 eval that has not run.

import (
	"fmt"
	"io"
	"sort"

	"github.com/switchboard-code/switchboard/internal/session"
)

func runRacesCLI(w io.Writer, store *session.Store, workspace string) error {
	infos, err := store.List(workspace)
	if err != nil {
		return err
	}
	return racesReport(w, infos, "for "+cliText(workspace)+"; /race <tier> <prompt> runs one")
}

// runRacesAllCLI tallies the corpus across every workspace: the paired
// verdicts are evidence for the routing eval, and that argument is global
// even though each race ran somewhere in particular.
func runRacesAllCLI(w io.Writer, store *session.Store) error {
	byWorkspace, err := store.ListAll()
	if err != nil {
		return err
	}
	var infos []session.Info
	for _, list := range byWorkspace {
		infos = append(infos, list...)
	}
	return racesReport(w, infos, "anywhere; /race <tier> <prompt> runs one")
}

func racesReport(w io.Writer, infos []session.Info, where string) error {

	// A verdict lives on the session that continued, but /fork copies a
	// log's records into the branch, so the same race can sit in two logs.
	// The arm session IDs identify the trial itself, whichever logs carry
	// its record.
	type tally struct {
		pair           string
		picks          map[string]int
		ties, censored int
		total          int
	}
	seen := map[string]bool{}
	byPair := map[string]*tally{}
	races := 0
	for _, info := range infos {
		found, err := session.ReadRaces(info.Path)
		if err != nil {
			fmt.Fprintf(w, "%-22s unreadable: %s\n", cliText(info.ID), cliText(err.Error()))
			continue
		}
		for _, race := range found {
			id := race.A.SessionID + "|" + race.B.SessionID
			if seen[id] {
				continue
			}
			seen[id] = true
			races++

			pair := race.A.Tier + " vs " + race.B.Tier
			if race.B.Tier < race.A.Tier {
				pair = race.B.Tier + " vs " + race.A.Tier
			}
			t := byPair[pair]
			if t == nil {
				t = &tally{pair: pair, picks: map[string]int{}}
				byPair[pair] = t
			}
			t.total++
			switch race.Outcome {
			case "a":
				t.picks[race.A.Tier]++
			case "b":
				t.picks[race.B.Tier]++
			case "tie":
				t.ties++
			default:
				// Abandoned and incomparable races are censored, not
				// negative (§8.4): they count as run, never as preference.
				t.censored++
			}
		}
	}

	if races == 0 {
		fmt.Fprintf(w, "no races recorded %s\n", where)
		return nil
	}

	pairs := make([]*tally, 0, len(byPair))
	for _, t := range byPair {
		pairs = append(pairs, t)
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].pair < pairs[j].pair })

	for _, t := range pairs {
		line := fmt.Sprintf("%-14s %d raced", t.pair, t.total)
		tiers := make([]string, 0, len(t.picks))
		for tier := range t.picks {
			tiers = append(tiers, tier)
		}
		sort.Strings(tiers)
		for _, tier := range tiers {
			line += fmt.Sprintf(", %s picked %d", tier, t.picks[tier])
		}
		if t.ties > 0 {
			line += fmt.Sprintf(", both sufficed %d", t.ties)
		}
		if t.censored > 0 {
			line += fmt.Sprintf(", censored %d", t.censored)
		}
		fmt.Fprintln(w, cliText(line))
	}

	fmt.Fprintf(w, "\n%d races; a tie is evidence the cheaper rung was enough, a censored race is evidence of nothing (§8.4)\n", races)
	fmt.Fprintln(w, "collected for the routing eval, consumed by nothing yet: the corpus comes before the verdict")
	return nil
}
