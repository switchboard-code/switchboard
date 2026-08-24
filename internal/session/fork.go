package session

// Fork branches a session at a turn into a new session sharing history up to
// that point (§12). The copy is the whole mechanism: the source log is read
// and never written, so the original stays exactly what it was, and the
// fork's messages are byte-identical to the source's prefix — which means a
// provider holding that prefix warm serves the fork warm too. This is the
// cache-honest answer to rewinding a conversation: not rewriting sent
// messages (the append-only rule in §6.1), but branching a new log that
// stops where you wish the old one had.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/switchboard-code/switchboard/internal/continuity"
	"github.com/switchboard-code/switchboard/internal/provider"
)

type forkAssistantDraft struct {
	included bool
	closed   bool
}

// Fork copies a session's first keepMessages conversation messages, with the
// usage and audit records that accompany them, into a new session. The cut
// must land on a turn boundary: when messages are dropped, the first dropped
// message has to be the user message that opened its turn, because a
// conversation cut mid-turn leaves tool calls without results and every
// request built from it would be malformed (§10.3).
//
// The source is read without the append lock, so a session open in this
// process — the usual case — forks from its durable prefix.
func (s *Store) Fork(id string, keepMessages int) (*Session, error) {
	return s.forkOnto(id, keepMessages, "", false, true)
}

// ForkOnto is Fork with the new log started against a different target,
// which is what a /race arm needs: the branch shares the source's messages
// but runs its turn on the rung being raced, and a session's start record
// has to name the target that actually served it, because /resume binds
// from that record. An empty target keeps the source's.
func (s *Store) ForkOnto(id string, keepMessages int, target provider.RouteTargetID) (*Session, error) {
	return s.forkOnto(id, keepMessages, target, false, true)
}

// ForkForRetry is the one branch operation allowed to keep zero messages. A
// first-turn retry needs a fresh conversation prefix but must still inherit
// every budget charge and retry reserve from the set-aside attempt; creating a
// blank session would make repeated first-turn retries erase ceiling spend.
func (s *Store) ForkForRetry(id string, keepMessages int) (*Session, error) {
	return s.forkOnto(id, keepMessages, "", true, true)
}

// ForkAccountingOnto creates an empty conversation branch on another target
// while carrying the source's full observed cost and retry reserve. Race arms
// use it when the origin has accounting lineage but no messages to copy.
func (s *Store) ForkAccountingOnto(id string, target provider.RouteTargetID) (*Session, error) {
	return s.forkOnto(id, 0, target, true, true)
}

// ForkSession is Fork against a live source. Holding the source append lock
// from the first byte read through the accounting reconciliation makes the
// branch a single durable snapshot: an asynchronous metered call can be
// wholly before it or wholly after it, never disappear between EOF and the
// later state read.
func (s *Store) ForkSession(source *Session, keepMessages int) (*Session, error) {
	return s.forkSessionOnto(source, keepMessages, "", false, true)
}

// ForkSessionOnto is the live-source form of ForkOnto.
func (s *Store) ForkSessionOnto(source *Session, keepMessages int, target provider.RouteTargetID) (*Session, error) {
	return s.forkSessionOnto(source, keepMessages, target, false, true)
}

// ForkSessionForRetry is the live-source form of ForkForRetry.
func (s *Store) ForkSessionForRetry(source *Session, keepMessages int) (*Session, error) {
	return s.forkSessionOnto(source, keepMessages, "", true, true)
}

// ForkSessionAccountingOnto is the live-source form of ForkAccountingOnto.
func (s *Store) ForkSessionAccountingOnto(source *Session, target provider.RouteTargetID) (*Session, error) {
	return s.forkSessionOnto(source, 0, target, true, true)
}

// ForkSessionStaged builds a live-source fork without publishing it. The
// caller must Publish only after its runtime adoption succeeds, or call
// CloseDiscardingStaged on every failed, cancelled, or stale path.
func (s *Store) ForkSessionStaged(source *Session, keepMessages int) (*Session, error) {
	return s.forkSessionOnto(source, keepMessages, "", false, false)
}

// ForkSessionOntoStaged is the staged live-source form of ForkOnto.
func (s *Store) ForkSessionOntoStaged(source *Session, keepMessages int, target provider.RouteTargetID) (*Session, error) {
	return s.forkSessionOnto(source, keepMessages, target, false, false)
}

// ForkSessionForRetryStaged is the staged live-source form of ForkForRetry.
func (s *Store) ForkSessionForRetryStaged(source *Session, keepMessages int) (*Session, error) {
	return s.forkSessionOnto(source, keepMessages, "", true, false)
}

