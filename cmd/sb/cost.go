package main

// The `sb cost` subcommand: §15's cross-provider accounting, from the
// command line. Per session it reports calls, tokens, and what the catalog
// priced the work at, and the three zero-dollar meterings stay three
// different things here as everywhere: a local session says "local", a plan
// session says "plan", and neither is folded into the dollar total, because
// telling someone their quota burn was free teaches the wrong lesson.

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

func runCostCLI(w io.Writer, store *session.Store, cat *catalog.Catalog, workspace string) error {
	infos, err := store.List(workspace)
	if err != nil {
		return err
	}
	if len(infos) == 0 {
		fmt.Fprintf(w, "no sessions recorded for %s\n", cliText(workspace))
		return nil
	}

	fmt.Fprintf(w, "%-22s %-19s %6s %10s %10s  %s\n", "session", "when", "calls", "in", "out", "cost")
	var priced, local, plan, unpriced, mixed int
	var ledgers []session.AccountingLedger
	for _, info := range infos {
		state, err := session.ReadState(info.Path)
		if err != nil {
			fmt.Fprintf(w, "%-22s unreadable: %s\n", cliText(info.ID), cliText(err.Error()))
			continue
		}
		fmt.Fprintf(w, "%-22s %-19s %6d %10d %10d  %s\n",
			state.ID, info.Modified.Local().Format("2006-01-02 15:04:05"),
			state.Calls, state.Usage.InputTokens, state.Usage.OutputTokens,
			costWord(cat, state, &priced, &local, &plan, &unpriced, &mixed))
		ledger, ledgerErr := session.ReadAccountingLedger(info.Path)
		if ledgerErr != nil {
			return fmt.Errorf("reading accounting lineage for session %s: %w", info.ID, ledgerErr)
		}
		ledgers = append(ledgers, ledger)
	}

	total, reserve := aggregateWorkspaceAccounting(ledgers)
	fmt.Fprintf(w, "\n%d sessions", len(infos))
	if priced > 0 {
		fmt.Fprintf(w, "; %s estimated for unique observed provider work represented by the %d session ledgers that bill dollars", total, priced)
	}
	if reserve > 0 {
		fmt.Fprintf(w, "; %s retry reserve remains separate for unique failed or unsettled attempts", reserve)
	}
	if local > 0 {
		fmt.Fprintf(w, "; %d ran locally, so there is nothing to bill", local)
	}
	if plan > 0 {
		fmt.Fprintf(w, "; %d billed a plan, consuming quota rather than dollars", plan)
	}
	if unpriced > 0 {
		fmt.Fprintf(w, "; %d had nothing the catalog could price", unpriced)
	}
	if mixed > 0 {
		fmt.Fprintf(w, "; %d crossed metering types, shown as mixed", mixed)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "an estimator and reconciliation aid, not the provider's invoice (§15)")
	return nil
}

// costWord renders one session's cost column and files it under the right
// metering for the totals line.
func costWord(cat *catalog.Catalog, state session.State, priced, local, plan, unpriced, mixed *int) string {
	accounted := catalog.Money(state.AccountedCostMicroUSD())
	if state.ExternalCostMicroUSD > 0 {
		*priced++
		return withRetryReserve(accounted.String()+" (includes delegate/race)", state.RetryReserveMicroUSD)
	}
	// The session target is its opening identity, while the accumulated ledger
	// may include calls after routing moved elsewhere. Once observed dollars
	// exist, classifying the whole ledger from that one target can hide real
	// spend under "local" or "plan". Observed accounting is authoritative.
	if accounted > 0 {
		*priced++
		return withRetryReserve(accounted.String(), state.RetryReserveMicroUSD)
	}
	if kinds, ok := routedMeteringKinds(cat, state); ok {
		if len(kinds) > 1 {
			*mixed++
			return withRetryReserve("mixed — "+strings.Join(kinds, " + "), state.RetryReserveMicroUSD)
		}
		switch kinds[0] {
		case "local":
			*local++
			return withRetryReserve("local", state.RetryReserveMicroUSD)
		case "plan":
			*plan++
			return withRetryReserve("plan", state.RetryReserveMicroUSD)
		case "dollar-metered":
			*priced++
			return withRetryReserve(accounted.String(), state.RetryReserveMicroUSD)
		case "no per-token cost":
			*unpriced++
			return withRetryReserve("no per-token cost", state.RetryReserveMicroUSD)
		default:
			*unpriced++
			return withRetryReserve("unpriced", state.RetryReserveMicroUSD)
		}
	}
	target, err := parseRecordedTarget(state.Target)
	if err != nil {
		*unpriced++
		return withRetryReserve("unpriced", state.RetryReserveMicroUSD)
	}
	info, _, ok := cat.Lookup(target)
	switch {
	case !ok:
		*unpriced++
		return withRetryReserve("unpriced", state.RetryReserveMicroUSD)
	case info.Metering == catalog.Local:
		*local++
		return withRetryReserve("local", state.RetryReserveMicroUSD)
	case info.Metering == catalog.Plan:
		*plan++
		return withRetryReserve("plan", state.RetryReserveMicroUSD)
	case info.Free():
		// Dollar-metered, priced at zero throughout: rendering the recorded
		// zero as $0.00 would claim a bill where the catalog holds no rates.
		*unpriced++
		return withRetryReserve("no per-token cost", state.RetryReserveMicroUSD)
	default:
		*priced++
		return withRetryReserve(accounted.String(), state.RetryReserveMicroUSD)
	}
}

