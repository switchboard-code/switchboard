package session

import (
	"fmt"

	"github.com/switchboard-code/switchboard/internal/provider"
)

const interruptedToolResult = "Switchboard was interrupted before this tool result was recorded; outcome unknown: the call may or may not have completed. Inspect the relevant state before retrying; never repeat a non-idempotent effect without evidence that it is still needed."

// ReconcileInterruptedToolCalls closes a crash window at an adoption boundary.
// The assistant call was already durable, so replaying it without a result
// would make the next provider request malformed. The missing result cannot be
// reconstructed and the call must never be reexecuted implicitly; an explicit
// error records that uncertainty and tells the next model to inspect first.
//
// Only the replay tail is considered: after incomplete assistant messages are
// projected out, zero or more tool-result messages must be immediately
// preceded by the assistant call batch. Older history is evidence, not a crash
// boundary this method is allowed to rewrite around. The synthetic message is
// append-only and this method is idempotent.
func (s *Session) ReconcileInterruptedToolCalls() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	missing := interruptedToolCallsAtTail(s.state.Messages)
	if len(missing) == 0 {
		return 0, nil
	}
	blocks := make([]provider.Block, len(missing))
	for i, call := range missing {
		blocks[i] = provider.ToolResult{
			ToolUseID: call.ID,
			Name:      call.Name,
			Content:   interruptedToolResult,
			IsError:   true,
		}
	}
	message := provider.Message{Role: provider.RoleTool, Content: blocks}
	if err := s.append(RecordMessage, message); err != nil {
		return 0, fmt.Errorf("recording interrupted tool results: %w", err)
	}
	s.state.Messages = append(s.state.Messages, provider.CloneMessage(message))
	return len(missing), nil
}

func interruptedToolCallsAtTail(messages []provider.Message) []provider.ToolUse {
	i := len(messages) - 1
	for i >= 0 && replayOmits(messages[i]) {
		i--
	}

	results := make(map[string]bool)
	for i >= 0 && messages[i].Role == provider.RoleTool {
		for _, block := range messages[i].Content {
			switch result := block.(type) {
			case provider.ToolResult:
				results[result.ToolUseID] = true
			case *provider.ToolResult:
				if result != nil {
					results[result.ToolUseID] = true
				}
			}
		}
		i--
		for i >= 0 && replayOmits(messages[i]) {
			i--
		}
	}
	if i < 0 || messages[i].Role != provider.RoleAssistant || messages[i].Incomplete {
		return nil
	}

	var missing []provider.ToolUse
	for _, call := range messages[i].ToolUses() {
		if !results[call.ID] {
			missing = append(missing, call)
		}
	}
	return missing
}

func replayOmits(message provider.Message) bool {
	return message.Role == provider.RoleAssistant && message.Incomplete
}
