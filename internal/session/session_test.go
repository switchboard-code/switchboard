package session

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func TestAppendUsageRejectsNegativeAndOverflowingTelemetry(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "anthropic/first-party/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.AppendUsage(Usage{Usage: provider.Usage{InputTokens: -1}}); err == nil {
		t.Fatal("negative provider usage was appended")
	}
	if err := sess.AppendUsage(Usage{Usage: provider.Usage{InputTokens: math.MaxInt}}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendUsage(Usage{Usage: provider.Usage{InputTokens: 1}}); err == nil {
		t.Fatal("overflowing provider usage was appended")
	}
	state := sess.State()
	if state.Calls != 1 || state.Usage.InputTokens != math.MaxInt {
		t.Fatalf("rejected usage changed state: %+v", state)
	}
}

func TestReplayRejectsNegativeUsageFromTheLog(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "anthropic/first-party/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	path := sess.Path()
	sess.mu.Lock()
	err = sess.append(RecordUsage, Usage{CallID: "hostile", Usage: provider.Usage{OutputTokens: -1}})
	sess.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadState(path); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("negative durable usage was accepted on replay: %v", err)
	}
}

func TestUsageRecordsKeepParameterizedTargetIdentity(t *testing.T) {
	store, workspace := newStore(t)
	base := provider.RouteTarget{Provider: "openai", Surface: "api", ModelID: "same-model"}
	withMax := base
	withMax.Params.MaxOutputTokens = 2_048
	temperature := 0.2
	withTemperature := base
	withTemperature.Params.Temperature = &temperature

	sess, err := store.Create(workspace, base.ID(), "rev")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []provider.RouteTarget{withMax, withTemperature} {
		if err := sess.AppendUsage(Usage{Target: string(target.ID()), Usage: provider.Usage{InputTokens: 1}}); err != nil {
			t.Fatal(err)
		}
	}
	path := sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	usages, err := ReadUsages(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(usages) != 2 || usages[0].Target != string(withMax.ID()) || usages[1].Target != string(withTemperature.ID()) {
		t.Fatalf("usage target attribution collapsed parameter variants: %+v", usages)
	}
}

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	store, err := NewStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	return store, workspace
}