// routedMeteringKinds classifies the calls that actually occurred, not the
// session's opening or current target. UsageTargets is replay-derived from the
// durable per-call records, so a local→plan move cannot be flattened into one
// zero-dollar category merely because both happen to total $0.
func routedMeteringKinds(cat *catalog.Catalog, state session.State) ([]string, bool) {
	if len(state.UsageTargets) == 0 {
		return nil, false
	}
	seen := map[string]bool{}
	for _, recorded := range state.UsageTargets {
		target, err := parseRecordedTarget(recorded)
		if err != nil {
			seen["unpriced"] = true
			continue
		}
		info, _, ok := cat.Lookup(target)
		switch {
		case !ok:
			seen["unpriced"] = true
		case info.Metering == catalog.Local:
			seen["local"] = true
		case info.Metering == catalog.Plan:
			seen["plan"] = true
		case info.Free():
			seen["no per-token cost"] = true
		default:
			seen["dollar-metered"] = true
		}
	}
	order := []string{"local", "plan", "dollar-metered", "no per-token cost", "unpriced"}
	kinds := make([]string, 0, len(seen))
	for _, kind := range order {
		if seen[kind] {
			kinds = append(kinds, kind)
		}
	}
	return kinds, len(kinds) > 0
}

func withRetryReserve(observed string, reserveMicroUSD int64) string {
	if reserveMicroUSD <= 0 {
		return observed
	}
	return observed + " + " + catalog.Money(reserveMicroUSD).String() + " retry reserve (not observed cost)"
}

// aggregateWorkspaceAccounting counts durable provider-call and attempt
// identities once even when a fork carries their records into several logs.
// Per-session rows deliberately retain that inherited accounting because it
// governs each continuation's ceiling; only this workspace grand total folds
// lineage.
func aggregateWorkspaceAccounting(ledgers []session.AccountingLedger) (observed, reserve catalog.Money) {
	seenCalls := make(map[string]bool)
	seenExternal := make(map[string]bool)
	seenReserve := make(map[string]bool)
	seenTransfer := make(map[string]bool)
	presentSessions := make(map[string]bool)
	for _, ledger := range ledgers {
		presentSessions[ledger.SessionID] = true
	}

	var calls, external catalog.Money
	for _, ledger := range ledgers {
		for _, call := range ledger.Calls {
			if seenCalls[call.CallID] {
				continue
			}
			seenCalls[call.CallID] = true
			calls = addMoney(calls, catalog.Money(call.CostMicroUSD))
		}
		for _, charge := range ledger.ExternalCharges {
			if seenExternal[charge.ID] {
				continue
			}
			seenExternal[charge.ID] = true
			external = addMoney(external, catalog.Money(charge.CostMicroUSD))
		}
		for _, charge := range ledger.RetryReserves {
			if seenReserve[charge.ID] {
				continue
			}
			seenReserve[charge.ID] = true
			reserve = addMoney(reserve, catalog.Money(charge.CostMicroUSD))
		}
		for _, transfer := range ledger.Transfers {
			if seenTransfer[transfer.Source] {
				continue
			}
			seenTransfer[transfer.Source] = true
			// Most transfers carry work already represented by another log.
			// These two reconciliation forms stand in for provider calls whose
			// ordinary settlement could not be written; delegate logs live in a
			// different store, and race reconcile is mirrored by arm logs below.
			if strings.HasPrefix(transfer.Source, "delegate-reconcile:") || strings.HasPrefix(transfer.Source, "race-reconcile:") {
				external = addMoney(external, catalog.Money(transfer.ExternalCostMicroUSD))
			}
		}
	}

	// Race calls have both an authoritative external admission on the origin
	// ledger and local Usage on the finalized arm logs. When those arm logs are
	// in this aggregate, remove exactly their recorded own-work cost from the
	// external mirror. Unique CallIDs already fold their copied prefixes.
	seenRace := make(map[string]bool)
	var representedRace catalog.Money
	for _, ledger := range ledgers {
		for _, race := range ledger.Races {
			key := race.A.SessionID + "\x00" + race.B.SessionID
			if seenRace[key] {
				continue
			}
			seenRace[key] = true
			for _, arm := range []session.RaceArm{race.A, race.B} {
				if presentSessions[arm.SessionID] {
					representedRace = addMoney(representedRace, catalog.Money(arm.CostMicroUSD))
				}
			}
		}
	}
	if representedRace >= external {
		external = 0
	} else {
		external -= representedRace
	}
	return addMoney(calls, external), reserve
}

