package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

func appendTurn(t *testing.T, m *tuiModel, question, answer string) {
	t.Helper()
	sess := m.app.loop.Session
	// Production binds the runtime workspace and session start together. Keep
	// the shared TUI fixture on that invariant before retry can journal it.
	m.app.workspace = sess.State().Workspace
	if err := sess.AppendMessage(provider.UserText(question)); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleAssistant,
		Content: []provider.Block{provider.Text{Text: answer}}}); err != nil {
		t.Fatal(err)
	}
}

func retrySourceLabelled(t *testing.T, path string) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(data), "user_corrected")
}

func mustStageRetry(t *testing.T, m *tuiModel) sessionSwapMsg {
	t.Helper()
	// Production app/session workspaces are assembled together. testModel uses
	// an intentionally synthetic display path, so repair that fixture invariant
	// before exercising durable retry preparation.
	m.app.workspace = m.app.loop.Session.State().Workspace
	cmd := cmdRetry(m, "")
	if cmd == nil {
		t.Fatal("retry returned no command")
	}
	msg, ok := cmd().(sessionSwapMsg)
	if !ok || msg.err != nil || msg.sess == nil {
		t.Fatalf("retry did not stage a swap (error %v): %#v", msg.err, msg)
	}
	return msg
}

func retryFileModel(t *testing.T) (*tuiModel, string, string, string) {
	t.Helper()
	m := testModel(t)
	// Durable retry journals are workspace-bound, just like the staged child.
	// testModel deliberately uses a display-only placeholder workspace, so bind
	// this fixture to the real temporary workspace recorded by its session.
	m.app.workspace = m.app.loop.Session.State().Workspace
	appendTurn(t, m, "edit the file", "done")
	path := filepath.Join(m.app.workspace, "retry.txt")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	recorder := checkpoint.NewRecorder()
	recorder.BeginTurn(m.app.loop.Session.ID(), 0, "edit the file")
	recorder.Record(path)
	if err := os.WriteFile(path, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.app.undo = recorder
	return m, path, m.app.loop.Session.ID(), m.app.loop.Session.Path()
}

func assertRetryKeptSource(t *testing.T, m *tuiModel, sourceID, sourcePath, path, wantFile string) {
	t.Helper()
	if got := m.app.loop.Session.ID(); got != sourceID {
		t.Fatalf("retry adopted a child after failure: got session %s, want %s", got, sourceID)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != wantFile {
		t.Fatalf("retry failure changed the workspace: content=%q err=%v, want %q", data, err, wantFile)
	}
	if retrySourceLabelled(t, sourcePath) {
		t.Fatal("retry failure labelled the source user_corrected")
	}
	if m.operationActive || m.busy {
		t.Fatalf("retry failure retained operation ownership: operation=%v busy=%v", m.operationActive, m.busy)
	}
}

func bindRetryRecoveryChild(t *testing.T, m *tuiModel, openingRecorded, started bool) (*session.Session, session.RetryIntent) {
	t.Helper()
	m.app.workspace = m.app.loop.Session.State().Workspace
	appendTurn(t, m, "recover this exact retry", "set aside")
	source := m.app.loop.Session
	opening := provider.CloneMessage(source.State().Messages[0])
	child, err := m.app.store.ForkSessionForRetryStaged(source, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Close() })
	target, tierSet := retryTierIdentity(m.app.tier)
	intent, err := child.AppendRetryIntent(source.ID(), 0, opening, m.app.tier.ID, target, tierSet)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := child.PublishDurably()
	if err != nil || !outcome.Visible || !outcome.Durable {
		t.Fatalf("publishing retry child = %+v, %v", outcome, err)
	}
	if openingRecorded {
		opening.RetryIntentID = intent.ID
		if err := child.AppendMessage(opening); err != nil {
			t.Fatal(err)
		}
	}
	if started {
		if err := child.StartRetryIntent(intent.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.app.loop.BindSession(child); err != nil {
		t.Fatal(err)
	}
	return source, intent
}

func TestPendingRetryStartupDistinguishesCutAndMarkedOpening(t *testing.T) {
	t.Run("source cut", func(t *testing.T) {
		m := testModel(t)
		_, intent := bindRetryRecoveryChild(t, m, false, false)
		cmd := pendingRetryStartup(m, m.app.store)
		if cmd == nil {
			t.Fatal("pending source-cut retry did not schedule recovery")
		}
		msg, ok := cmd().(retryStartMsg)
		if !ok || msg.openingRecorded || msg.generation == 0 || msg.opening.AuthoredText() != "recover this exact retry" {
			t.Fatalf("source-cut retry recovery = %#v", msg)
		}
		if got := m.app.loop.Session.State().RetryIntent; got == nil || got.ID != intent.ID || got.Status != session.RetryIntentPending {
			t.Fatalf("startup mutated pending intent before execution: %#v", got)
		}
	})

	t.Run("marked opening", func(t *testing.T) {
		m := testModel(t)
		_, intent := bindRetryRecoveryChild(t, m, true, false)
		cmd := pendingRetryStartup(m, m.app.store)
		if cmd == nil {
			t.Fatal("pending marked-opening retry did not schedule recovery")
		}
		msg, ok := cmd().(retryStartMsg)
		if !ok || !msg.openingRecorded || msg.opening.RetryIntentID != intent.ID {
			t.Fatalf("marked-opening retry recovery = %#v", msg)
		}
	})
}

func TestStartedRetryStartupAndIdleGuardNeverLaunchNewWork(t *testing.T) {
	m := testModel(t)
	_, intent := bindRetryRecoveryChild(t, m, true, true)
	if cmd := pendingRetryStartup(m, m.app.store); cmd != nil {
		t.Fatal("started retry scheduled an automatic provider replay")
	}
	if got := m.app.loop.Session.State().RetryIntent; got == nil || got.ID != intent.ID || got.Status != session.RetryIntentStarted {
		t.Fatalf("started recovery state = %#v", got)
	}

	m.ta.SetValue("do something else")
	if cmd := m.submit(); cmd == nil {
		t.Fatal("plain prompt was not refused while retry recovery was unresolved")
	}
	if cmd := m.enqueue("queued work", ""); cmd == nil || len(m.queue) != 0 {
		t.Fatalf("idle unresolved retry accepted queue: cmd=%v queue=%v", cmd != nil, m.queue)
	}
	if cmd := m.startSyntheticTurn("scheduled work"); cmd == nil {
		t.Fatal("scheduled synthetic turn was not refused")
	}
	if cmd := m.runSlash("/retry"); cmd == nil {
		t.Fatal("bare retry was not refused")
	}
	if cmd := m.runSlash("/fork"); cmd == nil {
		t.Fatal("fork was not refused")
	}
	if cmd := m.runSlash("/resume " + intent.SourceSessionID); cmd == nil || m.operationActive {
		t.Fatal("resume escaped the unresolved retry guard before explicit abandon")
	}
	if got := len(m.app.loop.Session.State().Messages); got != 1 {
		t.Fatalf("guard appended %d messages, want the one recorded retry opening", got)
	}

	released := false
	m.deferredStartup = func() tea.Cmd {
		released = true
		return nil
	}
	abandon := m.runSlash("/retry abandon")
	if abandon == nil {
		t.Fatal("explicit abandon returned no confirmation")
	}
	_ = abandon()
	if got := m.app.loop.Session.State().RetryIntent; got != nil {
		t.Fatalf("explicit abandon left recovery active: %#v", got)
	}
	if !released || m.deferredStartup != nil {
		t.Fatal("explicit abandon did not release deferred background startup")
	}
}

func TestResolvedRecoveredRetryReleasesDeferredStartupAfterProbeRefusal(t *testing.T) {
	m := testModel(t)
	_, intent := bindRetryRecoveryChild(t, m, false, false)
	_, generation := m.startPlanning()
	m.activeRetryIntent = intent.ID
	released := false
	m.deferredStartup = func() tea.Cmd {
		released = true
		return nil
	}

	m.onOverrideProbe(overrideProbeMsg{generation: generation, err: errors.New("injected probe refusal")})
	if !released || m.deferredStartup != nil {
		t.Fatalf("resolved retry left startup deferred: released=%v deferred=%v", released, m.deferredStartup != nil)
	}
	if got := m.app.loop.Session.State().RetryIntent; got != nil {
		t.Fatalf("probe refusal left retry intent unresolved: %#v", got)
	}
}

// An injected user-role message — advice, a watch report — must not be
// mistaken for the turn it landed in.
func TestLastTurnOpeningSkipsInjectedMessages(t *testing.T) {
	messages := []provider.Message{
		provider.UserText("the real opening"),
		{Role: provider.RoleAssistant, Content: []provider.Block{
			provider.ToolUse{ID: "c1", Name: "read", Input: []byte(`{}`)},
		}},
		{Role: provider.RoleTool, Content: []provider.Block{
			provider.ToolResult{ToolUseID: "c1", Name: "read", Content: "x"},
		}},
		provider.UserText("[watch] injected mid-turn"),
		{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "done"}}},
	}
	if got := lastTurnOpening(messages); got != 0 {
		t.Fatalf("want the real opening at 0, got %d", got)
	}

	messages = append(messages, provider.UserText("second turn"))
	if got := lastTurnOpening(messages); got != 5 {
		t.Fatalf("want the second opening at 5, got %d", got)
	}
}

// A cancelled or round-limited turn ends on its tool results; the prompt
// typed after that tail opened a turn all the same.
func TestLastTurnOpeningAcceptsAPromptAfterAnInterruptedTurn(t *testing.T) {
	messages := []provider.Message{
		provider.UserText("first question"),
		{Role: provider.RoleAssistant, Content: []provider.Block{
			provider.ToolUse{ID: "c1", Name: "exec", Input: []byte(`{}`)},
		}},
		{Role: provider.RoleTool, Content: []provider.Block{
			provider.ToolResult{ToolUseID: "c1", Name: "exec", Content: "cancelled before this call ran", IsError: true},
		}},
		provider.UserText("second question, typed after the esc"),
		{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "answer"}}},
	}
	if got := lastTurnOpening(messages); got != 3 {
		t.Fatalf("the opening after the interrupted tail was missed: got %d, want 3", got)
	}
}