// ForkSessionAccountingOntoStaged creates a staged empty accounting branch.
func (s *Store) ForkSessionAccountingOntoStaged(source *Session, target provider.RouteTargetID) (*Session, error) {
	return s.forkSessionOnto(source, 0, target, true, false)
}

func (s *Store) forkSessionOnto(source *Session, keepMessages int, target provider.RouteTargetID, allowEmpty, publish bool) (*Session, error) {
	if source == nil {
		return nil, fmt.Errorf("cannot fork a nil session")
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.state.publicationPending {
		return nil, fmt.Errorf("%w: source session %s", ErrSessionUnpublished, source.state.ID)
	}
	if source.state.RetryIntent != nil {
		return nil, fmt.Errorf("%w: source session %s", ErrRetryIntentUnresolved, source.state.ID)
	}
	if source.state.raceBranchPending {
		return nil, fmt.Errorf("%w: origin session %s", ErrRaceBranchPending, source.state.raceBranchOrigin)
	}
	if err := verifyCurrentSessionLogPath(source.f, source.path); err != nil {
		return nil, fmt.Errorf("verifying live fork source: %w", err)
	}
	sourceInfo, err := source.f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat live fork source: %w", err)
	}
	state := source.state
	state.Messages = provider.CloneMessages(source.state.Messages)
	return s.forkPathOnto(source.state.ID, source.path, keepMessages, target, allowEmpty, &state, sourceInfo, publish)
}

func (s *Store) forkOnto(id string, keepMessages int, target provider.RouteTargetID, allowEmpty, publish bool) (*Session, error) {
	if keepMessages < 0 || (keepMessages == 0 && !allowEmpty) {
		return nil, fmt.Errorf("a fork keeping no messages is an empty session; /clear is how those start")
	}
	candidate, err := s.resolveCandidate(id)
	if err != nil {
		return nil, err
	}
	return s.forkPathOnto(id, candidate.path, keepMessages, target, allowEmpty, nil, nil, publish)
}

