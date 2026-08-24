package session

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/switchboard-code/switchboard/internal/provider"
)

// assistantDraftRecord is an incremental WAL checkpoint, not a conversation
// message by itself. Replay folds every record with the same ID into one
// incomplete assistant message. The eventual ordinary Message carries DraftID
// and atomically replaces that logical draft, so a clean run has exactly the
// same conversation shape it did before streaming checkpoints existed.
type assistantDraftRecord struct {
	ID     string                `json:"id"`
	Events []assistantDraftEvent `json:"events"`
}

type assistantDraftEvent struct {
	Type      provider.EventType `json:"type"`
	Index     int                `json:"index"`
	Text      string             `json:"text,omitempty"`
	Signature string             `json:"signature,omitempty"`
	ToolUse   *provider.ToolUse  `json:"tool_use,omitempty"`
}

type assistantDraftState struct {
	messageIndex int
	blockIndex   map[int]int
	blocks       []assistantDraftBlock
}

// assistantDraftBlock retains immutable delta chunks until a caller actually
// needs a canonical provider.Message. Concatenating a growing string at every
// WAL checkpoint makes N streamed bytes cost O(N²) copies; appending chunks
// here and joining once at State/finalization keeps the write path linear.
type assistantDraftBlock struct {
	kind      provider.BlockKind
	chunks    []string
	textBytes int
	signature string
	toolUse   provider.ToolUse
}

// CheckpointAssistantDraft durably adds provider deltas to one incomplete
// assistant message. It returns the generated ID on the first checkpoint;
// callers carry that ID on later checkpoints and on the final Message.
//
// The record is synced before this method returns. A surface must not render
// the corresponding deltas before that return: doing so would recreate the
// visible-but-not-recoverable SIGKILL window this API exists to close.
func (s *Session) CheckpointAssistantDraft(id string, events []provider.Event) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	checkpoint, err := prepareAssistantDraftRecord(id, events)
	if err != nil {
		return "", err
	}
	if checkpoint.ID == "" {
		nextSeq, err := s.nextRecordSequence()
		if err != nil {
			return "", err
		}
		checkpoint.ID = fmt.Sprintf("%s:draft:%d", s.state.ID, nextSeq)
	}
	if err := s.validateAssistantDraft(checkpoint); err != nil {
		return "", err
	}
	if err := s.append(RecordAssistantDraft, checkpoint); err != nil {
		return "", err
	}
	// The exact event shapes and block transitions were validated before the
	// append. Applying them now only links owned immutable chunks into the
	// replay-derived accumulator and cannot fail after the durable commit.
	s.applyAssistantDraftCommitted(checkpoint)
	return checkpoint.ID, nil
}

func prepareAssistantDraftRecord(id string, events []provider.Event) (assistantDraftRecord, error) {
	if id != strings.TrimSpace(id) {
		return assistantDraftRecord{}, fmt.Errorf("assistant draft id has surrounding whitespace")
	}
	if len(id) > 256 {
		return assistantDraftRecord{}, fmt.Errorf("assistant draft id is too long")
	}
	if len(events) == 0 {
		return assistantDraftRecord{}, fmt.Errorf("assistant draft checkpoint has no events")
	}
	out := assistantDraftRecord{ID: id, Events: make([]assistantDraftEvent, len(events))}
	for i, event := range events {
		if event.Index < 0 {
			return assistantDraftRecord{}, fmt.Errorf("assistant draft event %d has a negative block index", i)
		}
		stored := assistantDraftEvent{Type: event.Type, Index: event.Index, Text: event.Text, Signature: event.Signature}
		switch event.Type {
		case provider.EventTextDelta:
			if event.Text == "" || event.Signature != "" || event.ToolUse != nil {
				return assistantDraftRecord{}, fmt.Errorf("assistant text draft event %d has invalid fields", i)
			}
		case provider.EventThinkingDelta:
			if event.Text == "" && event.Signature == "" {
				return assistantDraftRecord{}, fmt.Errorf("assistant thinking draft event %d is empty", i)
			}
			if event.ToolUse != nil {
				return assistantDraftRecord{}, fmt.Errorf("assistant thinking draft event %d carries a tool call", i)
			}
		case provider.EventToolUse:
			if event.ToolUse == nil || event.ToolUse.ID == "" || event.ToolUse.Name == "" || event.Text != "" || event.Signature != "" {
				return assistantDraftRecord{}, fmt.Errorf("assistant tool draft event %d is incomplete", i)
			}
			use := *event.ToolUse
			use.Input = append(json.RawMessage(nil), event.ToolUse.Input...)
			stored.ToolUse = &use
		default:
			return assistantDraftRecord{}, fmt.Errorf("assistant draft event %d has unsupported type %q", i, event.Type)
		}
		out.Events[i] = stored
	}
	return out, nil
}