// The marker outranks the shape: a marked injection is skipped whatever its
// text says, and an opening that happens to mention the label is kept.
func TestLastTurnOpeningTrustsTheInjectedMarker(t *testing.T) {
	injected := provider.UserText("plain-looking advice with no label")
	injected.Injected = true
	messages := []provider.Message{
		provider.UserText("the opening"),
		{Role: provider.RoleAssistant, Content: []provider.Block{
			provider.ToolUse{ID: "c1", Name: "read", Input: []byte(`{}`)},
		}},
		{Role: provider.RoleTool, Content: []provider.Block{
			provider.ToolResult{ToolUseID: "c1", Name: "read", Content: "x"},
		}},
		injected,
	}
	if got := lastTurnOpening(messages); got != 0 {
		t.Fatalf("a marked injection was taken for an opening: got %d", got)
	}
}

func TestRetryStartDropsTheReplayWhenATurnGotThereFirst(t *testing.T) {
	m := testModel(t)
	m.busy = true
	cmd := m.retryStart(retryStartMsg{prompt: "replay"})
	if cmd == nil {
		t.Fatal("the dropped replay said nothing")
	}
	if msg, ok := cmd().(noticeMsg); !ok || !strings.Contains(msg.text, "before the retry") {
		t.Fatalf("the drop does not say what happened: %+v", msg)
	}
}