func (s *Store) forkPathOnto(id, path string, keepMessages int, target provider.RouteTargetID, allowEmpty bool, sourceState *State, sourceInfo os.FileInfo, publish bool) (*Session, error) {
	if keepMessages < 0 || (keepMessages == 0 && !allowEmpty) {
		return nil, fmt.Errorf("a fork keeping no messages is an empty session; /clear is how those start")
	}
	f, err := openPublishedLog(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if sourceInfo != nil {
		opened, err := f.Stat()
		if err != nil {
			return nil, err
		}
		if !os.SameFile(sourceInfo, opened) {
			return nil, fmt.Errorf("live fork source %s changed before its snapshot", path)
		}
	}
	expect := candidateExpectation{id: id}
	if sourceState != nil {
		expect.workspace = effectiveWorkspace(sourceState.Workspace, sourceState.WorkspaceBinding)
	}
	r := bufio.NewReader(f)

	_, _, err = readSessionHeader(r, path)
	if err != nil {
		return nil, err
	}

	explicitRetarget := target != ""
	var start SessionStart
	startSeen := false
	sourceReplay := &Session{}
	var kept []Record
	messages := 0
	pastCut := false
	drafts := make(map[string]forkAssistantDraft)
	raceBranchPending := false
	lastSeq := 0
	budget := newReplayBudget(s.replayLimit, len(magic)+4)
	for {
		rec, _, err := budget.decode(r, &lastSeq)
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, errTornFinalRecord) {
			if !startSeen {
				return nil, fmt.Errorf("%s has a torn or corrupt first session_start record", path)
			}
			// An incomplete final append is what replay would have truncated;
			// the fork carries the valid prefix, same as a resume would.
			break
		}
		if errors.Is(err, ErrCorruptRecord) {
			return nil, fmt.Errorf("%s contains a complete corrupt record; refusing to fork and preserving the source: %w", path, err)
		}
		if err != nil {
			return nil, err
		}
		if !startSeen {
			start, err = s.validateFirstStart(path, rec, expect)
			if err != nil {
				return nil, err
			}
			if err := validatePublishedMarker(path, start); err != nil {
				return nil, err
			}
			if err := verifyCurrentSessionLogPath(f, path); err != nil {
				return nil, err
			}
			if err := sourceReplay.apply(rec); err != nil {
				return nil, fmt.Errorf("%s has an invalid first session_start: %w", path, err)
			}
			startSeen = true
			continue
		}
		if rec.Type == RecordSessionStart {
			return nil, fmt.Errorf("%s has a duplicate session_start at record %d", path, rec.Seq)
		}
		if err := sourceReplay.apply(rec); err != nil {
			return nil, fmt.Errorf("%s record %d is invalid: %w", path, rec.Seq, err)
		}
		if rec.Type == RecordRetryIntent {
			// The handoff belongs to one physical child and is bound to that
			// child's publication capability. Forks may use the fully replayed
			// source for validation, but must never copy its execution authority.
			continue
		}
		if rec.Type == RecordRaceBranch {
			var branch RaceBranch
			if err := json.Unmarshal(rec.Payload, &branch); err != nil {
				return nil, err
			}
			raceBranchPending = !branch.Finalized
			// Branch lifecycle belongs to this physical log. Copying a finalized
			// marker makes the child look like the old race branch and prevents it
			// from participating in a later independent race.
			continue
		}
		if explicitRetarget && rec.Type == RecordRuntimeBinding {
			// An Onto fork's SessionStart names its new target. Carrying the
			// source's moving binding would overwrite that target during replay
			// and can also inherit a user pin into an automatic race arm. An audit
			// note embedded in that frame remains ordinary history, though, so
			// preserve it without the stale binding when it falls before the cut.
			var binding RuntimeBinding
			if err := json.Unmarshal(rec.Payload, &binding); err != nil {
				return nil, err
			}
			if !pastCut && binding.Note != nil {
				payload, err := json.Marshal(*binding.Note)
				if err != nil {
					return nil, err
				}
				rec.Type = RecordNote
				rec.Payload = payload
				kept = append(kept, rec)
			}
			continue
		}
		if rec.Type == RecordContinuity {
			capsule, err := continuity.DecodeStored(rec.Payload)
			if err != nil {
				return nil, err
			}
			// Keep every capsule derived from the exact prefix. This includes a
			// basis-zero capsule referenced by a first opening: retry must replay
			// that opening byte-for-byte, and its durable reference is valid only
			// while the same capsule remains current in the fork.
			if pastCut || capsule.BasisMessages > keepMessages {
				continue
			}
			kept = append(kept, rec)
			continue
		}
		if rec.Type == RecordAssistantDraft {
			checkpoint, err := decodeAssistantDraft(rec.Payload)
			if err != nil {
				return nil, err
			}
			draft, seen := drafts[checkpoint.ID]
			if seen {
				if draft.closed {
					return nil, fmt.Errorf("assistant draft %q continues after finalization", checkpoint.ID)
				}
				if draft.included {
					kept = append(kept, rec)
				}
				continue
			}
			if !pastCut && messages == keepMessages {
				return nil, fmt.Errorf(
					"the cut falls inside a turn: message %d is an assistant draft, and a turn is dropped whole or kept whole",
					keepMessages)
			}
			draft.included = !pastCut
			drafts[checkpoint.ID] = draft
			if draft.included {
				messages++
				kept = append(kept, rec)
			}
			continue
		}
		if message, isMessage, err := conversationMessage(rec); err != nil {
			return nil, err
		} else if isMessage {
			if message.DraftID != "" {
				draft, seen := drafts[message.DraftID]
				if !seen || draft.closed {
					return nil, fmt.Errorf("assistant draft finalization %q has no active checkpoint", message.DraftID)
				}
				if draft.included {
					kept = append(kept, rec)
				}
				draft.closed = true
				drafts[message.DraftID] = draft
				continue
			}
			if !pastCut && messages == keepMessages {
				if !OpensTurn(message) {
					return nil, fmt.Errorf(
						"the cut falls inside a turn: message %d does not open a user turn, and a turn is dropped whole or kept whole",
						keepMessages)
				}
				pastCut = true
			}
			if !pastCut {
				messages++
				if rec.Type == RecordMessage && message.RetryIntentID != "" {
					// A retry-opening capability belongs to one physical child just
					// like the retry_intent records filtered above. It is not provider
					// wire data, so stripping it preserves the conversation exactly
					// while preventing a derived log from claiming the old handoff.
					message.RetryIntentID = ""
					payload, marshalErr := json.Marshal(message)
					if marshalErr != nil {
						return nil, marshalErr
					}
					rec.Payload = payload
				}
				kept = append(kept, rec)
			}
			continue
		}
		if !pastCut || carriesBudgetAccounting(rec.Type) {
			kept = append(kept, rec)
		}
	}
	if start.ID == "" {
		return nil, fmt.Errorf("session %s has no start record", id)
	}
	if raceBranchPending {
		return nil, fmt.Errorf("%w: session %s", ErrRaceBranchPending, id)
	}
	if sourceReplay.state.RetryIntent != nil {
		return nil, fmt.Errorf("%w: source session %s", ErrRetryIntentUnresolved, id)
	}
	if err := validateWorkspaceExpectation(path, start.Workspace, sourceReplay.state.WorkspaceBinding, expect.workspace); err != nil {
		return nil, err
	}
	if messages < keepMessages {
		return nil, fmt.Errorf("session %s holds %d messages, cannot keep %d", id, messages, keepMessages)
	}
	// Publication and pathname ownership are rechecked after the payload walk,
	// before any child is created. The bytes above came from f throughout; a
	// replacement can neither feed the fork nor leave a child from a stale path.
	if err := validatePublishedMarker(path, start); err != nil {
		return nil, err
	}
	if err := verifyCurrentSessionLogPath(f, path); err != nil {
		return nil, err
	}

	if target == "" {
		target = provider.RouteTargetID(start.Target)
	}
	forkWorkspace := effectiveWorkspace(start.Workspace, sourceReplay.state.WorkspaceBinding)
	fork, err := s.CreateStaged(forkWorkspace, target, start.CatalogRevision)
	if err != nil {
		return nil, err
	}
	for _, rec := range kept {
		if err := fork.appendCopied(rec); err != nil {
			_ = fork.CloseDiscardingStaged()
			return nil, err
		}
	}
	if allowEmpty {
		// Retry discards conversation and its Usage records, not the bill for
		// requests already sent. Carry the dropped observed cost as an external
		// charge so repeated retries cannot reset a hard ceiling, while keeping
		// token/call telemetry honest for the new branch.
		var snapshot State
		if sourceState != nil {
			snapshot = *sourceState
		} else {
			snapshot = sourceReplay.state
		}
		droppedCost := snapshot.AccountedCostMicroUSD() - fork.State().AccountedCostMicroUSD()
		if droppedCost > 0 {
			if err := fork.AppendBudgetTransfer("retry:"+id, droppedCost, 0); err != nil {
				_ = fork.CloseDiscardingStaged()
				return nil, err
			}
		}
	}
	// Provenance rides the log, so an exported or audited fork names where
	// its history came from.
	if err := fork.AppendNote("info", fmt.Sprintf("forked from %s, keeping %d messages", id, keepMessages)); err != nil {
		_ = fork.CloseDiscardingStaged()
		return nil, err
	}
	// A concurrent live-source fork or a full-tip ID fork can legitimately copy
	// the durable assistant call before its result. The child is an adoption
	// boundary of its own: close that tail there and never mutate or reexecute
	// against the source, which may still finish the real call.
	if _, err := fork.ReconcileInterruptedToolCalls(); err != nil {
		_ = fork.CloseDiscardingStaged()
		return nil, fmt.Errorf("recovering interrupted tool calls in fork: %w", err)
	}
	if publish {
		if err := publishForkDurably(fork); err != nil {
			return nil, err
		}
	}
	return fork, nil
}