func decodeAssistantDraft(raw []byte) (assistantDraftRecord, error) {
	var checkpoint assistantDraftRecord
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		return assistantDraftRecord{}, err
	}
	if strings.TrimSpace(checkpoint.ID) == "" || len(checkpoint.ID) > 256 {
		return assistantDraftRecord{}, fmt.Errorf("assistant draft has an invalid id")
	}
	// Reuse the outbound validator for the event shape and ownership copy.
	events := make([]provider.Event, len(checkpoint.Events))
	for i, event := range checkpoint.Events {
		events[i] = provider.Event{
			Type: event.Type, Index: event.Index, Text: event.Text,
			Signature: event.Signature, ToolUse: event.ToolUse,
		}
	}
	validated, err := prepareAssistantDraftRecord(checkpoint.ID, events)
	if err != nil {
		return assistantDraftRecord{}, err
	}
	return validated, nil
}

func (s *Session) validateAssistantDraft(checkpoint assistantDraftRecord) error {
	if checkpoint.ID == "" || len(checkpoint.Events) == 0 {
		return fmt.Errorf("assistant draft record is incomplete")
	}
	draft, exists := s.assistantDrafts[checkpoint.ID]
	if exists {
		if err := s.validateActiveAssistantDraft(checkpoint.ID, draft); err != nil {
			return err
		}
	}

	// Only indexes introduced by this batch need overlay state. Existing block
	// metadata is immutable, so validating a checkpoint never clones the full
	// accumulated message or its index map.
	introduced := make(map[int]provider.BlockKind)
	for i, event := range checkpoint.Events {
		kind, err := assistantDraftEventKind(event)
		if err != nil {
			return fmt.Errorf("assistant draft event %d: %w", i, err)
		}
		position, present := 0, false
		if exists {
			position, present = draft.blockIndex[event.Index]
		}
		if present {
			if position < 0 || position >= len(draft.blocks) {
				return fmt.Errorf("assistant draft block %d has an invalid position", event.Index)
			}
			prior := draft.blocks[position].kind
			if prior != kind {
				return fmt.Errorf("block %d changed kind to %s", event.Index, kind)
			}
			if kind == provider.KindToolUse {
				return fmt.Errorf("tool block %d was checkpointed twice", event.Index)
			}
			continue
		}
		if prior, ok := introduced[event.Index]; ok {
			if prior != kind {
				return fmt.Errorf("block %d changed kind to %s", event.Index, kind)
			}
			if kind == provider.KindToolUse {
				return fmt.Errorf("tool block %d was checkpointed twice", event.Index)
			}
			continue
		}
		introduced[event.Index] = kind
	}
	return nil
}

func (s *Session) applyAssistantDraft(checkpoint assistantDraftRecord) error {
	if err := s.validateAssistantDraft(checkpoint); err != nil {
		return err
	}
	s.applyAssistantDraftCommitted(checkpoint)
	return nil
}

func (s *Session) validateActiveAssistantDraft(id string, draft *assistantDraftState) error {
	if draft == nil || draft.messageIndex < 0 || draft.messageIndex != len(s.state.Messages)-1 {
		return fmt.Errorf("assistant draft %q no longer names the conversation tail", id)
	}
	message := s.state.Messages[draft.messageIndex]
	if message.Role != provider.RoleAssistant || !message.Incomplete || message.DraftID != id {
		return fmt.Errorf("assistant draft %q does not name an incomplete assistant message", id)
	}
	return nil
}

func assistantDraftEventKind(event assistantDraftEvent) (provider.BlockKind, error) {
	switch event.Type {
	case provider.EventTextDelta:
		return provider.KindText, nil
	case provider.EventThinkingDelta:
		return provider.KindThinking, nil
	case provider.EventToolUse:
		if event.ToolUse == nil {
			return "", fmt.Errorf("tool block %d has no call", event.Index)
		}
		return provider.KindToolUse, nil
	default:
		return "", fmt.Errorf("unsupported event type %q", event.Type)
	}
}