func TestRetryForksOffTheLastTurnAndReplaysItsPrompt(t *testing.T) {
	m := testModel(t)
	appendTurn(t, m, "first question", "first answer")
	appendTurn(t, m, "second question", "weak answer")
	source := m.app.loop.Session.ID()
	sourcePath := m.app.loop.Session.Path()

	cmd := cmdRetry(m, "")
	if cmd == nil {
		t.Fatal("retry returned nothing")
	}
	msg, ok := cmd().(sessionSwapMsg)
	if !ok || msg.err != nil {
		t.Fatalf("retry did not produce a swap: %+v", msg)
	}
	defer msg.sess.CloseDiscardingStaged()
	defer m.finishOperation(msg.operation, false)
	if retrySourceLabelled(t, sourcePath) {
		t.Fatal("staging a retry labelled the source before adoption")
	}

	if got := len(msg.sess.State().Messages); got != 2 {
		t.Fatalf("the fork should keep only the first turn, holds %d messages", got)
	}
	if !strings.Contains(msg.note, source) {
		t.Errorf("the note does not say where the set-aside answer lives: %q", msg.note)
	}
	if msg.andThen == nil {
		t.Fatal("the swap carries no continuation")
	}
	start, ok := msg.andThen().(retryStartMsg)
	if !ok || start.prompt != "second question" || start.tier != "" {
		t.Fatalf("the replay is not the recorded opening: %+v", start)
	}
}

