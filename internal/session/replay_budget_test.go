package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func replayTestRecord(t *testing.T, seq int, typ RecordType, payload any) Record {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return Record{Seq: seq, At: time.Unix(int64(seq), 0).UTC(), Type: typ, Payload: raw}
}

func TestReplayBudgetExactAndOverEveryDimension(t *testing.T) {
	message := replayTestRecord(t, 1, RecordMessage, provider.Message{
		Role: provider.RoleUser,
		Content: []provider.Block{
			provider.Text{Text: "one"}, provider.Text{Text: "two"},
		},
	})

	tests := []struct {
		name   string
		limits replayLimits
		first  Record
		second Record
		bytes1 int
		bytes2 int
	}{
		{
			name: "bytes", limits: replayLimits{bytes: 10, records: 10, messages: 10, blocks: 10},
			first: message, second: replayTestRecord(t, 2, RecordNote, Note{}), bytes1: 5, bytes2: 6,
		},
		{
			name: "records", limits: replayLimits{bytes: 1 << 20, records: 1, messages: 10, blocks: 10},
			first: message, second: replayTestRecord(t, 2, RecordNote, Note{}), bytes1: 1, bytes2: 1,
		},
		{
			name: "messages", limits: replayLimits{bytes: 1 << 20, records: 10, messages: 1, blocks: 10},
			first: message, second: replayTestRecord(t, 2, RecordMessage, provider.UserText("next")), bytes1: 1, bytes2: 1,
		},
		{
			name: "blocks", limits: replayLimits{bytes: 1 << 20, records: 10, messages: 10, blocks: 2},
			first: message, second: replayTestRecord(t, 2, RecordMessage, provider.UserText("third")), bytes1: 1, bytes2: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			budget := newReplayBudget(tt.limits, 0)
			if err := budget.admit(tt.first, tt.bytes1); err != nil {
				t.Fatalf("exact-limit record refused: %v", err)
			}
			before := *budget
			if err := budget.admit(tt.second, tt.bytes2); !errors.Is(err, ErrSessionReplayTooLarge) {
				t.Fatalf("over-limit error = %v, want ErrSessionReplayTooLarge", err)
			}
			if budget.bytes != before.bytes || budget.records != before.records || budget.messages != before.messages || budget.blocks != before.blocks {
				t.Fatalf("rejected record changed accounting: before=%+v after=%+v", before, *budget)
			}
		})
	}
}

func TestReplayBudgetCountsLogicalDraftBlocksWithoutDoubleCountingDeltas(t *testing.T) {
	limits := replayLimits{bytes: 1 << 20, records: 10, messages: 1, blocks: 2}
	budget := newReplayBudget(limits, 0)
	first := replayTestRecord(t, 1, RecordAssistantDraft, assistantDraftRecord{
		ID: "draft", Events: []assistantDraftEvent{
			{Type: provider.EventTextDelta, Index: 0, Text: "a"},
			{Type: provider.EventTextDelta, Index: 0, Text: "b"},
			{Type: provider.EventThinkingDelta, Index: 1, Text: "c"},
		},
	})
	if err := budget.admit(first, 1); err != nil {
		t.Fatal(err)
	}
	if budget.messages != 1 || budget.blocks != 2 {
		t.Fatalf("draft accounting = %d messages, %d blocks; want 1, 2", budget.messages, budget.blocks)
	}
	continuation := replayTestRecord(t, 2, RecordAssistantDraft, assistantDraftRecord{
		ID: "draft", Events: []assistantDraftEvent{{Type: provider.EventTextDelta, Index: 0, Text: "d"}},
	})
	if err := budget.admit(continuation, 1); err != nil {
		t.Fatalf("existing draft delta was double-counted: %v", err)
	}
	third := replayTestRecord(t, 3, RecordAssistantDraft, assistantDraftRecord{
		ID: "draft", Events: []assistantDraftEvent{{Type: provider.EventTextDelta, Index: 2, Text: "e"}},
	})
	if err := budget.admit(third, 1); !errors.Is(err, ErrSessionReplayTooLarge) {
		t.Fatalf("new block error = %v, want ErrSessionReplayTooLarge", err)
	}
}

func TestReplayLimitIsInventoryOnlyAndNeverPartiallyAdopted(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("admitted prefix")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "must not be adopted"}}}); err != nil {
		t.Fatal(err)
	}
	id, path := sess.ID(), sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	limits := replayLimits{bytes: 1 << 20, records: 2, messages: 10, blocks: 10}
	store.replayLimit = limits
	infos, err := store.List(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != id || !infos[0].Health.ReplayLimit {
		t.Fatalf("bounded inventory = %+v, want one replay-blocked session", infos)
	}
	if infos[0].Health.Messages != 1 {
		t.Fatalf("health prefix messages = %d, want 1", infos[0].Health.Messages)
	}
	all, err := store.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all[workspace]) != 1 || !all[workspace][0].Health.ReplayLimit {
		t.Fatalf("ListAll did not preserve replay-blocked health: %+v", all)
	}
	if _, err := store.Latest(workspace); !errors.Is(err, ErrSessionReplayTooLarge) {
		t.Fatalf("Latest error = %v, want ErrSessionReplayTooLarge", err)
	}
	if _, err := store.Open(id); !errors.Is(err, ErrSessionReplayTooLarge) {
		t.Fatalf("Open error = %v, want ErrSessionReplayTooLarge", err)
	}
	if fork, err := store.Fork(id, 1); !errors.Is(err, ErrSessionReplayTooLarge) {
		if fork != nil {
			_ = fork.CloseDiscardingStaged()
		}
		t.Fatalf("Fork error = %v, want ErrSessionReplayTooLarge", err)
	}
	state, err := readStateWithLimits(path, limits)
	if !errors.Is(err, ErrSessionReplayTooLarge) {
		t.Fatalf("ReadState error = %v, want ErrSessionReplayTooLarge", err)
	}
	if state.ID != "" || len(state.Messages) != 0 {
		t.Fatalf("ReadState returned a partial state: %+v", state)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("bounded inventory or failed adoption changed the source log")
	}
}