func (s *Session) applyAssistantDraftCommitted(checkpoint assistantDraftRecord) {
	draft, exists := s.assistantDrafts[checkpoint.ID]
	if s.assistantDrafts == nil {
		s.assistantDrafts = make(map[string]*assistantDraftState)
	}
	if !exists {
		draft = &assistantDraftState{
			messageIndex: len(s.state.Messages),
			blockIndex:   make(map[int]int),
		}
		s.state.Messages = append(s.state.Messages, provider.Message{
			Role: provider.RoleAssistant, Incomplete: true, DraftID: checkpoint.ID,
		})
		s.assistantDrafts[checkpoint.ID] = draft
	}
	for _, event := range checkpoint.Events {
		position, present := draft.blockIndex[event.Index]
		if !present {
			kind, _ := assistantDraftEventKind(event)
			position = len(draft.blocks)
			draft.blockIndex[event.Index] = position
			draft.blocks = append(draft.blocks, assistantDraftBlock{kind: kind})
		}
		block := &draft.blocks[position]
		switch event.Type {
		case provider.EventTextDelta:
			block.chunks = append(block.chunks, event.Text)
			block.textBytes += len(event.Text)
		case provider.EventThinkingDelta:
			if event.Text != "" {
				block.chunks = append(block.chunks, event.Text)
				block.textBytes += len(event.Text)
			}
			if event.Signature != "" {
				block.signature = event.Signature
			}
		case provider.EventToolUse:
			block.toolUse = *event.ToolUse
			block.toolUse.Input = append(json.RawMessage(nil), event.ToolUse.Input...)
		}
	}
}

func (s *Session) materializeAssistantDraft(draft *assistantDraftState) provider.Message {
	message := provider.CloneMessage(s.state.Messages[draft.messageIndex])
	message.Content = make([]provider.Block, 0, len(draft.blocks))
	for _, block := range draft.blocks {
		switch block.kind {
		case provider.KindText:
			message.Content = append(message.Content, provider.Text{Text: joinAssistantDraftChunks(block)})
		case provider.KindThinking:
			message.Content = append(message.Content, provider.Thinking{
				Text: joinAssistantDraftChunks(block), Signature: block.signature,
			})
		case provider.KindToolUse:
			use := block.toolUse
			use.Input = append(json.RawMessage(nil), block.toolUse.Input...)
			message.Content = append(message.Content, use)
		}
	}
	return message
}

func joinAssistantDraftChunks(block assistantDraftBlock) string {
	if len(block.chunks) == 1 {
		return block.chunks[0]
	}
	var out strings.Builder
	out.Grow(block.textBytes)
	for _, chunk := range block.chunks {
		out.WriteString(chunk)
	}
	return out.String()
}

func (s *Session) validateDraftFinal(message provider.Message) error {
	if message.DraftID == "" {
		return nil
	}
	if message.Role != provider.RoleAssistant || message.Injected || message.ContinuityRef != "" {
		return fmt.Errorf("assistant draft finalization requires an ordinary assistant message")
	}
	draft, ok := s.assistantDrafts[message.DraftID]
	if !ok {
		return fmt.Errorf("assistant draft %q is not active", message.DraftID)
	}
	if draft.messageIndex < 0 || draft.messageIndex != len(s.state.Messages)-1 {
		return fmt.Errorf("assistant draft %q no longer names the conversation tail", message.DraftID)
	}
	checkpoint := s.materializeAssistantDraft(draft)
	checkpoint.Incomplete = false
	final := provider.CloneMessage(message)
	final.Incomplete = false
	if !reflect.DeepEqual(checkpoint, final) {
		return fmt.Errorf("assistant draft %q final message does not match its durable checkpoints", message.DraftID)
	}
	return nil
}

func (s *Session) applyMessageState(message provider.Message) error {
	if err := s.validateDraftFinal(message); err != nil {
		return err
	}
	if message.DraftID == "" {
		s.state.Messages = append(s.state.Messages, message)
		return nil
	}
	draft := s.assistantDrafts[message.DraftID]
	s.state.Messages[draft.messageIndex] = message
	delete(s.assistantDrafts, message.DraftID)
	return nil
}