func TestRetryNeverPaintsLegacyExpansionOrInjectedContentAsAuthored(t *testing.T) {
	t.Run("legacy provider expansion", func(t *testing.T) {
		m := testModel(t)
		legacy := provider.Message{Role: provider.RoleUser, Content: []provider.Block{
			provider.Text{Text: "possibly typed\n\nMACHINE-EXPANDED-FILE-CONTENTS"},
		}}
		if err := m.app.loop.Session.AppendMessage(legacy); err != nil {
			t.Fatal(err)
		}
		if err := m.app.loop.Session.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
			provider.Text{Text: "legacy answer"},
		}}); err != nil {
			t.Fatal(err)
		}

		msg := cmdRetry(m, "")()
		notice, ok := msg.(noticeMsg)
		if !ok || notice.level != "error" || !strings.Contains(notice.text, "legacy turn") ||
			!strings.Contains(notice.text, "provider-expanded") {
			t.Fatalf("legacy retry result = %#v, want visible authored-projection refusal", msg)
		}
		_, _ = m.Update(msg)
		view := stripANSI(strings.Join(m.tr.flat, "\n"))
		if strings.Contains(view, "MACHINE-EXPANDED-FILE-CONTENTS") {
			t.Fatalf("legacy provider expansion was painted as authored:\n%s", view)
		}
		if m.operationActive || m.busy {
			t.Fatal("legacy retry refusal started a transactional retry")
		}
	})

	t.Run("injected user-role content", func(t *testing.T) {
		m := testModel(t)
		_, generation := m.startPlanning()
		injected := provider.UserText("MACHINE-INJECTED-WATCH-CONTEXT")
		injected.Injected = true

		_ = m.retryStart(retryStartMsg{generation: generation, opening: injected, prompt: "unused"})
		view := stripANSI(strings.Join(m.tr.flat, "\n"))
		if strings.Contains(view, "MACHINE-INJECTED-WATCH-CONTEXT") {
			t.Fatalf("injected content was painted as an authored retry:\n%s", view)
		}
		for _, entry := range m.tr.entries {
			if entry.kind == kindUser {
				t.Fatal("injected retry opening produced a user card")
			}
		}
		if m.busy || m.turnPlanning {
			t.Fatal("injected retry refusal left turn ownership active")
		}
	})
}

func TestForkAndRetrySessionOperationsCannotOverlap(t *testing.T) {
	m := testModel(t)
	appendTurn(t, m, "question", "answer")

	fork := cmdFork(m, "")
	if fork == nil || !m.operationActive || !m.busy {
		t.Fatalf("fork did not claim exclusive ownership: cmd=%v operation=%v busy=%v", fork != nil, m.operationActive, m.busy)
	}
	generation := m.operationGeneration
	retry := cmdRetry(m, "")
	if retry == nil {
		t.Fatal("overlapping retry returned nothing")
	}
	if notice, ok := retry().(noticeMsg); !ok || !strings.Contains(notice.text, "already running") {
		t.Fatalf("overlapping retry was not refused: %#v", notice)
	}
	if !m.operationActive || m.operationGeneration != generation {
		t.Fatal("rejected retry disturbed the fork's ownership")
	}

	msg, ok := fork().(sessionSwapMsg)
	if !ok || msg.err != nil {
		t.Fatalf("fork result = %#v", msg)
	}
	defer msg.sess.CloseDiscardingStaged()
	m.onSessionSwap(msg)
	if m.operationActive || m.busy {
		t.Fatal("completed fork did not release exclusive ownership")
	}
}

