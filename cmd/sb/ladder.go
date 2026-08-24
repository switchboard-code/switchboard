package main

// /ladder and `sb ladder`: where the workspace's recorded turns opened and
// where they ended, summed per rung. Every route record already carries the
// answer for one turn, and /why answers for one session; nothing answered
// the question the ladder itself poses — does work that starts low stay
// low — across the whole record. The sum is presented under §8.4's own
// caveats, stated in the output: a move is not a verdict on the rung it
// left, and an abandoned turn is censored, counted as opened and nothing
// more. Read-only over the logs, the open one included.

import (
	"fmt"
	"io"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
	route "github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/session"
)

// rungRecord is one rung's summed turns: where they opened, whether they
// stayed, and where the ones that moved went. "Stayed" is position, not
// praise — a clean completion is weak evidence of sufficiency, so the word
// deliberately claims nothing beyond the turn ending where it began.
type rungRecord struct {
	opened      int
	stayed      int
	abandoned   int
	unavailable int
	movedTo     map[routeDestination]int
}

type routeDestination struct {
	tier   string
	target provider.RouteTargetID
}

// gatherLadder sums route records across the workspace's sessions, keyed by
// the tier id each record opened on. A fork's copied record deduplicates on
// the record's own timestamp, the same mechanism every other aggregate
// reader here uses.
func gatherLadder(store *session.Store, workspace string) (map[string]*rungRecord, int, error) {
	infos, err := store.List(workspace)
	if err != nil {
		return nil, 0, err
	}

	byTier := map[string]*rungRecord{}
	seen := map[string]bool{}
	sessions := 0
	for _, info := range infos {
		tl, err := session.ReadTimeline(info.Path)
		if err != nil {
			continue
		}
		contributed := false
		for _, rec := range tl {
			if rec.Route == nil {
				continue
			}
			r := rec.Route
			key := fmt.Sprintf("%d/%s/%s/%d", rec.At.UnixNano(), r.Tier, r.Target, r.TurnDepth)
			if seen[key] {
				continue
			}
			seen[key] = true
			contributed = true

			rung := byTier[r.Tier]
			if rung == nil {
				rung = &rungRecord{movedTo: map[routeDestination]int{}}
				byTier[r.Tier] = rung
			}
			rung.opened++
			switch {
			case r.Outcome == string(route.Abandoned):
				rung.abandoned++
			case r.Outcome == string(route.Failed):
				rung.unavailable++
			case r.EndedTier != "" && (r.Escalations > 0 || r.EndedTier != r.Tier):
				rung.movedTo[routeDestination{tier: r.EndedTier, target: r.EndedOn}]++
			case r.EndedOn != "" && (r.Escalations > 0 || r.EndedOn != r.Target):
				rung.movedTo[routeDestination{target: r.EndedOn}]++
			default:
				rung.stayed++
			}
		}
		if contributed {
			sessions++
		}
	}
	return byTier, sessions, nil
}

