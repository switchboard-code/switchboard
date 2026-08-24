package session

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/provider"
)

// forkFixture records two turns: a plain exchange, then a turn with a tool
// round, with usage after each assistant message.
func forkFixture(t *testing.T) (*Store, *Session) {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), "scripted/local/test", "rev-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })

	append := func(m provider.Message) {
		t.Helper()
		if err := sess.AppendMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	usage := func(cost int64) {
		t.Helper()
		if err := sess.AppendUsage(Usage{Usage: provider.Usage{InputTokens: 10, OutputTokens: 5}, CostMicroUSD: cost}); err != nil {
			t.Fatal(err)
		}
	}

	append(provider.UserText("first question"))
	append(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "first answer"}}})
	usage(100)
	append(provider.UserText("second question"))
	append(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
		provider.ToolUse{ID: "call_1", Name: "read", Input: []byte(`{"path":"a.txt"}`)},
	}})
	usage(200)
	append(provider.Message{Role: provider.RoleTool, Content: []provider.Block{
		provider.ToolResult{ToolUseID: "call_1", Name: "read", Content: "contents"},
	}})
	append(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "second answer"}}})
	usage(300)
	return store, sess
}

func TestForkCopiesThePrefixAndItsUsage(t *testing.T) {
	store, src := forkFixture(t)

	fork, err := store.Fork(src.ID(), 2) // the first turn and its usage
	if err != nil {
		t.Fatal(err)
	}
	defer fork.Close()

	state := fork.State()
	if state.ID == src.ID() {
		t.Fatal("a fork must be a new session, not the source reopened")
	}
	if len(state.Messages) != 2 {
		t.Fatalf("fork holds %d messages, want 2", len(state.Messages))
	}
	if state.Messages[1].Role != provider.RoleAssistant || state.Messages[1].Text() != "first answer" {
		t.Errorf("fork's last message = %+v, want the first turn's answer", state.Messages[1])
	}
	if state.CostMicroUSD != 100 {
		t.Errorf("fork cost = %d, want only the kept turn's 100", state.CostMicroUSD)
	}
	if state.Workspace != src.State().Workspace || state.CatalogRevision != "rev-1" {
		t.Error("workspace and catalog revision must carry over")
	}
}

func TestForkCarriesRetryReserveEvenWhenItsTurnIsDropped(t *testing.T) {
	store, src := forkFixture(t)
	if err := src.AppendRetryReserve(77); err != nil {
		t.Fatal(err)
	}

	full, err := store.Fork(src.ID(), 6)
	if err != nil {
		t.Fatal(err)
	}
	defer full.Close()
	if got := full.State().RetryReserveMicroUSD; got != 77 {
		t.Fatalf("full fork retry reserve = %d, want 77", got)
	}

	earlier, err := store.Fork(src.ID(), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer earlier.Close()
	if got := earlier.State().RetryReserveMicroUSD; got != 77 {
		t.Fatalf("earlier fork erased dropped-turn retry reserve: %d", got)
	}
}

func TestFirstTurnRetryCarriesObservedCostAndDebtWithoutUsage(t *testing.T) {
	store, src := forkFixture(t)
	if err := src.AppendRetryReserve(77); err != nil {
		t.Fatal(err)
	}

	retry, err := store.ForkForRetry(src.ID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer retry.Close()
	state := retry.State()
	if len(state.Messages) != 0 {
		t.Fatalf("first-turn retry retained %d conversation messages", len(state.Messages))
	}
	if state.RetryReserveMicroUSD != 77 {
		t.Fatalf("retry debt = %d, want 77", state.RetryReserveMicroUSD)
	}
	if state.CostMicroUSD != 0 || state.ExternalCostMicroUSD != 600 || state.AccountedCostMicroUSD() != 600 {
		t.Fatalf("dropped observed cost was not transferred exactly: %+v", state)
	}
	if state.Calls != 0 || state.Usage != (provider.Usage{}) {
		t.Fatalf("retry cost transfer invented provider usage: %+v", state)
	}
}

func TestRepeatedRetryCannotResetPriorCeilingSpend(t *testing.T) {
	store, src := forkFixture(t)
	first, err := store.ForkForRetry(src.ID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.AppendMessage(provider.UserText("again")); err != nil {
		t.Fatal(err)
	}
	if err := first.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "again answer"}}}); err != nil {
		t.Fatal(err)
	}
	if err := first.AppendUsage(Usage{CostMicroUSD: 125}); err != nil {
		t.Fatal(err)
	}
	firstID := first.ID()
	first.Close()

	second, err := store.ForkForRetry(firstID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if got := second.State().AccountedCostMicroUSD(); got != 725 {
		t.Fatalf("second retry accounted cost = %d, want cumulative 725", got)
	}
}