func TestRetrySwapClaimsReplayBeforeReturningContinuation(t *testing.T) {
	m := testModel(t)
	appendTurn(t, m, "question", "answer")

	swap, ok := cmdRetry(m, "")().(sessionSwapMsg)
	if !ok || swap.err != nil {
		t.Fatalf("retry result = %#v", swap)
	}
	defer swap.sess.CloseDiscardingStaged()
	continuation := m.onSessionSwap(swap)
	if continuation == nil || !m.busy || !m.turnPlanning || m.turnCancel == nil {
		t.Fatalf("retry bind left an idle continuation gap: cmd=%v busy=%v planning=%v cancel=%v",
			continuation != nil, m.busy, m.turnPlanning, m.turnCancel != nil)
	}
	if cmd := m.enqueue("interloper", ""); cmd != nil || len(m.queue) != 1 {
		t.Fatalf("prompt was not queued behind retry replay: cmd=%v queue=%v", cmd != nil, m.queue)
	}
	start, ok := continuation().(retryStartMsg)
	if !ok || start.generation != m.turnGeneration {
		t.Fatalf("continuation does not own the claimed generation: %#v", start)
	}
	m.interrupt()
}

func TestRetryLabelsTheSetAsideAnswerOnTheSource(t *testing.T) {
	m := testModel(t)
	appendTurn(t, m, "only question", "answer")
	sourcePath := m.app.loop.Session.Path()

	swap := mustStageRetry(t, m)
	defer swap.sess.CloseDiscardingStaged()
	if retrySourceLabelled(t, sourcePath) {
		t.Fatal("the source was labelled before the retry was adopted")
	}
	continuation := m.onSessionSwap(swap)
	if continuation == nil {
		t.Fatal("adopted retry lost its replay continuation")
	}
	if !retrySourceLabelled(t, sourcePath) {
		t.Fatal("the source log carries no user_corrected label")
	}
	m.interrupt()
}

func TestRetryOfTheOnlyTurnStartsFresh(t *testing.T) {
	m := testModel(t)
	appendTurn(t, m, "only question", "answer")
	if err := m.app.loop.Session.AppendUsage(session.Usage{CostMicroUSD: 12_345}); err != nil {
		t.Fatal(err)
	}
	if err := m.app.loop.Session.AppendRetryReserve(54_321); err != nil {
		t.Fatal(err)
	}

	msg, ok := cmdRetry(m, "")().(sessionSwapMsg)
	if !ok || msg.err != nil {
		t.Fatalf("first-turn retry failed: %+v", msg)
	}
	defer msg.sess.CloseDiscardingStaged()
	defer m.finishOperation(msg.operation, false)
	if !msg.fresh || len(msg.sess.State().Messages) != 0 {
		t.Fatalf("dropping the only turn should start fresh: fresh=%v messages=%d", msg.fresh, len(msg.sess.State().Messages))
	}
	state := msg.sess.State()
	if state.AccountedCostMicroUSD() != 12_345 || state.RetryReserveMicroUSD != 54_321 {
		t.Fatalf("first-turn retry reset budget accounting: %+v", state)
	}
	if start, ok := msg.andThen().(retryStartMsg); !ok || start.prompt != "only question" {
		t.Fatalf("the replay lost the prompt: %+v", msg.andThen())
	}
}

