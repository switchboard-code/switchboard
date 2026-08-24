package session

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/switchboard-code/switchboard/internal/provider"
)

type RetryIntentStatus string

const (
	RetryIntentPending   RetryIntentStatus = "pending"
	RetryIntentStarted   RetryIntentStatus = "started"
	RetryIntentCompleted RetryIntentStatus = "completed"
)

var ErrRetryIntentUnresolved = errors.New("session has an unresolved retry handoff")

// RetryIntent is a bounded, non-secret replay coordinate. OpeningSHA256 binds
// the coordinate to the exact source message without copying prompt text,
// attachments, or credentials into a second durable record.
type RetryIntent struct {
	ID              string            `json:"id"`
	OwnerSessionID  string            `json:"owner_session_id"`
	SourceSessionID string            `json:"source_session_id"`
	OpeningMessage  int               `json:"opening_message"`
	OpeningSHA256   string            `json:"opening_sha256"`
	Tier            string            `json:"tier"`
	TierTarget      string            `json:"tier_target"`
	TierSetSHA256   string            `json:"tier_set_sha256"`
	Status          RetryIntentStatus `json:"status"`
}

func retryOpeningDigest(opening provider.Message) (string, error) {
	canonical := provider.CloneMessage(opening)
	// The child opening carries the publication-bound execution capability while
	// the source opening necessarily does not. It authenticates who appended the
	// child record, but is not part of the source-content digest.
	canonical.RetryIntentID = ""
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	if len(raw) == 0 || len(raw) > maxSessionRecordBytes {
		return "", fmt.Errorf("retry opening is empty or exceeds %d bytes", maxSessionRecordBytes)
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func validateRetryOpeningAppend(state State, opening provider.Message) error {
	intent := state.RetryIntent
	if intent == nil {
		if opening.RetryIntentID != "" {
			return errors.New("retry opening marker has no pending durable handoff")
		}
		return nil
	}
	if intent.Status != RetryIntentPending {
		return nil
	}
	if opening.RetryIntentID == "" {
		return errors.New("pending retry handoff refuses an unbound conversation message")
	}
	if opening.RetryIntentID != intent.ID || len(state.Messages) != intent.OpeningMessage ||
		opening.Role != provider.RoleUser || opening.Injected {
		return errors.New("retry opening marker does not match its pending durable handoff")
	}
	matches, err := RetryIntentOpeningMatches(*intent, opening)
	if err != nil || !matches {
		return errors.New("retry opening marker does not match its durable source digest")
	}
	return nil
}

// RetryIntentOpeningMatches verifies a reconstructed source opening without
// exposing the recorded digest comparison to timing-dependent string logic.
func RetryIntentOpeningMatches(intent RetryIntent, opening provider.Message) (bool, error) {
	digest, err := retryOpeningDigest(opening)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare([]byte(intent.OpeningSHA256), []byte(digest)) == 1, nil
}

// ReadRetrySourceOpening reconstructs the source coordinate without opening
// the log for append. In particular, it never migrates a schema, truncates a
// torn tail, or reconciles an interrupted tool call merely because startup is
// deciding whether an unpublished provider call is safe to resume.
func (s *Store) ReadRetrySourceOpening(sourceID, workspace string, openingMessage int) (provider.Message, error) {
	if openingMessage < 0 {
		return provider.Message{}, errors.New("retry source message coordinate is negative")
	}
	workspace, err := cleanAbsoluteWorkspace(workspace)
	if err != nil {
		return provider.Message{}, err
	}
	candidate, err := s.resolveCandidate(sourceID)
	if err != nil {
		return provider.Message{}, err
	}
	effective := effectiveWorkspace(candidate.start.Workspace, candidate.state.WorkspaceBinding)
	if !workspaceIdentityMatches(effective, workspace) {
		return provider.Message{}, fmt.Errorf("retry source %s belongs to workspace %q, not %q", sourceID, effective, workspace)
	}
	if candidate.state.ID != sourceID || openingMessage >= len(candidate.state.Messages) {
		return provider.Message{}, errors.New("retry source no longer contains its recorded opening coordinate")
	}
	return provider.CloneMessage(candidate.state.Messages[openingMessage]), nil
}

func validateRetryIntentShape(intent RetryIntent) error {
	if digest, err := hex.DecodeString(intent.ID); err != nil || len(digest) != sha256.Size {
		return errors.New("retry intent id is not a SHA-256 publication identity")
	}
	if !validSessionID(intent.SourceSessionID) {
		return errors.New("retry intent has an invalid source session id")
	}
	if !validSessionID(intent.OwnerSessionID) {
		return errors.New("retry intent has an invalid owner session id")
	}
	if intent.OpeningMessage < 0 {
		return errors.New("retry intent has a negative source message coordinate")
	}
	if digest, err := hex.DecodeString(intent.OpeningSHA256); err != nil || len(digest) != sha256.Size {
		return errors.New("retry intent opening digest is not SHA-256")
	}
	if intent.Tier == "" || len(intent.Tier) > 256 {
		return errors.New("retry intent tier is empty or over its bound")
	}
	if intent.TierTarget == "" || len(intent.TierTarget) > 1024 {
		return errors.New("retry intent tier target is empty or over its bound")
	}
	parsedTarget, err := provider.ParseRouteTargetID(provider.RouteTargetID(intent.TierTarget))
	if err != nil || string(parsedTarget.ID()) != intent.TierTarget {
		return errors.New("retry intent tier target is not a canonical route target id")
	}
	if digest, err := hex.DecodeString(intent.TierSetSHA256); err != nil || len(digest) != sha256.Size {
		return errors.New("retry intent tier-set digest is not SHA-256")
	}
	switch intent.Status {
	case RetryIntentPending, RetryIntentStarted, RetryIntentCompleted:
		return nil
	default:
		return fmt.Errorf("retry intent has unknown status %q", intent.Status)
	}
}

func sameRetryIntentIdentity(a, b RetryIntent) bool {
	return a.ID == b.ID && a.OwnerSessionID == b.OwnerSessionID && a.SourceSessionID == b.SourceSessionID &&
		a.OpeningMessage == b.OpeningMessage && a.OpeningSHA256 == b.OpeningSHA256 && a.Tier == b.Tier &&
		a.TierTarget == b.TierTarget && a.TierSetSHA256 == b.TierSetSHA256
}

func (state *State) applyRetryIntent(intent RetryIntent) error {
	if err := validateRetryIntentShape(intent); err != nil {
		return err
	}
	// Same-schema older binaries are allowed to ignore an unknown record and
	// may therefore carry it through a fork. Its explicit physical-log owner
	// makes that carried record inert padding instead of corrupting the child or
	// granting it execution authority.
	if intent.OwnerSessionID != state.ID {
		return nil
	}
	if state.ID == "" || intent.ID != publicationRecoveryIdentity(state.ID, state.publicationID) {
		return errors.New("retry intent does not belong to this staged child publication")
	}
	if intent.SourceSessionID == state.ID {
		return errors.New("retry intent source is the child itself")
	}
	switch intent.Status {
	case RetryIntentPending:
		if state.RetryIntent != nil || state.retryIntentSeen {
			return errors.New("retry intent pending record replaces an existing handoff")
		}
		copy := intent
		state.RetryIntent = &copy
		state.retryIntentSeen = true
	case RetryIntentStarted:
		if state.RetryIntent == nil || state.RetryIntent.Status != RetryIntentPending ||
			!sameRetryIntentIdentity(*state.RetryIntent, intent) {
			return errors.New("retry intent started without its exact pending handoff")
		}
		copy := intent
		state.RetryIntent = &copy
	case RetryIntentCompleted:
		if state.RetryIntent == nil ||
			(state.RetryIntent.Status != RetryIntentPending && state.RetryIntent.Status != RetryIntentStarted) ||
			!sameRetryIntentIdentity(*state.RetryIntent, intent) {
			return errors.New("retry intent completed without its exact unresolved handoff")
		}
		state.RetryIntent = nil
	}
	return nil
}

// AppendRetryIntent records the exact replay coordinate before a staged child
// may become visible. The source opening itself remains in its original log.
func (s *Session) AppendRetryIntent(sourceSessionID string, openingMessage int, opening provider.Message, tier, tierTarget, tierSetSHA256 string) (RetryIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.f == nil || !s.state.publicationPending ||
		s.publicationOwner == "" || s.publicationOwner != s.state.publicationID {
		return RetryIntent{}, ErrPublicationOwnership
	}
	if openingMessage != len(s.state.Messages) {
		return RetryIntent{}, fmt.Errorf("retry source opening %d does not match the child cut at %d messages", openingMessage, len(s.state.Messages))
	}
	if opening.Role != provider.RoleUser || opening.Injected {
		return RetryIntent{}, errors.New("retry intent opening is not an authored user message")
	}
	if _, known := opening.AuthoredProjection(); !known {
		return RetryIntent{}, errors.New("retry intent opening has no exact authored projection")
	}
	digest, err := retryOpeningDigest(opening)
	if err != nil {
		return RetryIntent{}, err
	}
	intent := RetryIntent{
		ID:              publicationRecoveryIdentity(s.state.ID, s.state.publicationID),
		OwnerSessionID:  s.state.ID,
		SourceSessionID: sourceSessionID,
		OpeningMessage:  openingMessage,
		OpeningSHA256:   digest,
		Tier:            tier,
		TierTarget:      tierTarget,
		TierSetSHA256:   tierSetSHA256,
		Status:          RetryIntentPending,
	}
	if err := validateRetryIntentShape(intent); err != nil {
		return RetryIntent{}, err
	}
	if s.state.RetryIntent != nil {
		return RetryIntent{}, errors.New("staged child already has a retry intent")
	}
	nextState := s.state
	if err := nextState.applyRetryIntent(intent); err != nil {
		return RetryIntent{}, err
	}
	if err := s.append(RecordRetryIntent, intent); err != nil {
		return RetryIntent{}, err
	}
	s.state.RetryIntent = nextState.RetryIntent
	s.state.retryIntentSeen = nextState.retryIntentSeen
	return intent, nil
}

func (s *Session) transitionRetryIntent(id string, from, to RetryIntentStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.f == nil {
		return errors.New("cannot update retry intent on a closed session")
	}
	if to == RetryIntentStarted && s.state.publicationPending {
		return errors.New("cannot start retry execution before the staged child is published")
	}
	if s.state.RetryIntent == nil || s.state.RetryIntent.ID != id || s.state.RetryIntent.Status != from {
		return fmt.Errorf("retry intent %s is not %s", id, from)
	}
	if to == RetryIntentStarted {
		intent := s.state.RetryIntent
		if intent.OpeningMessage < 0 || len(s.state.Messages) != intent.OpeningMessage+1 {
			return errors.New("cannot start retry execution without its exact recorded opening")
		}
		opening := s.state.Messages[intent.OpeningMessage]
		if opening.RetryIntentID != id {
			return errors.New("cannot start retry execution without its publication-bound opening marker")
		}
		matches, err := RetryIntentOpeningMatches(*intent, opening)
		if err != nil || !matches {
			return errors.New("cannot start retry execution with a mismatched recorded opening")
		}
	}
	next := *s.state.RetryIntent
	next.Status = to
	if err := s.append(RecordRetryIntent, next); err != nil {
		return err
	}
	return s.state.applyRetryIntent(next)
}

func (s *Session) StartRetryIntent(id string) error {
	return s.transitionRetryIntent(id, RetryIntentPending, RetryIntentStarted)
}

func (s *Session) CompleteRetryIntent(id string) error {
	return s.transitionRetryIntent(id, RetryIntentStarted, RetryIntentCompleted)
}

// AbandonRetryIntent is the explicit acknowledgement for an interrupted
// handoff. It never starts or repeats work; it only clears the recovery guard
// after the user has chosen to keep the child as it stands.
func (s *Session) AbandonRetryIntent(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.f == nil {
		return errors.New("cannot abandon retry intent on a closed session")
	}
	if s.state.RetryIntent == nil || s.state.RetryIntent.ID != id {
		return fmt.Errorf("retry intent %s is not active", id)
	}
	next := *s.state.RetryIntent
	next.Status = RetryIntentCompleted
	if err := s.append(RecordRetryIntent, next); err != nil {
		return err
	}
	return s.state.applyRetryIntent(next)
}