func ladderLines(tiers []config.Tier, store *session.Store, workspace string) []string {
	byTier, sessions, err := gatherLadder(store, workspace)
	if err != nil {
		return []string{"  " + err.Error()}
	}
	total := 0
	for _, r := range byTier {
		total += r.opened
	}
	if total == 0 {
		return []string{"  no routed turns recorded for this workspace yet"}
	}

	// Legacy records name only a target. Map it back to a tier only when the
	// current ladder has exactly one owner (primary or fallback); shared targets
	// stay target-only rather than acquiring whichever tier was visited last.
	targetOwners := map[provider.RouteTargetID][]string{}
	for _, t := range tiers {
		seen := map[provider.RouteTargetID]bool{}
		for _, target := range append([]provider.RouteTarget{t.Target}, t.Fallbacks...) {
			id := target.ID()
			if !seen[id] {
				targetOwners[id] = append(targetOwners[id], t.ID)
				seen[id] = true
			}
		}
	}

	turnWord, sessWord := "turns", "sessions"
	if total == 1 {
		turnWord = "turn"
	}
	if sessions == 1 {
		sessWord = "session"
	}
	var lines []string
	lines = append(lines, fmt.Sprintf("  %d %s across %d %s, each summed where it opened", total, turnWord, sessions, sessWord))
	covered := map[string]bool{}
	renderRung := func(id, label string, r *rungRecord) {
		head := fmt.Sprintf("  %-4s %-8s %d turns · stayed %d", id, label, r.opened, r.stayed)
		if r.abandoned > 0 {
			head += fmt.Sprintf(" · abandoned %d", r.abandoned)
		}
		if r.unavailable > 0 {
			head += fmt.Sprintf(" · unavailable %d", r.unavailable)
		}
		lines = append(lines, head)
		if len(r.movedTo) == 0 {
			return
		}
		type destination struct {
			name  string
			count int
		}
		normalized := map[routeDestination]int{}
		configuredByID := make(map[string]config.Tier, len(tiers))
		for _, configured := range tiers {
			configuredByID[configured.ID] = configured
		}
		for d, count := range r.movedTo {
			if d.tier == "" && d.target != "" {
				if owners := targetOwners[d.target]; len(owners) == 1 {
					d.tier = owners[0]
				}
			}
			normalized[d] += count
		}
		dests := make([]destination, 0, len(normalized))
		for d, count := range normalized {
			name := d.tier
			target := d.target
			if target == "" && d.tier != "" {
				if configured, ok := configuredByID[d.tier]; ok {
					target = configured.Target.ID()
				}
			}
			if target != "" {
				targetName := provider.DisplayRouteTargetID(target)
				if name == "" {
					name = targetName
				} else {
					name += " (" + targetName + ")"
				}
			}
			dests = append(dests, destination{name: name, count: count})
		}
		sort.Slice(dests, func(i, j int) bool {
			if dests[i].count != dests[j].count {
				return dests[i].count > dests[j].count
			}
			return dests[i].name < dests[j].name
		})
		parts := make([]string, 0, len(dests))
		for _, d := range dests {
			parts = append(parts, fmt.Sprintf("%s ×%d", d.name, d.count))
		}
		lines = append(lines, "       moved to "+strings.Join(parts, ", "))
	}
	for _, t := range tiers {
		covered[t.ID] = true
		r := byTier[t.ID]
		if r == nil {
			lines = append(lines, fmt.Sprintf("  %-4s %-8s no recorded turns opened here", t.ID, t.Label))
			continue
		}
		renderRung(t.ID, t.Label, r)
	}
	var stray []string
	for id := range byTier {
		if !covered[id] {
			stray = append(stray, id)
		}
	}
	sort.Strings(stray)
	for _, id := range stray {
		renderRung(id, "", byTier[id])
		lines = append(lines, "       a rung today's ladder does not name")
	}

	lines = append(lines,
		"",
		"  a move is not a verdict on the rung it left: provider failure, a phase",
		"  change, and a bad rule all produce one, and an abandoned turn says",
		"  nothing about the choice. The paired verdicts are /races, the",
		"  surviving lines are /blame, the money is /stats.")
	return lines
}

func cmdLadder(m *tuiModel, args string) tea.Cmd {
	if strings.TrimSpace(args) != "" {
		return noticeCmd("error", "/ladder reads this workspace's record and takes no argument")
	}
	m.addInfo("the ladder at work\n" +
		strings.Join(ladderLines(m.app.config.Tiers, m.app.store, m.app.workspace), "\n"))
	return nil
}

func runLadderCLI(w io.Writer, tiers []config.Tier, store *session.Store, workspace string) error {
	fmt.Fprintln(w, "the ladder at work")
	for _, line := range ladderLines(tiers, store, workspace) {
		fmt.Fprintln(w, cliText(strings.TrimRight(line, " ")))
	}
	return nil
}