func TestRetryWithoutAFileCheckpointStillUsesADurablePublicationJournal(t *testing.T) {
	m := testModel(t)
	appendTurn(t, m, "question", "answer")
	swap := mustStageRetry(t, m)
	if swap.retry == nil || swap.retry.undo == nil || swap.retry.checkpointKnown {
		t.Fatalf("no-checkpoint retry transaction = %#v", swap.retry)
	}
	journalDir, err := m.app.store.WorkspaceDir(m.app.workspace)
	if err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(journalDir, ".switchboard-retry-transaction")
	if _, err := os.Stat(journalPath); err != nil {
		t.Fatalf("no-checkpoint retry installed no durable journal: %v", err)
	}
	continuation := m.onSessionSwap(swap)
	if continuation == nil {
		t.Fatal("committed no-checkpoint retry lost its continuation")
	}
	if _, err := os.Stat(journalPath); !os.IsNotExist(err) {
		t.Fatalf("committed no-checkpoint retry retained its journal: %v", err)
	}
	m.interrupt()
}

func TestAbnormalTUIExitAbortsDroppedRetryTransaction(t *testing.T) {
	m, path, sourceID, sourcePath := retryFileModel(t)
	m.trackOperationTasks = true
	cmd := cmdRetry(m, "")
	if cmd == nil {
		t.Fatal("retry returned no command")
	}
	wrapped, ok := cmd().(operationTaskResultMsg)
	if !ok {
		t.Fatalf("retry result was not operation-owned: %T", wrapped)
	}
	swap, ok := wrapped.msg.(sessionSwapMsg)
	if !ok || swap.err != nil || swap.sess == nil || swap.retry == nil || swap.retry.undo == nil {
		t.Fatalf("retry did not stage a durable transaction: %#v", wrapped.msg)
	}
	childID := swap.sess.ID()
	childPath := swap.sess.Path()
	journalDir, err := m.app.store.WorkspaceDir(m.app.workspace)
	if err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(journalDir, ".switchboard-retry-transaction")
	if _, err := os.Stat(journalPath); err != nil {
		t.Fatalf("staged retry installed no durable journal: %v", err)
	}

	if err := runTUIProgram(tuiProgramFunc(func() (tea.Model, error) { return m, nil }), m); err != nil {
		t.Fatal(err)
	}
	assertRetryKeptSource(t, m, sourceID, sourcePath, path, "after")
	assertRetainedUnpublishedStage(t, m.app.store, m.app.workspace, childID, childPath)
	if _, err := os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dropped retry retained durable journal %s: %v", journalPath, err)
	}
}

