package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
)

// ErrSessionReplayTooLarge reports a complete log whose cumulative replay
// would exceed one of the process's finite adoption limits. The log remains
// intact and inventory may report its validated prefix, but no full-history
// reader or writable resume returns that prefix as a usable session.
var ErrSessionReplayTooLarge = errors.New("session replay exceeds size limit")

// A single record already has a 64 MiB physical cap. These cumulative limits
// are deliberately generous enough for long-running interactive sessions,
// while preventing a validly framed private-store log from consuming memory
// without bound when it is listed, resumed, forked, or summarized.
type replayLimits struct {
	bytes    int64
	records  int
	messages int
	blocks   int
}

var defaultReplayLimits = replayLimits{
	bytes:    128 << 20,
	records:  262_144,
	messages: 65_536,
	blocks:   262_144,
}

func (l replayLimits) normalized() replayLimits {
	if l.bytes <= 0 || l.records <= 0 || l.messages <= 0 || l.blocks <= 0 {
		return defaultReplayLimits
	}
	return l
}

type replayBudget struct {
	limits replayLimits
	bytes  int64

	records  int
	messages int
	blocks   int

	// drafts records only block indexes already admitted for a logical draft.
	// It is accounting state, not replay state; the real Session still validates
	// record ordering and exact final-message equality before adopting anything.
	drafts map[string]map[int]struct{}
}

func newReplayBudget(limits replayLimits, headerBytes int) *replayBudget {
	limits = limits.normalized()
	return &replayBudget{limits: limits, bytes: int64(headerBytes), drafts: make(map[string]map[int]struct{})}
}

func (b *replayBudget) decode(r *bufio.Reader, previous *int) (Record, int, error) {
	rec, consumed, err := decodeSequencedRecord(r, previous)
	if err != nil {
		return rec, consumed, err
	}
	if err := b.admit(rec, consumed); err != nil {
		return Record{}, consumed, err
	}
	return rec, consumed, nil
}

type replayContinuityShape struct {
	Message json.RawMessage `json:"message"`
}

type replayDraftShape struct {
	ID string `json:"id"`
}

// admit accounts for one complete frame before any caller folds that record
// into state or appends it to an output slice. Accounting itself commits only
// after every dimension fits, so the record that crosses a limit is never
// partially adopted.
func (b *replayBudget) admit(rec Record, consumed int) error {
	if consumed < 0 || b.bytes > math.MaxInt64-int64(consumed) {
		return fmt.Errorf("%w: byte accounting overflow", ErrSessionReplayTooLarge)
	}
	nextBytes := b.bytes + int64(consumed)
	nextRecords, err := replayCountAdd(b.records, 1)
	if err != nil {
		return err
	}

	addMessages, addBlocks := 0, 0
	var closeDraft string
	var draftID string
	createDraft := false
	var newDraftIndexes []int

	switch rec.Type {
	case RecordMessage, RecordMessageContinuity:
		payload := rec.Payload
		if rec.Type == RecordMessageContinuity {
			var outer replayContinuityShape
			if err := json.Unmarshal(rec.Payload, &outer); err != nil {
				return err
			}
			payload = outer.Message
		}
		draftID, blockCount, err := replayMessageCounts(payload)
		if err != nil {
			return err
		}
		if draftID != "" {
			if _, active := b.drafts[draftID]; active {
				closeDraft = draftID
			} else {
				// Replay will reject the dangling finalization. Count it as an
				// ordinary message meanwhile so a projection that ignores its
				// semantics cannot use malformed records to evade the limits.
				addMessages = 1
				addBlocks = blockCount
			}
		} else {
			addMessages = 1
			addBlocks = blockCount
		}
	case RecordAssistantDraft:
		var draft replayDraftShape
		if err := json.Unmarshal(rec.Payload, &draft); err != nil {
			return err
		}
		draftID = draft.ID
		indexes, active := b.drafts[draftID]
		if !active {
			addMessages = 1
			createDraft = true
		}
		newDraftIndexes, err = replayDraftIndexes(rec.Payload, indexes, b.limits.blocks-b.blocks)
		if err != nil {
			return err
		}
		addBlocks = len(newDraftIndexes)
	}

	nextMessages, err := replayCountAdd(b.messages, addMessages)
	if err != nil {
		return err
	}
	nextBlocks, err := replayCountAdd(b.blocks, addBlocks)
	if err != nil {
		return err
	}
	if nextBytes > b.limits.bytes {
		return fmt.Errorf("%w: more than %d bytes", ErrSessionReplayTooLarge, b.limits.bytes)
	}
	if nextRecords > b.limits.records {
		return fmt.Errorf("%w: more than %d records", ErrSessionReplayTooLarge, b.limits.records)
	}
	if nextMessages > b.limits.messages {
		return fmt.Errorf("%w: more than %d messages", ErrSessionReplayTooLarge, b.limits.messages)
	}
	if nextBlocks > b.limits.blocks {
		return fmt.Errorf("%w: more than %d message blocks", ErrSessionReplayTooLarge, b.limits.blocks)
	}

	b.bytes, b.records, b.messages, b.blocks = nextBytes, nextRecords, nextMessages, nextBlocks
	if closeDraft != "" {
		delete(b.drafts, closeDraft)
	}
	if createDraft {
		b.drafts[draftID] = make(map[int]struct{}, len(newDraftIndexes))
	}
	if rec.Type == RecordAssistantDraft {
		indexes := b.drafts[draftID]
		for _, index := range newDraftIndexes {
			indexes[index] = struct{}{}
		}
	}
	return nil
}