func publishForkDurably(fork *Session) error {
	outcome, err := fork.PublishDurably()
	if !outcome.Visible {
		if err == nil {
			err = errors.New("fork publication returned no visible commit")
		}
		err = errors.Join(err, fork.CloseDiscardingStaged())
		return fmt.Errorf("publishing fork: %w", err)
	}
	if !outcome.Durable {
		if err == nil {
			err = errors.New("fork publication became visible before its durability was proven")
		}
		// Visibility commits this child. Closing releases the descriptor but
		// deliberately leaves the log and marker in place; returning an error
		// stops the caller from doing more work until restart can rediscover it.
		id := fork.ID()
		err = errors.Join(err, fork.Close())
		return fmt.Errorf("fork %s became visible, but publication durability could not be confirmed; restart Switchboard before continuing: %w", id, err)
	}
	return nil
}

// carriesBudgetAccounting names records that survive a conversation rewind.
// A retry can discard messages and tool work, but it cannot un-send provider
// requests or un-spend delegated/raced calls already admitted by this ledger.
func carriesBudgetAccounting(t RecordType) bool {
	switch t {
	case RecordRetryReserve, RecordBudgetAttempt, RecordBudgetSettle, RecordBudgetTransfer:
		return true
	default:
		return false
	}
}

// appendCopied writes a record carried over by a fork, keeping the source's
// timestamp: the moment the turn happened is a fact about the turn, not
// about the copy.
func (s *Session) appendCopied(rec Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned != nil {
		return fmt.Errorf("%w: %v", ErrSessionPoisoned, s.poisoned)
	}
	nextSeq, err := s.nextRecordSequence()
	if err != nil {
		return err
	}
	copied := Record{Seq: nextSeq, At: rec.At, Type: rec.Type, Payload: rec.Payload}
	frame, err := encodeRecord(copied)
	if err != nil {
		return err
	}
	if err := s.writeFrame(frame, nextSeq); err != nil {
		return fmt.Errorf("appending to forked log: %w", err)
	}
	if err := s.f.Sync(); err != nil {
		s.poisoned = fmt.Errorf("syncing copied record %d: %w", nextSeq, err)
		return fmt.Errorf("%w: %v", ErrSessionPoisoned, s.poisoned)
	}
	return s.apply(copied)
}
