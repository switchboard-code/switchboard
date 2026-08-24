package session

// Workspace-level accounting needs the identities and lineage records that a
// replayed State intentionally folds away. Per-session state keeps inherited
// spend for hard ceilings; this reader exposes the durable primitives so a
// cross-session receipt can count one provider call once.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

type AccountingCharge struct {
	ID           string
	CostMicroUSD int64
}

// AccountingLedger is one physical log's unfurled accounting lineage.
// Transfers are carries between ledgers rather than new provider work; the
// aggregate surface decides which reconciliation transfers stand in for a
// log outside its scope.
type AccountingLedger struct {
	SessionID       string
	Calls           []Usage
	ExternalCharges []AccountingCharge
	RetryReserves   []AccountingCharge
	Transfers       []BudgetTransfer
	Races           []Race
}

type accountingAttempt struct {
	bound    int64
	outcome  string
	external int64
	settled  bool
}

func legacyAccountingID(kind string, rec Record) string {
	sum := sha256.Sum256(rec.Payload)
	return fmt.Sprintf("legacy:%s:%d:%s", kind, rec.At.UnixNano(), hex.EncodeToString(sum[:8]))
}

// ReadAccountingLedger reads the records needed for a workspace aggregate.
// New provider calls carry an exact CallID. The timestamp-and-payload legacy
// identity exists only for pre-CallID logs and stays stable when a fork copies
// the record byte-for-byte.
func ReadAccountingLedger(path string) (AccountingLedger, error) {
	f, err := openPublishedLog(path)
	if err != nil {
		return AccountingLedger{}, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	if err := checkHeader(r, path); err != nil {
		return AccountingLedger{}, err
	}

	var out AccountingLedger
	attempts := make(map[string]accountingAttempt)
	lastSeq := 0
	budget := newReplayBudget(defaultReplayLimits, len(magic)+4)
	for {
		rec, _, err := budget.decode(r, &lastSeq)
		if isRecoverableRecordEnd(err) {
			break
		}
		if err != nil {
			return AccountingLedger{}, err
		}
		switch rec.Type {
		case RecordSessionStart:
			var start SessionStart
			if err := json.Unmarshal(rec.Payload, &start); err != nil {
				return AccountingLedger{}, err
			}
			out.SessionID = start.ID
		case RecordUsage:
			var usage Usage
			if err := json.Unmarshal(rec.Payload, &usage); err != nil {
				return AccountingLedger{}, err
			}
			if err := usage.Usage.Validate(); err != nil || usage.CostMicroUSD < 0 {
				return AccountingLedger{}, fmt.Errorf("invalid usage accounting in record %d", rec.Seq)
			}
			usage.At = rec.At
			if usage.CallID == "" {
				usage.CallID = legacyAccountingID("call", rec)
			}
			out.Calls = append(out.Calls, usage)
		case RecordRetryReserve:
			var reserve RetryReserve
			if err := json.Unmarshal(rec.Payload, &reserve); err != nil {
				return AccountingLedger{}, err
			}
			if reserve.CostMicroUSD < 0 {
				return AccountingLedger{}, fmt.Errorf("negative retry reserve in record %d", rec.Seq)
			}
			out.RetryReserves = append(out.RetryReserves, AccountingCharge{
				ID: legacyAccountingID("reserve", rec), CostMicroUSD: reserve.CostMicroUSD,
			})
		case RecordBudgetAttempt:
			var attempt BudgetAttempt
			if err := json.Unmarshal(rec.Payload, &attempt); err != nil {
				return AccountingLedger{}, err
			}
			if attempt.ID == "" || attempt.CostMicroUSD <= 0 {
				return AccountingLedger{}, fmt.Errorf("invalid budget attempt in record %d", rec.Seq)
			}
			if _, exists := attempts[attempt.ID]; exists {
				return AccountingLedger{}, fmt.Errorf("duplicate budget attempt %q", attempt.ID)
			}
			attempts[attempt.ID] = accountingAttempt{bound: attempt.CostMicroUSD}
		case RecordBudgetSettle:
			var settlement BudgetSettlement
			if err := json.Unmarshal(rec.Payload, &settlement); err != nil {
				return AccountingLedger{}, err
			}
			attempt, exists := attempts[settlement.AttemptID]
			if !exists || attempt.settled || settlement.ExternalCostMicroUSD < 0 {
				return AccountingLedger{}, fmt.Errorf("invalid budget settlement in record %d", rec.Seq)
			}
			attempt.outcome = settlement.Outcome
			if attempt.outcome != BudgetOutcomeSucceeded && attempt.outcome != BudgetOutcomeFailed {
				return AccountingLedger{}, fmt.Errorf("unknown budget outcome in record %d", rec.Seq)
			}
			attempt.external = settlement.ExternalCostMicroUSD
			attempt.settled = true
			attempts[settlement.AttemptID] = attempt
		case RecordBudgetTransfer:
			var transfer BudgetTransfer
			if err := json.Unmarshal(rec.Payload, &transfer); err != nil {
				return AccountingLedger{}, err
			}
			if transfer.Source == "" || transfer.ExternalCostMicroUSD < 0 || transfer.RetryReserveMicroUSD < 0 {
				return AccountingLedger{}, fmt.Errorf("invalid budget transfer in record %d", rec.Seq)
			}
			out.Transfers = append(out.Transfers, transfer)
		case RecordRace:
			var race Race
			if err := json.Unmarshal(rec.Payload, &race); err != nil {
				return AccountingLedger{}, err
			}
			if race.A.CostMicroUSD < 0 || race.B.CostMicroUSD < 0 {
				return AccountingLedger{}, fmt.Errorf("negative race cost in record %d", rec.Seq)
			}
			out.Races = append(out.Races, race)
		}
	}

	for id, attempt := range attempts {
		if attempt.external > 0 {
			out.ExternalCharges = append(out.ExternalCharges, AccountingCharge{ID: "attempt:" + id, CostMicroUSD: attempt.external})
		}
		if !attempt.settled || attempt.outcome == BudgetOutcomeFailed {
			out.RetryReserves = append(out.RetryReserves, AccountingCharge{ID: "attempt:" + id, CostMicroUSD: attempt.bound})
		}
	}
	return out, nil
}