// costTurnsLines is the per-ask receipt: the session's turns ordered by
// what they billed, each beside its own words, so "which asks cost the
// most" reads straight off the record. Turns that billed nothing fold
// into one closing line — local, plan, and unpriced stay out of the
// dollar rows, because a $0.00 row teaches the wrong lesson here as
// everywhere.
func costTurnsLines(turns []session.TurnCost) []string {
	if len(turns) == 0 {
		return []string{"  no turns recorded yet"}
	}
	billed := make([]session.TurnCost, 0, len(turns))
	background := make([]session.TurnCost, 0, 4)
	var unbilled, unbilledCalls int
	var unbilledUsage provider.Usage
	for _, t := range turns {
		if t.Purpose != "" && t.Purpose != session.UsagePurposeTurn {
			background = append(background, t)
			continue
		}
		if t.CostMicroUSD > 0 {
			billed = append(billed, t)
			continue
		}
		unbilled++
		if t.Calls > 0 && unbilledCalls <= int(^uint(0)>>1)-t.Calls {
			unbilledCalls += t.Calls
		} else if t.Calls > 0 {
			unbilledCalls = int(^uint(0) >> 1)
		}
		unbilledUsage = unbilledUsage.Add(t.Usage)
	}
	sort.SliceStable(billed, func(i, j int) bool { return billed[i].CostMicroUSD > billed[j].CostMicroUSD })

	var lines []string
	const shown = 8
	for i, t := range billed {
		if i == shown {
			var rest catalog.Money
			for _, more := range billed[shown:] {
				rest = addMoney(rest, catalog.Money(more.CostMicroUSD))
			}
			lines = append(lines, fmt.Sprintf("  … and %d more billed turns, %s between them", len(billed)-shown, rest))
			break
		}
		in := t.Usage.TotalInputTokens()
		lines = append(lines, fmt.Sprintf("  #%-3d %-10s ↓%s ↑%s  %d calls  %s",
			t.Turn, catalog.Money(t.CostMicroUSD).String(), compact(in), compact(t.Usage.OutputTokens),
			t.Calls, recordedTurnPrompt(t.Prompt, t.PromptAuthoredKnown, t.PromptSynthetic, 48)))
	}
	if len(billed) == 0 {
		lines = append(lines, "  no turn billed dollars")
	}
	for _, t := range background {
		lines = append(lines, fmt.Sprintf("  background/%-11s %-10s ↓%s ↑%s  %d calls  (not attributed to a user turn)",
			t.Purpose, catalog.Money(t.CostMicroUSD).String(), compact(t.Usage.TotalInputTokens()),
			compact(t.Usage.OutputTokens), t.Calls))
	}
	if unbilled > 0 {
		word := "turns"
		if unbilled == 1 {
			word = "turn"
		}
		lines = append(lines, fmt.Sprintf("  %d %s billed nothing — local, plan, or unpriced calls (↓%s ↑%s across %d calls); /cost keeps those meterings apart",
			unbilled, word, compact(unbilledUsage.TotalInputTokens()), compact(unbilledUsage.OutputTokens), unbilledCalls))
	}
	return lines
}