func TestReplayReconstructsState(t *testing.T) {
	store, workspace := newStore(t)

	sess, err := store.Create(workspace, "ollama/local/qwen3.5:9b-mlx", "test-revision")
	if err != nil {
		t.Fatal(err)
	}
	id := sess.ID()

	messages := []provider.Message{
		provider.UserText("add a test"),
		{Role: provider.RoleAssistant, Content: []provider.Block{
			provider.Thinking{Text: "look at the file first"},
			provider.ToolUse{ID: "call_1", Name: "read", Input: []byte(`{"path":"main.go"}`)},
		}},
		{Role: provider.RoleTool, Content: []provider.Block{
			provider.ToolResult{ToolUseID: "call_1", Name: "read", Content: "package main"},
		}},
	}
	for _, m := range messages {
		if err := sess.AppendMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := sess.AppendUsage(Usage{
		Target:   "ollama/local/qwen3.5:9b-mlx",
		Usage:    provider.Usage{InputTokens: 283, OutputTokens: 57},
		Attempts: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	got := reopened.State()
	if len(got.Messages) != len(messages) {
		t.Fatalf("replayed %d messages, want %d", len(got.Messages), len(messages))
	}
	if got.Messages[1].ToolUses()[0].Name != "read" {
		t.Errorf("tool call did not survive replay: %+v", got.Messages[1])
	}
	if got.Usage.InputTokens != 283 || got.Usage.OutputTokens != 57 {
		t.Errorf("usage = %+v", got.Usage)
	}
	if got.Calls != 1 {
		t.Errorf("calls = %d, want 1", got.Calls)
	}
	if got.Target != "ollama/local/qwen3.5:9b-mlx" {
		t.Errorf("target = %q", got.Target)
	}
	if got.Workspace != workspace {
		t.Errorf("workspace = %q, want %q", got.Workspace, workspace)
	}
	if reopened.TruncatedBytes() != 0 {
		t.Errorf("clean log reported %d truncated bytes", reopened.TruncatedBytes())
	}
}

// A process killed mid-write leaves a frame with no terminator. Replay must
// recover everything before it rather than refusing to load the session.
func TestTornFinalWriteRecovers(t *testing.T) {
	store, workspace := newStore(t)

	sess, err := store.Create(workspace, "t", "test-revision")
	if err != nil {
		t.Fatal(err)
	}
	id, path := sess.ID(), sess.Path()
	for _, text := range []string{"first", "second", "third"} {
		if err := sess.AppendMessage(provider.UserText(text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate the kill: chop the last record in half.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lastNewline := strings.LastIndexByte(string(data[:len(data)-1]), '\n')
	torn := append([]byte{}, data[:lastNewline+1]...)
	torn = append(torn, data[lastNewline+1:len(data)-10]...)
	if err := os.WriteFile(path, torn, 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(id)
	if err != nil {
		t.Fatalf("a torn tail must not make the session unloadable: %v", err)
	}
	defer reopened.Close()

	got := reopened.State()
	if len(got.Messages) != 2 {
		t.Fatalf("recovered %d messages, want the 2 that were fully written", len(got.Messages))
	}
	if got.Messages[1].Text() != "second" {
		t.Errorf("last recovered message = %q, want second", got.Messages[1].Text())
	}
	if reopened.TruncatedBytes() == 0 {
		t.Error("lost bytes must be reported, not swallowed")
	}

	// The truncation is durable, so appending after recovery produces a log that
	// replays cleanly the next time.
	if err := reopened.AppendMessage(provider.UserText("fourth")); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.TruncatedBytes() != 0 {
		t.Error("reopening a repaired log truncated again")
	}
	if n := len(again.State().Messages); n != 3 {
		t.Errorf("got %d messages after appending post-recovery, want 3", n)
	}
}

func TestCompleteCorruptMiddleFrameIsRefusedAndPreserved(t *testing.T) {
	store, workspace := newStore(t)

	sess, err := store.Create(workspace, "t", "test-revision")
	if err != nil {
		t.Fatal(err)
	}
	id, path := sess.ID(), sess.Path()
	for _, text := range []string{"keep before", "tamper middle", "keep after"} {
		if err := sess.AppendMessage(provider.UserText(text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Flip bytes inside a complete middle record's payload, preserving its
	// framing and the complete valid record after it. Only the checksum catches
	// the change; treating every checksum failure as a torn final write would
	// silently discard both records.
	tampered := strings.Replace(string(data), "tamper middle", "TAMPER MIDDLE", 1)
	if tampered == string(data) {
		t.Fatal("test fixture did not contain the expected payload")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Open(id); !errors.Is(err, ErrCorruptRecord) {
		t.Fatalf("Open error = %v, want ErrCorruptRecord", err)
	}
	if fork, err := store.Fork(id, 1); !errors.Is(err, ErrCorruptRecord) {
		if fork != nil {
			_ = fork.Close()
		}
		t.Fatalf("Fork error = %v, want ErrCorruptRecord", err)
	}

	readers := map[string]func() error{
		"state": func() error { _, err := ReadState(path); return err },
		"races": func() error { _, err := ReadRaces(path); return err },
		"permissions": func() error {
			_, err := ReadPermissions(path)
			return err
		},
		"timeline":   func() error { _, err := ReadTimeline(path); return err },
		"usage":      func() error { _, err := ReadUsages(path); return err },
		"file edits": func() error { _, err := ReadFileEdits(path); return err },
		"accounting": func() error { _, err := ReadAccountingLedger(path); return err },
		"turn costs": func() error { _, err := ReadTurnCosts(path); return err },
	}
	for name, read := range readers {
		if err := read(); !errors.Is(err, ErrCorruptRecord) {
			t.Errorf("%s error = %v, want ErrCorruptRecord", name, err)
		}
	}
	infos, err := store.List(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != id || !infos[0].Health.CorruptRecord {
		t.Fatalf("preserved corrupt session disappeared from inventory: %+v", infos)
	}
	if infos[0].Health.RecoveredCorruptTail || infos[0].Health.Messages != 1 {
		t.Fatalf("corrupt-session health overclaimed recovery or crossed the bad frame: %+v", infos[0].Health)
	}
	all, err := store.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all[workspace]) != 1 || !all[workspace][0].Health.CorruptRecord {
		t.Fatalf("ListAll hid the preserved corrupt session: %+v", all[workspace])
	}
	if latest, err := store.Latest(workspace); !errors.Is(err, ErrCorruptRecord) {
		if latest != nil {
			_ = latest.Close()
		}
		t.Fatalf("Latest error = %v, want the preserved corruption refusal", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != tampered {
		t.Fatal("corruption refusal modified or truncated the original log")
	}
}

func TestLatestSkipsANewerIntegrityBlockedSession(t *testing.T) {
	store, workspace := newStore(t)
	healthy, err := store.Create(workspace, "test/local/healthy", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := healthy.AppendMessage(provider.UserText("resume the healthy session")); err != nil {
		t.Fatal(err)
	}
	healthyID, healthyPath := healthy.ID(), healthy.Path()
	if err := healthy.Close(); err != nil {
		t.Fatal(err)
	}

	blocked, err := store.Create(workspace, "test/local/blocked", "rev")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"blocked prefix", "damage middle", "blocked suffix"} {
		if err := blocked.AppendMessage(provider.UserText(text)); err != nil {
			t.Fatal(err)
		}
	}
	blockedID, blockedPath := blocked.ID(), blocked.Path()
	if err := blocked.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(blockedPath)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), "damage middle", "DAMAGE MIDDLE", 1)
	if tampered == string(raw) {
		t.Fatal("blocked fixture did not contain its middle payload")
	}
	if err := os.WriteFile(blockedPath, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(healthyPath, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(blockedPath, now, now); err != nil {
		t.Fatal(err)
	}

	infos, err := store.List(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[0].ID != blockedID || !infos[0].Health.CorruptRecord {
		t.Fatalf("inventory did not expose the newer blocked session first: %+v", infos)
	}
	latest, err := store.Latest(workspace)
	if err != nil {
		t.Fatalf("healthy continuation was hidden behind newer corruption: %v", err)
	}
	defer latest.Close()
	if latest.ID() != healthyID {
		t.Fatalf("Latest resumed %s, want healthy session %s", latest.ID(), healthyID)
	}
}

func TestSchemaFromNewerBinaryIsRefused(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "t", "test-revision")
	if err != nil {
		t.Fatal(err)
	}
	id, path := sess.ID(), sess.Path()
	sess.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bumped := strings.Replace(string(data), fmt.Sprintf("%s %d", magic, SchemaVersion), magic+" 99", 1)
	if err := os.WriteFile(path, []byte(bumped), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = store.Open(id)
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("err = %v, want ErrSchemaTooNew; a best-effort parse would drop records silently", err)
	}
}

func TestSchemaOneIsDurablyUpgradedBeforeBudgetRecords(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "t", "test-revision")
	if err != nil {
		t.Fatal(err)
	}
	id, path := sess.ID(), sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	v1 := strings.Replace(string(data), fmt.Sprintf("%s %d", magic, SchemaVersion), magic+" 1", 1)
	if err := os.WriteFile(path, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(id)
	if err != nil {
		t.Fatalf("opening schema 1 for migration: %v", err)
	}
	upgraded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(upgraded), fmt.Sprintf("%s %d\n", magic, SchemaVersion)) {
		t.Fatalf("schema header was not upgraded before append: %q", strings.SplitN(string(upgraded), "\n", 2)[0])
	}
	if _, err := reopened.BeginBudgetAttempt(1_234); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if got := again.State().RetryReserveMicroUSD; got != 1_234 {
		t.Fatalf("budget record appended after migration replayed reserve %d, want 1234", got)
	}
}

func TestFailedAppendPoisonsSessionAgainstLaterBudgetWAL(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "t", "test-revision")
	if err != nil {
		t.Fatal(err)
	}
	// Closing the descriptor underneath Session deterministically forces the
	// same Write failure path as a short or failed filesystem append.
	if err := sess.f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.BeginBudgetAttempt(111); !errors.Is(err, ErrSessionPoisoned) {
		t.Fatalf("first failed WAL err = %v, want ErrSessionPoisoned", err)
	}
	if _, err := sess.BeginBudgetAttempt(222); !errors.Is(err, ErrSessionPoisoned) {
		t.Fatalf("later WAL behind failed append err = %v, want ErrSessionPoisoned", err)
	}
	if err := sess.AppendNote("info", "must not land behind a torn frame"); !errors.Is(err, ErrSessionPoisoned) {
		t.Fatalf("later non-budget append err = %v, want ErrSessionPoisoned", err)
	}
}

func TestSecondWriterIsRefused(t *testing.T) {
	store, workspace := newStore(t)

	first, err := store.Create(workspace, "t", "test-revision")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	_, err = store.Open(first.ID())
	if !errors.Is(err, ErrSessionLocked) {
		t.Fatalf("err = %v, want ErrSessionLocked; interleaved frames would corrupt the log", err)
	}

	first.Close()
	reopened, err := store.Open(first.ID())
	if err != nil {
		t.Fatalf("the lock must release on close: %v", err)
	}
	reopened.Close()
}

func TestLatestAndListOrdering(t *testing.T) {
	store, workspace := newStore(t)

	var ids []string
	for range 3 {
		s, err := store.Create(workspace, "t", "test-revision")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AppendMessage(provider.UserText("hello")); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.ID())
		s.Close()
	}

	infos, err := store.List(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 3 {
		t.Fatalf("listed %d sessions, want 3", len(infos))
	}
	if infos[0].ID != ids[2] {
		t.Errorf("most recent = %s, want %s", infos[0].ID, ids[2])
	}

	latest, err := store.Latest(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer latest.Close()
	if latest.ID() != ids[2] {
		t.Errorf("Latest = %s, want %s", latest.ID(), ids[2])
	}
}

// A filesystem can stamp several files in the same tick, and on a fast one it
// routinely does. When it happens, mtime carries no ordering and the id is the
// only thing left to sort by, so `--continue` resuming the right session comes
// down to whether the id encodes when it was made.
//
// The timestamps are forced here rather than raced for, so the tiebreak is
// exercised on every platform instead of only on whichever machine happens to
// be fast enough.
func TestOrderingSurvivesIdenticalTimestamps(t *testing.T) {
	store, workspace := newStore(t)

	var ids []string
	for range 3 {
		s, err := store.Create(workspace, "t", "test-revision")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AppendMessage(provider.UserText("hello")); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.ID())
		s.Close()
	}

	infos, err := store.List(workspace)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	for _, info := range infos {
		if err := os.Chtimes(info.Path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}

	infos, err = store.List(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 3 {
		t.Fatalf("listed %d sessions, want 3", len(infos))
	}
	if infos[0].ID != ids[2] {
		t.Errorf("with identical timestamps the most recent is %s, want %s; "+
			"the id has to carry the ordering that mtime lost", infos[0].ID, ids[2])
	}

	latest, err := store.Latest(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer latest.Close()
	if latest.ID() != ids[2] {
		t.Errorf("Latest = %s, want %s", latest.ID(), ids[2])
	}
}

// A crash between creating the file and syncing its header leaves a stub that
// cannot replay. `--continue` must step over it to the last real session rather
// than failing on a file that never held a conversation.
func TestHeaderlessStubIsSkipped(t *testing.T) {
	store, workspace := newStore(t)

	real, err := store.Create(workspace, "t", "test-revision")
	if err != nil {
		t.Fatal(err)
	}
	if err := real.AppendMessage(provider.UserText("keep me")); err != nil {
		t.Fatal(err)
	}
	realID := real.ID()
	real.Close()

	stub := filepath.Join(filepath.Dir(real.Path()), "20991231T235959-deadbeef.log")
	if err := os.WriteFile(stub, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	latest, err := store.Latest(workspace)
	if err != nil {
		t.Fatalf("a headerless stub must not break --continue: %v", err)
	}
	defer latest.Close()
	if latest.ID() != realID {
		t.Errorf("resumed %s, want the last real session %s", latest.ID(), realID)
	}
}

func TestLatestWithNoSessions(t *testing.T) {
	store, workspace := newStore(t)
	if _, err := store.Latest(workspace); !errors.Is(err, ErrNoSessions) {
		t.Errorf("err = %v, want ErrNoSessions", err)
	}
}

func TestIncompleteMessageSurvivesReplay(t *testing.T) {
	store, workspace := newStore(t)

	sess, err := store.Create(workspace, "t", "test-revision")
	if err != nil {
		t.Fatal(err)
	}
	id := sess.ID()
	partial := provider.Message{
		Role:       provider.RoleAssistant,
		Incomplete: true,
		Content:    []provider.Block{provider.Text{Text: "cut off mid"}},
	}
	if err := sess.AppendMessage(partial); err != nil {
		t.Fatal(err)
	}
	sess.Close()

	reopened, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	got := reopened.State().Messages
	if len(got) != 1 || !got[0].Incomplete {
		t.Fatalf("incomplete flag did not survive replay: %+v", got)
	}
}

func TestSessionsAreNotWorldReadable(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "t", "test-revision")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ownerOnly, err := privateSessionFileIsOwnerOnly(sess.f)
	if err != nil {
		t.Fatal(err)
	}
	if !ownerOnly {
		t.Error("session log holds prompts and code but is not owner-only")
	}
}

// §8.4's training signal is written from ordinary sessions rather than only
// from eval runs, because a corpus of deliberate measurements is a corpus of
// tasks somebody thought to write down, and the distribution that matters is
// the one people work in.
func TestRouteRecordsSurviveReplay(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "t/s/m", "rev")
	if err != nil {
		t.Fatal(err)
	}

	route := Route{
		TurnDepth: 2, PromptTokens: 80, ContextTokens: 120, PriorFailures: 1, TestFailures: 1, TestsInvolved: true,
		Tier: "t1", Target: "t/s/m", Source: "heuristic",
		Rationale: "following a test failure", Escalations: 1,
		EndedOn: "t/s/other", Outcome: "completed", Verified: true, FailureKind: RouteFailureContext,
		Usage: provider.Usage{InputTokens: 100, OutputTokens: 20}, WallTimeMS: 4200,
	}
	if err := sess.AppendRoute(route); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("hello")); err != nil {
		t.Fatal(err)
	}
	timeline, err := ReadTimeline(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(timeline) < 1 || timeline[0].Route == nil ||
		timeline[0].Route.VerificationStatus != RouteVerificationPassed ||
		!timeline[0].Route.VerificationRan || timeline[0].Route.TestFailures != 1 || timeline[0].Route.ContextTokens != 120 ||
		timeline[0].Route.FailureKind != RouteFailureContext {
		t.Fatalf("route verification telemetry = %+v", timeline)
	}
	id := sess.ID()
	sess.Close()

	// A route record carries no conversation state, so replay has to skip it
	// without either losing the messages around it or refusing the log.
	reopened, err := store.Open(id)
	if err != nil {
		t.Fatalf("a log containing route records could not be replayed: %v", err)
	}
	defer reopened.Close()

	if got := len(reopened.State().Messages); got != 1 {
		t.Errorf("replayed %d messages, want the one that was written", got)
	}
}

func TestRouteUsageWindowCorrelatesTurnCallsAndExcludesOverlappingBackground(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "t/s/m", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	window := sess.BeginUsageWindow()
	advisorDone := make(chan error, 1)
	go func() {
		_, appendErr := sess.AppendUsageRecord(Usage{
			Purpose: UsagePurposeAdvisor, Usage: provider.Usage{InputTokens: 700, OutputTokens: 70}, CostMicroUSD: 7000,
		})
		advisorDone <- appendErr
	}()
	first, err := sess.AppendUsageRecord(Usage{
		Purpose: UsagePurposeTurn, Usage: provider.Usage{InputTokens: 100, OutputTokens: 10}, CostMicroUSD: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-advisorDone; err != nil {
		t.Fatal(err)
	}
	if _, err := sess.AppendUsageRecord(Usage{
		Purpose: UsagePurposeCompact, Usage: provider.Usage{InputTokens: 800, OutputTokens: 80}, CostMicroUSD: 8000,
	}); err != nil {
		t.Fatal(err)
	}
	second, err := sess.AppendUsageRecord(Usage{
		Purpose: UsagePurposeTurn, Usage: provider.Usage{InputTokens: 200, OutputTokens: 20}, CostMicroUSD: 2000,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Caller-supplied aggregate values are deliberately ignored. The exact
	// durable purpose=turn receipts own route accounting.
	if err := sess.AppendRouteWithUsage(window, Route{
		Tier: "t1", Target: "t/s/m", Outcome: "completed",
		Usage: provider.Usage{InputTokens: math.MaxInt}, CostMicroUSD: -1,
	}); err != nil {
		t.Fatal(err)
	}
	timeline, err := ReadTimeline(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	var route *Route
	for _, item := range timeline {
		if item.Route != nil {
			route = item.Route
		}
	}
	if route == nil {
		t.Fatal("route record missing")
	}
	if route.Usage != (provider.Usage{InputTokens: 300, OutputTokens: 30}) || route.CostMicroUSD != 3000 {
		t.Fatalf("route accounting = usage %+v cost %d", route.Usage, route.CostMicroUSD)
	}
	if len(route.UsageCallIDs) != 2 || route.UsageCallIDs[0] != first.CallID || route.UsageCallIDs[1] != second.CallID {
		t.Fatalf("route CallIDs = %#v, want %q and %q", route.UsageCallIDs, first.CallID, second.CallID)
	}
	durable, err := ReadUsages(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[string]int)
	for _, usage := range durable {
		counts[usage.CallID]++
	}
	for _, id := range route.UsageCallIDs {
		if counts[id] != 1 {
			t.Fatalf("route CallID %q resolves to %d durable usages", id, counts[id])
		}
	}
}

func TestRouteUsageWindowRejectsAnotherSession(t *testing.T) {
	store, workspace := newStore(t)
	first, err := store.Create(workspace, "t/s/first", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := store.Create(workspace, "t/s/second", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := second.AppendRouteWithUsage(first.BeginUsageWindow(), Route{}); err == nil {
		t.Fatal("cross-session route cursor was accepted")
	}
}

func TestRetryReserveSurvivesReplayWithoutBecomingUsage(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "anthropic/first-party/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendRetryReserve(42_000); err != nil {
		t.Fatal(err)
	}
	id := sess.ID()
	sess.Close()

	reopened, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	state := reopened.State()
	if state.RetryReserveMicroUSD != 42_000 {
		t.Fatalf("retry reserve = %d, want 42000", state.RetryReserveMicroUSD)
	}
	if state.CostMicroUSD != 0 || state.Calls != 0 || state.Usage != (provider.Usage{}) {
		t.Fatalf("retry reserve was misreported as successful usage: %+v", state)
	}
}

func TestPendingBudgetAttemptSurvivesCrashAndRestart(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "anthropic/first-party/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	id := sess.ID()
	attempt, err := sess.BeginBudgetAttempt(42_000)
	if err != nil {
		t.Fatal(err)
	}
	if attempt == "" {
		t.Fatal("pending attempt has no durable identity")
	}
	if got := sess.State().RetryReserveMicroUSD; got != 42_000 {
		t.Fatalf("live pending reserve = %d, want 42000", got)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	state := reopened.State()
	if state.RetryReserveMicroUSD != 42_000 {
		t.Fatalf("replayed pending reserve = %d, want 42000", state.RetryReserveMicroUSD)
	}
	if state.Calls != 0 || state.Usage != (provider.Usage{}) || state.AccountedCostMicroUSD() != 0 {
		t.Fatalf("pending attempt invented successful usage: %+v", state)
	}
}

func TestBudgetSettlementSeparatesObservedExternalCostFromUsage(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "anthropic/first-party/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := sess.BeginBudgetAttempt(100_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.SettleBudgetAttempt(attempt, BudgetOutcomeSucceeded, 12_345); err != nil {
		t.Fatal(err)
	}
	state := sess.State()
	if state.RetryReserveMicroUSD != 0 || state.ExternalCostMicroUSD != 12_345 || state.AccountedCostMicroUSD() != 12_345 {
		t.Fatalf("settled external accounting = %+v", state)
	}
	if state.Calls != 0 || state.Usage != (provider.Usage{}) {
		t.Fatalf("external charge became fake provider usage: %+v", state)
	}
	id := sess.ID()
	sess.Close()
	reopened, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.State(); got.ExternalCostMicroUSD != 12_345 || got.RetryReserveMicroUSD != 0 {
		t.Fatalf("replayed settlement = %+v", got)
	}
}

func TestFailedBudgetSettlementKeepsTheWholeBound(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "anthropic/first-party/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := sess.BeginBudgetAttempt(55_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.SettleBudgetAttempt(attempt, BudgetOutcomeFailed, 0); err != nil {
		t.Fatal(err)
	}
	if got := sess.State().RetryReserveMicroUSD; got != 55_000 {
		t.Fatalf("failed settlement reserve = %d, want 55000", got)
	}
	if err := sess.SettleBudgetAttempt(attempt, BudgetOutcomeSucceeded, 0); err == nil {
		t.Fatal("a settled attempt was allowed to settle twice")
	}
}

func TestSettlementAppendFailureCannotForgetPendingAttempt(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "anthropic/first-party/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := sess.BeginBudgetAttempt(88_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sess.SettleBudgetAttempt(attempt, BudgetOutcomeSucceeded, 11_000); err == nil {
		t.Fatal("settlement unexpectedly appended to a closed log")
	}
	state := sess.State()
	if state.RetryReserveMicroUSD != 88_000 || state.ExternalCostMicroUSD != 0 {
		t.Fatalf("failed append forgot or partially settled reservation: %+v", state)
	}
}

func TestBudgetTransferIsAtomicDistinctFromUsageAndDeduplicated(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "anthropic/first-party/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendBudgetTransfer("race:one", 23_000, 77_000); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendBudgetTransfer("race:one", 23_000, 77_000); err == nil {
		t.Fatal("duplicate transfer was charged twice")
	}
	state := sess.State()
	if state.ExternalCostMicroUSD != 23_000 || state.RetryReserveMicroUSD != 77_000 {
		t.Fatalf("transfer accounting = %+v", state)
	}
	if state.Calls != 0 || state.Usage != (provider.Usage{}) {
		t.Fatalf("transfer invented provider telemetry: %+v", state)
	}
}