func TestForkNeverWritesTheSource(t *testing.T) {
	store, src := forkFixture(t)
	before, err := os.ReadFile(src.Path())
	if err != nil {
		t.Fatal(err)
	}

	fork, err := store.Fork(src.ID(), 6)
	if err != nil {
		t.Fatal(err)
	}
	fork.Close()

	after, err := os.ReadFile(src.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("the source log changed; a fork reads, never writes")
	}
}

func TestForkRefusesACutInsideATurn(t *testing.T) {
	store, src := forkFixture(t)

	// Message 4 is the assistant's tool call: cutting there would drop the
	// call's results while keeping the call.
	_, err := store.Fork(src.ID(), 4)
	if err == nil || !strings.Contains(err.Error(), "inside a turn") {
		t.Fatalf("err = %v, want the mid-turn cut refused", err)
	}
}

func TestForkRefusesInjectedUserMessageAsATurnBoundary(t *testing.T) {
	store, src := forkFixture(t)

	if err := src.AppendMessage(provider.UserText("third question")); err != nil {
		t.Fatal(err)
	}
	if err := src.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
		provider.ToolUse{ID: "call_2", Name: "read", Input: []byte(`{"path":"b.txt"}`)},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := src.AppendMessage(provider.Message{Role: provider.RoleTool, Content: []provider.Block{
		provider.ToolResult{ToolUseID: "call_2", Name: "read", Content: "contents"},
	}}); err != nil {
		t.Fatal(err)
	}
	injected := provider.UserText("[watch] tests are still red")
	injected.Injected = true
	if err := src.AppendMessage(injected); err != nil {
		t.Fatal(err)
	}
	if err := src.AppendMessage(provider.Message{
		Role: provider.RoleAssistant,
		Content: []provider.Block{
			provider.Text{Text: "I will keep debugging."},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// The injected report is user-role on provider wires, but it belongs to the
	// turn already in progress. Treating it as an opening would let a fork keep
	// the tool round while silently discarding that turn's closing answer.
	_, err := store.Fork(src.ID(), 9)
	if err == nil || !strings.Contains(err.Error(), "inside a turn") {
		t.Fatalf("err = %v, want the injected mid-turn cut refused", err)
	}
}

func TestForkBoundsAndProvenance(t *testing.T) {
	store, src := forkFixture(t)

	if _, err := store.Fork(src.ID(), 0); err == nil {
		t.Error("keeping zero messages must refuse; that is /clear")
	}
	if _, err := store.Fork(src.ID(), 99); err == nil {
		t.Error("keeping more than the session holds must refuse")
	}
	if _, err := store.Fork("no-such-id", 1); err == nil {
		t.Error("an unknown session must refuse")
	}

	// The full-copy fork resumes cleanly from disk, records where it came
	// from, and shares the complete history.
	fork, err := store.Fork(src.ID(), 6)
	if err != nil {
		t.Fatal(err)
	}
	id := fork.ID()
	fork.Close()
	reopened, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := len(reopened.State().Messages); got != 6 {
		t.Errorf("reopened fork holds %d messages, want all 6", got)
	}
	if reopened.State().CostMicroUSD != 600 {
		t.Errorf("reopened fork cost = %d, want the full 600", reopened.State().CostMicroUSD)
	}
}

func TestReadStateNeedsNoLock(t *testing.T) {
	_, src := forkFixture(t) // src stays open, holding the append lock

	state, err := ReadState(src.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Messages) != 6 || state.CostMicroUSD != 600 {
		t.Fatalf("read-only replay saw %d messages costing %d, want the full 6 and 600",
			len(state.Messages), state.CostMicroUSD)
	}
	if state.ID != src.ID() {
		t.Errorf("ID = %s, want the source's own", state.ID)
	}
}

func TestLiveForkTakesOneAccountingSnapshot(t *testing.T) {
	store, src := forkFixture(t)
	src.mu.Lock()
	started := make(chan struct{})
	done := make(chan *Session, 1)
	errs := make(chan error, 1)
	go func() {
		close(started)
		fork, err := store.ForkSessionForRetry(src, 0)
		if err != nil {
			errs <- err
			return
		}
		done <- fork
	}()
	<-started
	select {
	case fork := <-done:
		fork.Close()
		src.mu.Unlock()
		t.Fatal("live fork did not wait for the source append lock")
	case err := <-errs:
		src.mu.Unlock()
		t.Fatalf("live fork failed before source unlock: %v", err)
	case <-time.After(30 * time.Millisecond):
	}

	p := BudgetAttempt{ID: fmt.Sprintf("%s:%d", src.state.ID, src.seq+1), CostMicroUSD: 1_234}
	if err := src.append(RecordBudgetAttempt, p); err != nil {
		src.mu.Unlock()
		t.Fatal(err)
	}
	if err := src.state.applyBudgetAttempt(p); err != nil {
		src.mu.Unlock()
		t.Fatal(err)
	}
	src.mu.Unlock()

	select {
	case err := <-errs:
		t.Fatal(err)
	case fork := <-done:
		defer fork.Close()
		if got := fork.State().RetryReserveMicroUSD; got != 1_234 {
			t.Fatalf("fork omitted WAL committed before its snapshot: reserve=%d", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("live fork did not finish")
	}
}

func TestRaceBranchLifecycleDoesNotRideANewFork(t *testing.T) {
	store, src := forkFixture(t)
	if err := src.MarkRaceBranchPending("first-origin"); err != nil {
		t.Fatal(err)
	}
	if err := src.FinalizeRaceBranch(); err != nil {
		t.Fatal(err)
	}

	child, err := store.ForkSessionOnto(src, len(src.State().Messages), "scripted/local/new-race")
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	if err := child.MarkRaceBranchPending("second-origin"); err != nil {
		t.Fatalf("finalized lifecycle marker from the first race rode the fork: %v", err)
	}
}

func TestPendingRaceBranchIsNeitherForkableNorResumable(t *testing.T) {
	store, src := forkFixture(t)
	if err := src.MarkRaceBranchPending("origin"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ForkSession(src, len(src.State().Messages)); !errors.Is(err, ErrRaceBranchPending) {
		t.Fatalf("fork pending branch err = %v, want ErrRaceBranchPending", err)
	}
	workspace, id := src.State().Workspace, src.ID()
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(id); !errors.Is(err, ErrRaceBranchPending) {
		t.Fatalf("open pending branch err = %v, want ErrRaceBranchPending", err)
	}
	infos, err := store.List(workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range infos {
		if info.ID == id {
			t.Fatal("pending race branch was advertised as resumable")
		}
	}
}

func TestLatestPrefersRaceVerdictOverLaterTouchedAlternative(t *testing.T) {
	store, workspace := newStore(t)
	origin, err := store.Create(workspace, "scripted/local/origin", "rev")
	if err != nil {
		t.Fatal(err)
	}
	winner, err := store.Create(workspace, "scripted/local/winner", "rev")
	if err != nil {
		t.Fatal(err)
	}
	alternative, err := store.Create(workspace, "scripted/local/alternative", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	defer winner.Close()
	defer alternative.Close()
	for _, branch := range []*Session{winner, alternative} {
		if err := branch.MarkRaceBranchPending(origin.ID()); err != nil {
			t.Fatal(err)
		}
	}
	if err := winner.FinalizeRaceBranch(); err != nil {
		t.Fatal(err)
	}
	if err := alternative.FinalizeRaceBranchAlternative(); err != nil {
		t.Fatal(err)
	}
	// Reproduce the old failure mode: the loser was annotated last and has the
	// newest mtime, but it is not the continuation the user selected.
	if err := alternative.AppendNote("info", "road not taken"); err != nil {
		t.Fatal(err)
	}
	if err := winner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := alternative.Close(); err != nil {
		t.Fatal(err)
	}
	latest, err := store.Latest(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer latest.Close()
	if got := latest.ID(); got != winner.ID() {
		t.Fatalf("--continue selected %s, want kept race branch %s", got, winner.ID())
	}
}
