package session

// Read-only replay, for surfaces that summarize sessions rather than append
// to them. `sb cost` reads every log a workspace has recorded, and taking
// the append lock for that would make the summary fail whenever a session
// is open — which is exactly when someone asks what things are costing.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/switchboard-code/switchboard/internal/provider"
)

const maxSessionHeaderBytes = 64

// ErrSessionHeaderTooLarge refuses a log before an attacker-controlled first
// line can grow an unbounded ReadString buffer.
var ErrSessionHeaderTooLarge = errors.New("session header exceeds size limit")

func readSessionHeader(r *bufio.Reader, path string) (string, int, error) {
	header := make([]byte, 0, len(magic)+4)
	for len(header) < maxSessionHeaderBytes {
		b, err := r.ReadByte()
		if err != nil {
			return "", 0, fmt.Errorf("reading session header: %w", err)
		}
		header = append(header, b)
		if b != '\n' {
			continue
		}

		var gotMagic string
		var version int
		line := string(header)
		if _, err := fmt.Sscanf(strings.TrimSpace(line), "%s %d", &gotMagic, &version); err != nil || gotMagic != magic {
			return "", 0, fmt.Errorf("%s is not a switchboard session log", path)
		}
		if version > SchemaVersion {
			return "", 0, fmt.Errorf("%w: log is schema %d, this binary understands %d", ErrSchemaTooNew, version, SchemaVersion)
		}
		if version < 1 {
			return "", 0, fmt.Errorf("%s uses unsupported session schema %d; the oldest supported schema is 1", path, version)
		}
		return line, version, nil
	}
	return "", 0, fmt.Errorf("%w: %s has no newline within %d bytes", ErrSessionHeaderTooLarge, path, maxSessionHeaderBytes)
}

// checkHeader validates a log's magic line and schema before any records are
// read, the same refusal an appending open makes.
func checkHeader(r *bufio.Reader, path string) error {
	_, _, err := readSessionHeader(r, path)
	return err
}

// ReadState replays a log without opening it for appending. An incomplete
// final frame ends the replay where an appending open would have truncated it,
// but the file is left alone: a reader repairs nothing. Complete corrupt
// frames are errors because silently returning their prefix would hide loss.
func ReadState(path string) (State, error) {
	return readStateWithLimits(path, defaultReplayLimits)
}

func readStateWithLimits(path string, limits replayLimits) (State, error) {
	f, err := openPublishedLog(path)
	if err != nil {
		return State{}, err
	}
	defer f.Close()
	r := bufio.NewReader(f)

	header, _, err := readSessionHeader(r, path)
	if err != nil {
		return State{}, err
	}

	replay := &Session{}
	var start SessionStart
	startSeen := false
	lastSeq := 0
	budget := newReplayBudget(limits, len(header))
	for {
		rec, _, err := budget.decode(r, &lastSeq)
		if isRecoverableRecordEnd(err) {
			break
		}
		if err != nil {
			return State{}, err
		}
		if rec.Type == RecordSessionStart {
			if startSeen {
				return State{}, fmt.Errorf("%s has a duplicate session_start at record %d", path, rec.Seq)
			}
			if rec.Seq != 1 {
				return State{}, fmt.Errorf("%s first session_start has sequence %d, want 1", path, rec.Seq)
			}
			if err := json.Unmarshal(rec.Payload, &start); err != nil {
				return State{}, fmt.Errorf("%s has an invalid session_start: %w", path, err)
			}
			startSeen = true
		} else if !startSeen {
			return State{}, fmt.Errorf("%s first record is %s, want session_start", path, rec.Type)
		}
		if err := replay.apply(rec); err != nil {
			return State{}, err
		}
	}
	if !startSeen {
		return State{}, fmt.Errorf("%s has no session_start", path)
	}
	replay.state.publicationPending = false
	return replay.State(), nil
}

