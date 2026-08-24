package main

// /stats and `sb stats`: the ladder's receipt at lifetime scale. /cost rungs
// prices one session against the rungs it did not run on; this reads every
// session recorded for the workspace and prices the whole history the same
// way, with the same honesty rules — cold counterfactuals, the three
// zero-dollar meterings kept apart, and an unpriceable rung saying so
// rather than showing a partial dollar figure.
//
// The scope is stated because it has edges: race losers and forks are
// counted, since their calls were real calls, while subagent sessions live
// in their own store and are not. Yesterday's calls are priced against
// today's catalog and today's ladder, because the question the table
// answers is "what would this history cost on the ladder I have now" —
// each session's own record keeps the revision that priced it at the time.

import (
	"fmt"
	"io"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

// statsLines reads every session log for the workspace and renders the
// lifetime receipt.
func statsLines(tiers []config.Tier, cat *catalog.Catalog, activeTier string, store *session.Store, workspace string) []string {
	infos, err := store.List(workspace)
	if err != nil {
		return []string{"  " + err.Error()}
	}
	if len(infos) == 0 {
		return []string{"  no sessions recorded for this workspace yet"}
	}

	var all []session.Usage
	read, unreadable := 0, 0
	for _, info := range infos {
		usages, err := session.ReadUsages(info.Path)
		if err != nil {
			unreadable++
			continue
		}
		read++
		all = append(all, usages...)
	}
	all = dedupeCopiedUsages(all)

	var totalUsage provider.Usage
	for _, u := range all {
		totalUsage = totalUsage.Add(u.Usage)
	}

	head := fmt.Sprintf("  %d sessions, %d model calls: ↓%s ↑%s tokens", read, len(all), compact(totalUsage.TotalInputTokens()), compact(totalUsage.OutputTokens))
	if totalUsage.CacheReadTokens > 0 {
		head += fmt.Sprintf(", %s of that served from cache", compact(totalUsage.CacheReadTokens))
	}
	lines := []string{head}
	if unreadable > 0 {
		lines = append(lines, fmt.Sprintf("  %d logs could not be read and are not counted", unreadable))
	}
	if len(all) == 0 {
		return append(lines, "  no model calls recorded, so there is nothing to price")
	}

	lines = append(lines, "  "+asRoutedLine(cat, all), "")

	if len(tiers) == 0 {
		return append(lines, "  no ladder is configured, so there are no rungs to compare")
	}
	lines = append(lines,
		"  the whole history priced as if every call had gone to one rung —",
		"  no cache assumed, every input token at the rung's ordinary input",
		"  rate, today's catalog and ladder, and these sessions' own token",
		"  counts, though another model would tokenize the same text differently:")
	width := 0
	for _, t := range tiers {
		if w := len(t.String()); w > width {
			width = w
		}
	}
	for _, t := range tiers {
		marker := "   "
		if t.ID == activeTier {
			marker = " * "
		}
		lines = append(lines, fmt.Sprintf(" %s%-*s  %s", marker, width, t.String(), rungWord(cat, t, all)))
	}
	lines = append(lines,
		"  race losers and forks count — their calls were real, and a fork's",
		"  copied prefix is one spend counted once; subagent sessions keep",
		"  their own store and do not",
		"  an estimator and reconciliation aid, not the provider's invoice (§15)")
	return lines
}

// statsAllLines is the receipt across every workspace the store holds: what
// sb has recorded anywhere, each workspace's as-routed line, and the grand
// totals. Rung repricing stays per workspace - a counterfactual prices one
// history against one ladder over one working set - so the all-form points
// back at the per-workspace command rather than blurring them together.
// dedupeCopiedUsages drops the usage records a fork's copy carries: the
// same target, timestamp, token counts, and cost is the same call in two
// logs, because a copied record keeps its source's At and no two real
// calls share all four. Aggregates fold them; per-session surfaces do not,
// because a fork's inherited spend is load-bearing there — /budget gates
// the fork's conversation on everything its log has priced. A record with
// no timestamp, from a log written before readers surfaced one, is never
// folded: without the discriminant, folding would be a guess.
func dedupeCopiedUsages(all []session.Usage) []session.Usage {
	seen := map[string]bool{}
	out := make([]session.Usage, 0, len(all))
	for _, u := range all {
		if u.At.IsZero() {
			out = append(out, u)
			continue
		}
		key := u.CallID
		if key == "" {
			key = fmt.Sprintf("%s@%d:%d/%d/%d/%d/%d", u.Target, u.At.UnixNano(),
				u.Usage.InputTokens, u.Usage.OutputTokens, u.Usage.CacheReadTokens, u.Usage.CacheWriteTokens, u.CostMicroUSD)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, u)
	}
	return out
}

func statsAllLines(cat *catalog.Catalog, store *session.Store) []string {
	byWorkspace, err := store.ListAll()
	if err != nil {
		return []string{"  " + err.Error()}
	}
	if len(byWorkspace) == 0 {
		return []string{"  no sessions recorded anywhere yet"}
	}
	workspaces := make([]string, 0, len(byWorkspace))
	for ws := range byWorkspace {
		workspaces = append(workspaces, ws)
	}
	sort.Strings(workspaces)

	var lines []string
	var all []session.Usage
	sessions := 0
	width := 0
	for _, ws := range workspaces {
		if len(ws) > width {
			width = len(ws)
		}
	}
	for _, ws := range workspaces {
		var usages []session.Usage
		for _, info := range byWorkspace[ws] {
			found, err := session.ReadUsages(info.Path)
			if err != nil {
				continue
			}
			usages = append(usages, found...)
		}
		usages = dedupeCopiedUsages(usages)
		sessions += len(byWorkspace[ws])
		all = append(all, usages...)
		lines = append(lines, fmt.Sprintf("  %-*s  %d sessions; %s", width, ws, len(byWorkspace[ws]), asRoutedLine(cat, usages)))
	}

	var totalUsage provider.Usage
	for _, u := range all {
		totalUsage = totalUsage.Add(u.Usage)
	}
	lines = append(lines,
		fmt.Sprintf("  across them: %d sessions, %d calls, ↓%s ↑%s tokens; %s",
			sessions, len(all), compact(totalUsage.TotalInputTokens()), compact(totalUsage.OutputTokens), asRoutedLine(cat, all)),
		"  rung repricing stays per workspace: sb stats inside one prices its history on your ladder")
	return lines
}

func cmdStats(m *tuiModel, args string) tea.Cmd {
	// Read-only over the workspace's logs, the current one included; its
	// open log reads the way `sb cost` reads it, which is what makes this
	// busy-safe.
	switch strings.TrimSpace(args) {
	case "all":
		m.addInfo(strings.Join(statsAllLines(m.app.catalog, m.app.store), "\n"))
		return nil
	case "":
		m.addInfo(strings.Join(statsLines(m.app.config.Tiers, m.app.catalog, m.app.tier.ID, m.app.store, m.app.workspace), "\n"))
		return nil
	default:
		return noticeCmd("error", "/stats takes no argument, or all")
	}
}

func runStatsCLI(w io.Writer, store *session.Store, cat *catalog.Catalog, cfg *config.Config, workspace, scope string) error {
	switch scope {
	case "all":
		for _, line := range statsAllLines(cat, store) {
			fmt.Fprintln(w, cliText(strings.TrimRight(line, " ")))
		}
		return nil
	case "":
	default:
		// A swallowed argument is a silent lie about what ran.
		return fmt.Errorf("sb stats takes no argument, or all; %q is neither", scope)
	}
	// No session is active in a CLI run, so no rung wears the marker.
	for _, line := range statsLines(cfg.Tiers, cat, "", store, workspace) {
		fmt.Fprintln(w, cliText(strings.TrimRight(line, " ")))
	}
	return nil
}
