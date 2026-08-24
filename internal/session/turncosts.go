package session

// Read-only per-turn metering, for the surface that answers "which asks
// cost the most". The log already interleaves each turn's opening with
// the usage records its calls produced; this reader just folds them back
// onto the turn, the same walk the edit reader makes.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"

	"github.com/switchboard-code/switchboard/internal/provider"
)

// TurnCost is one user turn's metering: what its calls consumed, summed
// from the usage records that rode between its opening and the next.
type TurnCost struct {
	Turn int
	// Prompt is populated only from an exact durable authored projection.
	// PromptAuthoredKnown distinguishes an intentionally empty authored prompt
	// from legacy Content that may contain @file, shell, or harness expansion.
	Prompt              string
	PromptAuthoredKnown bool
	PromptSynthetic     bool
	// Purpose is "turn" for user turns. Compact, learn, advisor, and
	// unattributed buckets are explicit background model work and have Turn 0.
	Purpose      string
	Calls        int
	Usage        provider.Usage
	CostMicroUSD int64
}

// ReadTurnCosts replays a log and returns each turn's summed metering, in
// turn order, followed by explicit background-purpose buckets. A one-shot
// advisor, compaction, or skill-distillation request must never ride the most
// recent user's bill merely because its record happened to follow that turn.
func ReadTurnCosts(path string) ([]TurnCost, error) {
	f, err := openPublishedLog(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	if err := checkHeader(r, path); err != nil {
		return nil, err
	}

	var turns []TurnCost
	background := make(map[string]*TurnCost)
	backgroundOrder := make([]string, 0, 4)
	backgroundFor := func(purpose string) *TurnCost {
		if purpose == "" || purpose == UsagePurposeTurn {
			purpose = UsagePurposeUnknown
		}
		if bucket := background[purpose]; bucket != nil {
			return bucket
		}
		bucket := &TurnCost{Purpose: purpose, Prompt: "background model work: " + purpose}
		background[purpose] = bucket
		backgroundOrder = append(backgroundOrder, purpose)
		return bucket
	}
	finish := func() []TurnCost {
		out := append([]TurnCost(nil), turns...)
		for _, purpose := range backgroundOrder {
			out = append(out, *background[purpose])
		}
		return out
	}
	addCost := func(cur *TurnCost, amount int64) error {
		total, err := checkedMicroUSDAdd(cur.CostMicroUSD, amount)
		if err != nil {
			return err
		}
		cur.CostMicroUSD = total
		return nil
	}
	lastSeq := 0
	budget := newReplayBudget(defaultReplayLimits, len(magic)+4)
	for {
		rec, _, err := budget.decode(r, &lastSeq)
		if isRecoverableRecordEnd(err) {
			return finish(), nil
		}
		if err != nil {
			return nil, err
		}
		if message, ok, err := conversationMessage(rec); err != nil {
			return nil, err
		} else if ok {
			if OpensTurn(message) {
				prompt, known, synthetic := authoredTurnPrompt(message)
				turns = append(turns, TurnCost{
					Turn: len(turns) + 1, Prompt: prompt, PromptAuthoredKnown: known,
					PromptSynthetic: synthetic, Purpose: UsagePurposeTurn,
				})
			}
			continue
		}
		switch rec.Type {
		case RecordUsage:
			var u Usage
			if err := json.Unmarshal(rec.Payload, &u); err != nil {
				return nil, err
			}
			var cur *TurnCost
			if u.EffectivePurpose() == UsagePurposeTurn && len(turns) > 0 {
				cur = &turns[len(turns)-1]
			} else {
				cur = backgroundFor(u.EffectivePurpose())
			}
			usage, err := cur.Usage.CheckedAdd(u.Usage)
			if err != nil {
				return nil, fmt.Errorf("turn usage in record %d: %w", rec.Seq, err)
			}
			if cur.Calls == math.MaxInt {
				return nil, fmt.Errorf("turn call accounting overflow in record %d", rec.Seq)
			}
			cur.Calls++
			cur.Usage = usage
			if err := addCost(cur, u.CostMicroUSD); err != nil {
				return nil, fmt.Errorf("turn cost in record %d: %w", rec.Seq, err)
			}
		case RecordBudgetSettle:
			if len(turns) == 0 {
				continue
			}
			var settlement BudgetSettlement
			if err := json.Unmarshal(rec.Payload, &settlement); err != nil {
				return nil, err
			}
			if err := addCost(&turns[len(turns)-1], settlement.ExternalCostMicroUSD); err != nil {
				return nil, fmt.Errorf("turn settlement in record %d: %w", rec.Seq, err)
			}
		case RecordBudgetTransfer:
			if len(turns) == 0 {
				continue
			}
			var transfer BudgetTransfer
			if err := json.Unmarshal(rec.Payload, &transfer); err != nil {
				return nil, err
			}
			if err := addCost(&turns[len(turns)-1], transfer.ExternalCostMicroUSD); err != nil {
				return nil, fmt.Errorf("turn transfer in record %d: %w", rec.Seq, err)
			}
		}
	}
}