// ReadRaces collects a log's race verdicts, read-only, same posture as
// ReadState: `sb races` sums the paired evidence a workspace has gathered,
// and holding the append lock for a summary would make the question
// unanswerable exactly while a session is racing.
func ReadRaces(path string) ([]Race, error) {
	f, err := openPublishedLog(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)

	if err := checkHeader(r, path); err != nil {
		return nil, err
	}

	var out []Race
	lastSeq := 0
	budget := newReplayBudget(defaultReplayLimits, len(magic)+4)
	for {
		rec, _, err := budget.decode(r, &lastSeq)
		if isRecoverableRecordEnd(err) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if rec.Type != RecordRace {
			continue
		}
		var race Race
		if err := json.Unmarshal(rec.Payload, &race); err != nil {
			return nil, err
		}
		out = append(out, race)
	}
}

// ReadPermissions returns the durable resolved permission audit in record
// order. Older records decode with an empty ResolvedBy; consumers can display
// them as legacy policy decisions without inventing whether the user approved.
func ReadPermissions(path string) ([]Permission, error) {
	f, err := openPublishedLog(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	if err := checkHeader(r, path); err != nil {
		return nil, err
	}

	var out []Permission
	lastSeq := 0
	budget := newReplayBudget(defaultReplayLimits, len(magic)+4)
	for {
		rec, _, err := budget.decode(r, &lastSeq)
		if isRecoverableRecordEnd(err) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if rec.Type != RecordPermission {
			continue
		}
		var permission Permission
		if err := json.Unmarshal(rec.Payload, &permission); err != nil {
			return nil, err
		}
		out = append(out, permission)
	}
}

// Timeline is one of a log's records in the order it was written, shaped
// for a surface replaying the session as a document rather than as a
// request: exactly one of the payload fields is set. Usage, pins, and
// permissions are deliberately absent — they are accounting, and State
// already sums them.
type Timeline struct {
	// At is the record's own timestamp. A fork's copy keeps its source's,
	// which is how an aggregate reader tells a copied record from a second
	// real one — the same mechanism Usage.At carries.
	At time.Time

	Message *provider.Message
	Route   *Route
	Race    *Race
	Note    *Note
}

// ReadTimeline collects a log's conversation and the decisions that rode
// beside it, in order, read-only. An export built from Messages alone
// would drop the routing record — the half of the session no transcript
// of the words can reconstruct.
func ReadTimeline(path string) ([]Timeline, error) {
	f, err := openPublishedLog(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)

	if err := checkHeader(r, path); err != nil {
		return nil, err
	}

	var out []Timeline
	replay := &Session{}
	draftTimeline := make(map[string]int)
	lastSeq := 0
	budget := newReplayBudget(defaultReplayLimits, len(magic)+4)
	for {
		rec, _, err := budget.decode(r, &lastSeq)
		if isRecoverableRecordEnd(err) {
			// An incomplete draft has no ordinary final message. Materialize each
			// surviving logical row once at the read boundary rather than rebuilding
			// its complete prefix for every physical delta checkpoint.
			for id, index := range draftTimeline {
				draft := replay.assistantDrafts[id]
				if draft == nil {
					return nil, fmt.Errorf("assistant draft %q has no replay state", id)
				}
				message := replay.materializeAssistantDraft(draft)
				out[index].Message = &message
			}
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if rec.Type == RecordAssistantDraft {
			checkpoint, err := decodeAssistantDraft(rec.Payload)
			if err != nil {
				return nil, err
			}
			if err := replay.apply(rec); err != nil {
				return nil, err
			}
			if index, exists := draftTimeline[checkpoint.ID]; exists {
				out[index].At = rec.At
			} else {
				draftTimeline[checkpoint.ID] = len(out)
				out = append(out, Timeline{At: rec.At})
			}
			continue
		}
		if message, ok, err := conversationMessage(rec); err != nil {
			return nil, err
		} else if ok {
			if message.DraftID != "" {
				draft := replay.assistantDrafts[message.DraftID]
				index, exists := draftTimeline[message.DraftID]
				if draft == nil || !exists {
					return nil, fmt.Errorf("assistant draft finalization %q has no timeline checkpoint", message.DraftID)
				}
				messageIndex := draft.messageIndex
				if err := replay.apply(rec); err != nil {
					return nil, err
				}
				message = provider.CloneMessage(replay.state.Messages[messageIndex])
				out[index] = Timeline{At: rec.At, Message: &message}
				delete(draftTimeline, message.DraftID)
				continue
			}
			if err := replay.apply(rec); err != nil {
				return nil, err
			}
			message = provider.CloneMessage(replay.state.Messages[len(replay.state.Messages)-1])
			out = append(out, Timeline{At: rec.At, Message: &message})
			continue
		}
		switch rec.Type {
		case RecordSessionStart, RecordContinuity:
			if err := replay.apply(rec); err != nil {
				return nil, err
			}
		case RecordRuntimeBinding:
			var binding RuntimeBinding
			if err := json.Unmarshal(rec.Payload, &binding); err != nil {
				return nil, err
			}
			if err := replay.apply(rec); err != nil {
				return nil, err
			}
			if binding.Note != nil {
				note := *binding.Note
				out = append(out, Timeline{At: rec.At, Note: &note})
			}
		case RecordRoute:
			var route Route
			if err := json.Unmarshal(rec.Payload, &route); err != nil {
				return nil, err
			}
			out = append(out, Timeline{At: rec.At, Route: &route})
		case RecordRace:
			var race Race
			if err := json.Unmarshal(rec.Payload, &race); err != nil {
				return nil, err
			}
			out = append(out, Timeline{At: rec.At, Race: &race})
		case RecordNote:
			var note Note
			if err := json.Unmarshal(rec.Payload, &note); err != nil {
				return nil, err
			}
			out = append(out, Timeline{At: rec.At, Note: &note})
		}
	}
}

// ReadUsages collects a log's per-call usage records, read-only. The replayed
// State sums them, and a sum is the wrong shape for counterfactual pricing:
// catalog prices are banded by the size of one call, so repricing a session on
// another rung has to see each call, not the total the calls added up to.
func ReadUsages(path string) ([]Usage, error) {
	f, err := openPublishedLog(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)

	if err := checkHeader(r, path); err != nil {
		return nil, err
	}

	var out []Usage
	lastSeq := 0
	budget := newReplayBudget(defaultReplayLimits, len(magic)+4)
	for {
		rec, _, err := budget.decode(r, &lastSeq)
		if isRecoverableRecordEnd(err) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if rec.Type != RecordUsage {
			continue
		}
		var u Usage
		if err := json.Unmarshal(rec.Payload, &u); err != nil {
			return nil, err
		}
		// The record's own timestamp rides along, runtime-only: a fork
		// copies usage records with their At intact, and the timestamp is
		// how an aggregate reader tells the copy from a second real call.
		u.At = rec.At
		if u.CallID == "" {
			u.CallID = legacyAccountingID("call", rec)
		}
		out = append(out, u)
	}
}

// ReadWorkspace returns the workspace a log's own header records, reading
// only as far as the session_start record: the store's directory names are
// hashes, so the log is the one place the path survives.
func ReadWorkspace(path string) (string, error) {
	f, err := openPublishedLog(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	if err := checkHeader(r, path); err != nil {
		return "", err
	}
	lastSeq := 0
	budget := newReplayBudget(defaultReplayLimits, len(magic)+4)
	for {
		rec, _, err := budget.decode(r, &lastSeq)
		if err != nil {
			return "", err
		}
		if rec.Type != RecordSessionStart {
			continue
		}
		var start SessionStart
		if err := json.Unmarshal(rec.Payload, &start); err != nil {
			return "", err
		}
		return start.Workspace, nil
	}
}

// ReadOpening returns the first verified authored words in a session. Legacy
// records did not preserve the boundary between user text and provider-visible
// expansion, so their wording is withheld rather than guessed from Content.
// Presentation surfaces that need to distinguish that case from an empty log
// use ReadOpeningSummary.
func ReadOpening(path string) (string, error) {
	opening, err := ReadOpeningSummary(path)
	if err != nil {
		return "", err
	}
	if !opening.AuthoredKnown || opening.Synthetic {
		return "", nil
	}
	return opening.Text, nil
}