// replayMessageCounts streams a stored message's object so counting blocks
// does not allocate []RawMessage proportional to attacker-controlled array
// cardinality. Scalar strings are visited one at a time and remain bounded by
// the physical record limit.
func replayMessageCounts(raw []byte) (draftID string, blocks int, resultErr error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "", 0, errors.New("session message is not an object")
	}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return "", 0, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return "", 0, errors.New("session message has a non-string key")
		}
		switch key {
		case "draft_id":
			if err := decoder.Decode(&draftID); err != nil {
				return "", 0, err
			}
		case "content":
			open, err := decoder.Token()
			if err != nil || open != json.Delim('[') {
				return "", 0, errors.New("session message content is not an array")
			}
			for decoder.More() {
				if blocks == math.MaxInt {
					return "", 0, fmt.Errorf("%w: block accounting overflow", ErrSessionReplayTooLarge)
				}
				blocks++
				if err := skipReplayJSONValue(decoder); err != nil {
					return "", 0, err
				}
			}
			close, err := decoder.Token()
			if err != nil || close != json.Delim(']') {
				return "", 0, errors.New("session message content has an invalid ending")
			}
		default:
			if err := skipReplayJSONValue(decoder); err != nil {
				return "", 0, err
			}
		}
	}
	close, err := decoder.Token()
	if err != nil || close != json.Delim('}') {
		return "", 0, errors.New("session message has an invalid ending")
	}
	if token, err = decoder.Token(); !errors.Is(err, io.EOF) || token != nil {
		return "", 0, errors.New("session message has trailing JSON")
	}
	return draftID, blocks, nil
}

// replayDraftIndexes streams the event list and retains only distinct indexes
// not already counted for this logical draft. The slice cannot exceed the
// remaining cumulative block allowance; the first additional distinct index
// fails immediately without materializing the rest of the event array.
func replayDraftIndexes(raw []byte, existing map[int]struct{}, remaining int) ([]int, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("assistant draft is not an object")
	}
	introduced := make(map[int]struct{}, min(remaining, 256))
	var indexes []int
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("assistant draft has a non-string key")
		}
		if key != "events" {
			if err := skipReplayJSONValue(decoder); err != nil {
				return nil, err
			}
			continue
		}
		open, err := decoder.Token()
		if err != nil || open != json.Delim('[') {
			return nil, errors.New("assistant draft events is not an array")
		}
		for decoder.More() {
			index, err := replayDraftEventIndex(decoder)
			if err != nil {
				return nil, err
			}
			if _, present := existing[index]; present {
				continue
			}
			if _, present := introduced[index]; present {
				continue
			}
			if len(indexes) >= remaining {
				return nil, fmt.Errorf("%w: more than %d message blocks", ErrSessionReplayTooLarge, remaining)
			}
			introduced[index] = struct{}{}
			indexes = append(indexes, index)
		}
		close, err := decoder.Token()
		if err != nil || close != json.Delim(']') {
			return nil, errors.New("assistant draft events has an invalid ending")
		}
	}
	close, err := decoder.Token()
	if err != nil || close != json.Delim('}') {
		return nil, errors.New("assistant draft has an invalid ending")
	}
	return indexes, nil
}

func replayDraftEventIndex(decoder *json.Decoder) (int, error) {
	open, err := decoder.Token()
	if err != nil || open != json.Delim('{') {
		return 0, errors.New("assistant draft event is not an object")
	}
	index := 0
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return 0, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return 0, errors.New("assistant draft event has a non-string key")
		}
		if key == "index" {
			if err := decoder.Decode(&index); err != nil {
				return 0, err
			}
		} else if err := skipReplayJSONValue(decoder); err != nil {
			return 0, err
		}
	}
	close, err := decoder.Token()
	if err != nil || close != json.Delim('}') {
		return 0, errors.New("assistant draft event has an invalid ending")
	}
	return index, nil
}

func skipReplayJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	var close json.Delim
	switch delim {
	case '{':
		close = '}'
		for decoder.More() {
			if _, err := decoder.Token(); err != nil { // object key
				return err
			}
			if err := skipReplayJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		close = ']'
		for decoder.More() {
			if err := skipReplayJSONValue(decoder); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	ending, err := decoder.Token()
	if err != nil || ending != close {
		return errors.New("invalid JSON ending")
	}
	return nil
}

func replayCountAdd(current, delta int) (int, error) {
	if current < 0 || delta < 0 || current > math.MaxInt-delta {
		return 0, fmt.Errorf("%w: item accounting overflow", ErrSessionReplayTooLarge)
	}
	return current + delta, nil
}