func TestRetryTakesBackTheTurnsFilesWhenTheStackTopMatches(t *testing.T) {
	m := testModel(t)
	m.app.workspace = m.app.loop.Session.State().Workspace
	appendTurn(t, m, "edit the file", "done")

	f := filepath.Join(m.app.workspace, "a.txt")
	if err := os.WriteFile(f, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := checkpoint.NewRecorder()
	rec.BeginTurn(m.app.loop.Session.ID(), 0, "edit the file")
	rec.Record(f)
	if err := os.WriteFile(f, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.app.undo = rec

	msg := mustStageRetry(t, m)
	defer msg.sess.CloseDiscardingStaged()
	if data, _ := os.ReadFile(f); string(data) != "after" {
		t.Fatalf("staging the retry changed the workspace: %q", data)
	}
	m.onSessionSwap(msg)
	m.interrupt()
	if data, _ := os.ReadFile(f); string(data) != "before" {
		t.Fatalf("the turn's file change survived the retry: %q", data)
	}
}

func TestRetryLeavesFilesWhenTheStackTopIsAnotherTurn(t *testing.T) {
	m := testModel(t)
	appendTurn(t, m, "second turn", "done")

	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := checkpoint.NewRecorder()
	rec.BeginTurn("another-session", 0, "an earlier turn")
	rec.Record(f)
	if err := os.WriteFile(f, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.app.undo = rec

	msg := mustStageRetry(t, m)
	defer msg.sess.CloseDiscardingStaged()
	m.onSessionSwap(msg)
	m.interrupt()
	if data, _ := os.ReadFile(f); string(data) != "after" {
		t.Fatalf("retry undid a turn it was not retrying: %q", data)
	}
}

func TestRetryDoesNotUndoAnOlderSameLabelTurnWhenCurrentTurnChangedNothing(t *testing.T) {
	m := testModel(t)
	appendTurn(t, m, "continue", "first answer")

	path := filepath.Join(t.TempDir(), "same-label.txt")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := checkpoint.NewRecorder()
	rec.BeginTurn(m.app.loop.Session.ID(), 0, "continue")
	rec.Record(path)
	if err := os.WriteFile(path, []byte("after first turn"), 0o644); err != nil {
		t.Fatal(err)
	}

	appendTurn(t, m, "continue", "second answer")
	// Beginning the second turn commits the first turn's mutation and opens an
	// authoritative empty scope. Both labels are deliberately identical.
	rec.BeginTurn(m.app.loop.Session.ID(), 2, "continue")
	m.app.undo = rec

	msg := mustStageRetry(t, m)
	defer msg.sess.CloseDiscardingStaged()
	m.onSessionSwap(msg)
	m.interrupt()
	if data, _ := os.ReadFile(path); string(data) != "after first turn" {
		t.Fatalf("retry of the no-op second turn undid the first turn: %q", data)
	}
	if turns := rec.Turns(); len(turns) != 1 || turns[0].Files != 1 {
		t.Fatalf("retry consumed the older turn's checkpoint: %+v", turns)
	}
}

func TestRetryPauseFailureKeepsSourceAndPostImage(t *testing.T) {
	m, path, sourceID, sourcePath := retryFileModel(t)
	m.app.retryPause = func(context.Context, *tuiApp) (func(), error) {
		return nil, errors.New("injected pause failure")
	}

	msg, ok := cmdRetry(m, "")().(sessionSwapMsg)
	if !ok || msg.err == nil || !strings.Contains(msg.err.Error(), "pause failure") {
		t.Fatalf("pause failure result = %#v", msg)
	}
	m.onSessionSwap(msg)
	assertRetryKeptSource(t, m, sourceID, sourcePath, path, "after")
}

func TestRetryForkFailureKeepsSourceAndPostImage(t *testing.T) {
	m, path, sourceID, sourcePath := retryFileModel(t)
	released := false
	m.app.retryPause = func(context.Context, *tuiApp) (func(), error) {
		return func() { released = true }, nil
	}
	m.app.retryFork = func(*session.Store, *session.Session, int) (*session.Session, error) {
		return nil, errors.New("injected fork failure")
	}

	msg, ok := cmdRetry(m, "")().(sessionSwapMsg)
	if !ok || msg.err == nil || !strings.Contains(msg.err.Error(), "fork failure") {
		t.Fatalf("fork failure result = %#v", msg)
	}
	m.onSessionSwap(msg)
	if !released {
		t.Fatal("fork failure did not release the advisor barrier")
	}
	assertRetryKeptSource(t, m, sourceID, sourcePath, path, "after")
}

func TestRetryCancellationAfterForkKeepsSourceAndPostImage(t *testing.T) {
	m, path, sourceID, sourcePath := retryFileModel(t)
	swap := mustStageRetry(t, m)
	m.interrupt()
	if !m.operationCancelling {
		t.Fatal("interrupt did not mark the staged retry cancelled")
	}
	m.onSessionSwap(swap)
	assertRetryKeptSource(t, m, sourceID, sourcePath, path, "after")
}

func TestRetryStaleOperationAfterForkKeepsSourceAndPostImage(t *testing.T) {
	m, path, sourceID, sourcePath := retryFileModel(t)
	swap := mustStageRetry(t, m)
	if !m.finishOperation(swap.operation, false) {
		t.Fatal("could not make the staged retry stale")
	}
	m.onSessionSwap(swap)
	assertRetryKeptSource(t, m, sourceID, sourcePath, path, "after")
}

func TestRetryStaleCheckpointRollsSessionBackToSource(t *testing.T) {
	m, path, sourceID, sourcePath := retryFileModel(t)
	swap := mustStageRetry(t, m)
	if err := os.WriteFile(path, []byte("newer external edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.onSessionSwap(swap)
	assertRetryKeptSource(t, m, sourceID, sourcePath, path, "newer external edit")
}

func TestRetryPublicationFailureRollsSessionAndFilesBack(t *testing.T) {
	m, path, sourceID, sourcePath := retryFileModel(t)
	swap := mustStageRetry(t, m)
	stagedID := swap.sess.ID()
	stagedPath := swap.sess.Path()
	// A marker collision is the publication layer's deterministic fail-closed
	// boundary: Publish may not replace a marker it did not create.
	if err := os.WriteFile(stagedPath+".published", []byte("injected collision"), 0o600); err != nil {
		t.Fatal(err)
	}

	m.onSessionSwap(swap)
	assertRetryKeptSource(t, m, sourceID, sourcePath, path, "after")
	assertRetainedUnpublishedStage(t, m.app.store, m.app.workspace, stagedID, stagedPath)
}

func TestRetryVisibleUncertainPublicationClosesChildWithoutLabellingSource(t *testing.T) {
	t.Run("checkpointed files", func(t *testing.T) {
		m, path, _, sourcePath := retryFileModel(t)
		swap := mustStageRetry(t, m)
		swap.publishDurably = func(*session.Session) (session.PublicationOutcome, error) {
			return session.PublicationOutcome{Visible: true}, errors.New("injected directory sync failure")
		}

		cmd := m.onSessionSwap(swap)
		if cmd == nil || !m.quitting || m.shutdownErr == nil {
			t.Fatalf("uncertain retry did not stop: cmd=%v quitting=%v err=%v", cmd != nil, m.quitting, m.shutdownErr)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != "before" {
			t.Fatalf("visible retry did not keep its committed pre-turn workspace: %q, %v", got, err)
		}
		if retrySourceLabelled(t, sourcePath) {
			t.Fatal("durability-uncertain retry labelled a source that recovery may restore")
		}
		if err := swap.sess.AppendNote("info", "must not append"); err == nil {
			t.Fatal("durability-uncertain retry child remained writable")
		}
	})

	t.Run("no checkpointed files", func(t *testing.T) {
		m := testModel(t)
		appendTurn(t, m, "question", "answer")
		sourcePath := m.app.loop.Session.Path()
		swap := mustStageRetry(t, m)
		swap.publishDurably = func(*session.Session) (session.PublicationOutcome, error) {
			return session.PublicationOutcome{Visible: true}, errors.New("injected directory sync failure")
		}

		cmd := m.onSessionSwap(swap)
		if cmd == nil || !m.quitting || m.shutdownErr == nil {
			t.Fatalf("uncertain retry did not stop: cmd=%v quitting=%v err=%v", cmd != nil, m.quitting, m.shutdownErr)
		}
		if retrySourceLabelled(t, sourcePath) {
			t.Fatal("durability-uncertain retry labelled a source that recovery may restore")
		}
		if err := swap.sess.AppendNote("info", "must not append"); err == nil {
			t.Fatal("durability-uncertain retry child remained writable")
		}
	})
}

func TestRetryChildAdoptionFailureKeepsSourceAndPostImage(t *testing.T) {
	m, path, sourceID, sourcePath := retryFileModel(t)
	swap := mustStageRetry(t, m)
	if err := swap.sess.Close(); err != nil {
		t.Fatal(err)
	}
	m.onSessionSwap(swap)
	assertRetryKeptSource(t, m, sourceID, sourcePath, path, "after")
}

func TestRetryRefusalsSayWhy(t *testing.T) {
	m := testModel(t)
	if msg, ok := cmdRetry(m, "")().(noticeMsg); !ok || !strings.Contains(msg.text, "nothing to retry") {
		t.Errorf("an empty session did not refuse plainly: %+v", msg)
	}

	appendTurn(t, m, "q", "a")
	if msg, ok := cmdRetry(m, "t9")().(noticeMsg); !ok || !strings.Contains(msg.text, "no tier t9") {
		t.Errorf("an unknown tier did not refuse plainly: %+v", msg)
	}
}
