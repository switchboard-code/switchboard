package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

func TestStreamDraftKnownAllowanceAcceptsExactByteBudget(t *testing.T) {
	const allowance = 1024
	limit := assistantDraftByteLimit(allowance)
	events := []provider.Event{
		{Type: provider.EventTextDelta, Index: 0, Text: strings.Repeat("x", limit)},
		{Type: provider.EventDone, StopReason: provider.StopEndTurn},
	}
	h := newHarness(t, permission.ModeDefault, scriptTurn{events: events})
	h.loop.OutputAllowance = func(provider.RouteTarget, int) int { return allowance }

	if err := h.loop.Turn(context.Background(), "use the exact allowance"); err != nil {
		t.Fatalf("exact %d-byte draft was refused: %v", limit, err)
	}
	state := h.sess.State()
	if len(state.Messages) != 2 || state.Messages[1].Incomplete || len(state.Messages[1].Text()) != limit {
		t.Fatalf("exact-limit state = %#v", state.Messages)
	}
}

func TestStreamDraftKnownAllowanceRefusesPlusOneWithoutStoringRejectedEvent(t *testing.T) {
	const allowance = 1024
	limit := assistantDraftByteLimit(allowance)
	const rejected = "\x00"
	events := []provider.Event{
		{Type: provider.EventTextDelta, Index: 0, Text: strings.Repeat("x", limit)},
		{Type: provider.EventTextDelta, Index: 0, Text: rejected},
	}
	h := newHarness(t, permission.ModeDefault, scriptTurn{events: events})
	h.loop.OutputAllowance = func(provider.RouteTarget, int) int { return allowance }

	err := h.loop.Turn(context.Background(), "cross the allowance")
	if !errors.Is(err, ErrStreamDraftLimit) {
		t.Fatalf("plus-one error = %v, want ErrStreamDraftLimit", err)
	}
	if strings.Contains(err.Error(), rejected) {
		t.Fatalf("limit error echoed rejected provider content: %v", err)
	}
	state := h.sess.State()
	if len(state.Messages) != 2 || !state.Messages[1].Incomplete || len(state.Messages[1].Text()) != limit {
		t.Fatalf("refused stream state = %#v", state.Messages)
	}
	raw, readErr := os.ReadFile(h.sess.Path())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if bytes.Contains(raw, []byte(`\u0000`)) {
		t.Fatal("rejected provider event reached the session WAL")
	}
	if got := bytes.Count(raw, []byte(assistantDraftLimitAuditMessage)); got != 1 {
		t.Fatalf("limit audit markers = %d, want exactly one", got)
	}

	path, id := h.sess.Path(), h.sess.ID()
	if closeErr := h.sess.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	store, openErr := session.NewStore(filepath.Dir(filepath.Dir(path)))
	if openErr != nil {
		t.Fatal(openErr)
	}
	reopened, openErr := store.Open(id)
	if openErr != nil {
		t.Fatalf("reopening limit-refused stream: %v", openErr)
	}
	defer reopened.Close()
	replayed := reopened.State()
	if len(replayed.Messages) != 2 || !replayed.Messages[1].Incomplete || len(replayed.Messages[1].Text()) != limit {
		t.Fatalf("replayed refused stream = %#v", replayed.Messages)
	}
	projected := provider.ReplayRequest(provider.Request{Messages: replayed.Messages})
	if len(projected.Messages) != 1 || projected.Messages[0].Role != provider.RoleUser {
		t.Fatalf("refused draft reached provider replay: %#v", projected.Messages)
	}
	if repaired, reconcileErr := reopened.ReconcileInterruptedToolCalls(); reconcileErr != nil || repaired != 0 {
		t.Fatalf("limit note was mistaken for a completed assistant tool call: repaired=%d err=%v", repaired, reconcileErr)
	}
}

func TestStreamDraftReportedTokenAllowanceExactAndPlusOne(t *testing.T) {
	const allowance = 3
	for _, test := range []struct {
		name       string
		reported   int
		wantLimit  bool
		incomplete bool
	}{
		{name: "exact", reported: allowance},
		{name: "plus-one", reported: allowance + 1, wantLimit: true, incomplete: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := []provider.Event{
				{Type: provider.EventTextDelta, Index: 0, Text: "ok"},
				{Type: provider.EventDone, StopReason: provider.StopEndTurn, Usage: provider.Usage{OutputTokens: test.reported}},
			}
			h := newHarness(t, permission.ModeDefault, scriptTurn{events: events})
			h.loop.OutputAllowance = func(provider.RouteTarget, int) int { return allowance }
			err := h.loop.Turn(context.Background(), "verify reported output tokens")
			if test.wantLimit != errors.Is(err, ErrStreamDraftLimit) {
				t.Fatalf("Turn error = %v, want limit=%v", err, test.wantLimit)
			}
			state := h.sess.State()
			if len(state.Messages) != 2 || state.Messages[1].Incomplete != test.incomplete || state.Messages[1].Text() != "ok" {
				t.Fatalf("reported-token state = %#v", state.Messages)
			}
			raw, readErr := os.ReadFile(h.sess.Path())
			if readErr != nil {
				t.Fatal(readErr)
			}
			wantNotes := 0
			if test.wantLimit {
				wantNotes = 1
			}
			if got := bytes.Count(raw, []byte(assistantDraftLimitAuditMessage)); got != wantNotes {
				t.Fatalf("limit audit markers = %d, want %d", got, wantNotes)
			}
		})
	}
}

func TestStreamDraftEventBudgetStopsEndlessTinyDeltasBeforeMutation(t *testing.T) {
	draft := newStreamDraft(nil, NopObserver{}, &messageBuilder{}, 0)
	event := provider.Event{Type: provider.EventTextDelta, Index: 0, Text: "x"}
	for i := 0; i < assistantDraftMaxEvents; i++ {
		if _, err := draft.admit(event); err != nil {
			t.Fatalf("event %d of exact budget was refused: %v", i+1, err)
		}
	}
	beforeBytes, beforeEvents := draft.totalBytes, draft.events
	if _, err := draft.admit(event); !errors.Is(err, ErrStreamDraftLimit) {
		t.Fatalf("event budget +1 error = %v, want ErrStreamDraftLimit", err)
	}
	if draft.totalBytes != beforeBytes || draft.events != beforeEvents || len(draft.pending) != 0 {
		t.Fatalf("refused tiny delta mutated draft: bytes=%d events=%d pending=%d", draft.totalBytes, draft.events, len(draft.pending))
	}
}

func TestStreamDraftDistinctBlockBudgetRefusesPlusOne(t *testing.T) {
	draft := newStreamDraft(nil, NopObserver{}, &messageBuilder{}, 0)
	for i := 0; i < assistantDraftMaxBlocks; i++ {
		event := provider.Event{Type: provider.EventTextDelta, Index: i, Text: "x"}
		if _, err := draft.admit(event); err != nil {
			t.Fatalf("block %d of exact budget was refused: %v", i+1, err)
		}
	}
	beforeBytes, beforeEvents := draft.totalBytes, draft.events
	_, err := draft.admit(provider.Event{
		Type: provider.EventTextDelta, Index: assistantDraftMaxBlocks, Text: "x",
	})
	if !errors.Is(err, ErrStreamDraftLimit) {
		t.Fatalf("block budget +1 error = %v, want ErrStreamDraftLimit", err)
	}
	if len(draft.blocks) != assistantDraftMaxBlocks || draft.totalBytes != beforeBytes || draft.events != beforeEvents {
		t.Fatalf("refused block mutated draft: blocks=%d bytes=%d events=%d", len(draft.blocks), draft.totalBytes, draft.events)
	}
}

func TestStreamDraftRefusesGiantToolArgumentsAndCancelsTheStream(t *testing.T) {
	input := append(json.RawMessage{'"'}, bytes.Repeat([]byte{'a'}, assistantDraftMaxToolInputBytes)...)
	input = append(input, '"')
	use := provider.ToolUse{ID: "must-not-persist", Name: "read", Input: input}
	probe := &draftBudgetProbeProvider{events: []provider.Event{{Type: provider.EventToolUse, Index: 0, ToolUse: &use}}}
	h := newHarness(t, permission.ModeDefault)
	h.loop.Provider = probe

	err := h.loop.Turn(context.Background(), "reject the giant tool call")
	if !errors.Is(err, ErrStreamDraftLimit) {
		t.Fatalf("giant tool argument error = %v, want ErrStreamDraftLimit", err)
	}
	if probe.stream == nil {
		t.Fatal("provider stream was never opened")
	}
	if !probe.stream.closed {
		t.Fatal("limit-refused provider stream was not closed")
	}
	if probe.ctx == nil {
		t.Fatal("provider did not receive a stream context")
	}
	if !errors.Is(probe.ctx.Err(), context.Canceled) {
		t.Fatalf("limit-refused provider context error = %v, want context.Canceled", probe.ctx.Err())
	}
	state := h.sess.State()
	if len(state.Messages) != 1 || state.Messages[0].Role != provider.RoleUser {
		t.Fatalf("giant first event created assistant content: %#v", state.Messages)
	}
	raw, readErr := os.ReadFile(h.sess.Path())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if bytes.Contains(raw, []byte(use.ID)) || len(raw) > 32<<10 {
		t.Fatalf("giant tool argument reached bounded WAL: id_present=%v bytes=%d", bytes.Contains(raw, []byte(use.ID)), len(raw))
	}
	if got := bytes.Count(raw, []byte(assistantDraftLimitAuditMessage)); got != 1 {
		t.Fatalf("limit audit markers = %d, want exactly one", got)
	}
}

type draftBudgetProbeProvider struct {
	events []provider.Event
	ctx    context.Context
	stream *draftBudgetProbeStream
}

func (*draftBudgetProbeProvider) Name() string { return "draft-budget-probe" }

func (p *draftBudgetProbeProvider) Stream(ctx context.Context, _ provider.RouteTarget, _ provider.Request) (provider.EventStream, error) {
	p.ctx = ctx
	p.stream = &draftBudgetProbeStream{events: p.events}
	return p.stream, nil
}

func (*draftBudgetProbeProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}

func (*draftBudgetProbeProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true}, nil
}

type draftBudgetProbeStream struct {
	events []provider.Event
	next   int
	closed bool
}

func (s *draftBudgetProbeStream) Next() (provider.Event, error) {
	if s.next >= len(s.events) {
		return provider.Event{}, io.EOF
	}
	event := s.events[s.next]
	s.next++
	return event, nil
}

func (s *draftBudgetProbeStream) Close() error {
	s.closed = true
	return nil
}

func TestStreamDraftCheckpointingIsLinearAndLogicallyInvisibleOnSuccess(t *testing.T) {
	const chunks = 128
	const chunk = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ--0123456789abcdefghijklmnopqrstuvwxyz"
	events := make([]provider.Event, 0, chunks+1)
	for range chunks {
		events = append(events, provider.Event{Type: provider.EventTextDelta, Index: 0, Text: chunk})
	}
	events = append(events, provider.Event{Type: provider.EventDone, StopReason: provider.StopEndTurn})
	h := newHarness(t, permission.ModeDefault, scriptTurn{events: events})

	if err := h.loop.Turn(context.Background(), "stream a long answer"); err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat(chunk, chunks)
	state := h.sess.State()
	if len(state.Messages) != 2 || state.Messages[1].Incomplete || state.Messages[1].Text() != want {
		t.Fatalf("successful checkpoints changed the logical conversation: messages=%d incomplete=%v text=%d bytes",
			len(state.Messages), state.Messages[len(state.Messages)-1].Incomplete, len(state.Messages[len(state.Messages)-1].Text()))
	}
	raw, err := os.ReadFile(h.sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	checkpoints := bytes.Count(raw, []byte(`"type":"assistant_draft"`))
	if checkpoints < 2 || checkpoints > 12 {
		t.Fatalf("checkpoint count=%d, want bounded batching rather than one fsync per %d deltas", checkpoints, chunks)
	}
	if len(raw) > len(want)*6 {
		t.Fatalf("session log grew %d bytes for %d streamed bytes; delta checkpoints must remain linear", len(raw), len(want))
	}
}

func TestStreamDeltaIsNotObserverVisibleWhenItsCheckpointFails(t *testing.T) {
	h := newHarness(t, permission.ModeDefault)
	h.loop.Provider = &closeBeforeDraftProvider{sess: h.sess}

	if err := h.loop.Turn(context.Background(), "show nothing undurable"); err == nil {
		t.Fatal("turn succeeded after its session descriptor was closed")
	}
	h.obs.mu.Lock()
	visible := h.obs.text.String()
	h.obs.mu.Unlock()
	if visible != "" {
		t.Fatalf("observer saw output whose checkpoint failed: %q", visible)
	}
	if got := h.sess.State().Messages; len(got) != 1 || got[0].Role != provider.RoleUser {
		t.Fatalf("failed checkpoint published a draft in memory: %#v", got)
	}
}

func TestRetriedStreamClosesEachDraftAndFiltersItFromLaterRequests(t *testing.T) {
	h := newHarness(t, permission.ModeDefault,
		scriptTurn{
			events: []provider.Event{{Type: provider.EventTextDelta, Index: 0, Text: "first attempt partial"}},
			endErr: provider.ErrStreamIncomplete,
		},
		textTurn("retry completed"),
		textTurn("later turn completed"),
	)
	h.loop.MaxAttempts = 2
	if err := h.loop.Turn(context.Background(), "do the work"); err != nil {
		t.Fatal(err)
	}
	firstTurn := h.sess.State().Messages
	if len(firstTurn) != 3 || !firstTurn[1].Incomplete || firstTurn[1].Text() != "first attempt partial" || firstTurn[2].Incomplete || firstTurn[2].Text() != "retry completed" {
		t.Fatalf("retry drafts were not closed as distinct logical messages: %#v", firstTurn)
	}
	if err := h.loop.Turn(context.Background(), "continue"); err != nil {
		t.Fatal(err)
	}
	if len(h.provider.requests) != 3 {
		t.Fatalf("provider requests=%d, want failed attempt, retry, later turn", len(h.provider.requests))
	}
	for _, message := range h.provider.requests[2].Messages {
		if message.Incomplete || strings.Contains(message.Text(), "first attempt partial") {
			t.Fatalf("closed retry draft reached a later provider request: %#v", h.provider.requests[2].Messages)
		}
	}
}

type closeBeforeDraftProvider struct{ sess *session.Session }

func (*closeBeforeDraftProvider) Name() string { return "close-before-draft" }
func (p *closeBeforeDraftProvider) Stream(context.Context, provider.RouteTarget, provider.Request) (provider.EventStream, error) {
	if err := p.sess.Close(); err != nil {
		return nil, err
	}
	return &scriptedStream{turn: scriptTurn{events: []provider.Event{
		{Type: provider.EventTextDelta, Index: 0, Text: "must stay hidden"},
	}}}, nil
}
func (*closeBeforeDraftProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}
func (*closeBeforeDraftProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true}, nil
}

const (
	crashDraftHelperEnv = "SWITCHBOARD_DRAFT_CRASH_HELPER"
	crashDraftStoreEnv  = "SWITCHBOARD_DRAFT_CRASH_STORE"
	crashDraftWorkEnv   = "SWITCHBOARD_DRAFT_CRASH_WORKSPACE"
	crashDraftIDEnv     = "SWITCHBOARD_DRAFT_CRASH_ID_FILE"
	crashDraftReadyEnv  = "SWITCHBOARD_DRAFT_CRASH_READY_FILE"
)

func TestSIGKILLPreservesEveryObserverVisibleAssistantDelta(t *testing.T) {
	if os.Getenv(crashDraftHelperEnv) == "1" {
		runDraftCrashHelper(t)
		return
	}

	root := t.TempDir()
	storeRoot := filepath.Join(root, "sessions")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	idFile := filepath.Join(root, "session-id")
	readyFile := filepath.Join(root, "observer-visible")
	cmd := exec.Command(os.Args[0], "-test.run=^TestSIGKILLPreservesEveryObserverVisibleAssistantDelta$")
	cmd.Env = append(os.Environ(),
		crashDraftHelperEnv+"=1",
		crashDraftStoreEnv+"="+storeRoot,
		crashDraftWorkEnv+"="+workspace,
		crashDraftIDEnv+"="+idFile,
		crashDraftReadyEnv+"="+readyFile,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	defer func() {
		if !killed && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper never made the durable delta observer-visible: %s", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	killed = true
	if err := cmd.Wait(); err == nil {
		t.Fatal("SIGKILL helper exited successfully instead of being killed")
	}

	idBytes, err := os.ReadFile(idFile)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(strings.TrimSpace(string(idBytes)))
	if err != nil {
		t.Fatalf("opening the killed process's session: %v\nhelper stderr: %s", err, stderr.String())
	}
	defer reopened.Close()
	state := reopened.State()
	const visible = "VISIBLE BEFORE SIGKILL"
	if len(state.Messages) != 2 || !state.Messages[1].Incomplete || state.Messages[1].Text() != visible {
		t.Fatalf("killed stream state=%#v, want one recoverable incomplete assistant delta", state.Messages)
	}
	request := provider.ReplayRequest(provider.Request{Messages: state.Messages})
	if len(request.Messages) != 1 || strings.Contains(request.Messages[0].Text(), visible) {
		t.Fatalf("killed stream draft reached provider replay: %#v", request.Messages)
	}
}

func runDraftCrashHelper(t *testing.T) {
	store, err := session.NewStore(os.Getenv(crashDraftStoreEnv))
	if err != nil {
		t.Fatal(err)
	}
	workspace := os.Getenv(crashDraftWorkEnv)
	sess, err := store.Create(workspace, "scripted/local/crash", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv(crashDraftIDEnv), []byte(sess.ID()), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(workspace, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	loop := &Loop{
		Provider: &crashDraftProvider{},
		Target:   provider.RouteTarget{Provider: "scripted", Surface: "local", ModelID: "crash"},
		Tools:    registry,
		Perms:    permission.NewEngine(permission.ModeDefault, execution.Capability{}),
		Session:  sess,
		Observer: crashDraftObserver{ready: os.Getenv(crashDraftReadyEnv)},
	}
	if err := loop.Turn(context.Background(), "start streaming"); err != nil {
		t.Fatalf("crash helper turn returned before it could be killed: %v", err)
	}
}

type crashDraftProvider struct{}

func (*crashDraftProvider) Name() string { return "crash-draft" }
func (*crashDraftProvider) Stream(context.Context, provider.RouteTarget, provider.Request) (provider.EventStream, error) {
	return &crashDraftStream{}, nil
}
func (*crashDraftProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}
func (*crashDraftProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true}, nil
}

type crashDraftStream struct{ sent bool }

func (s *crashDraftStream) Next() (provider.Event, error) {
	if !s.sent {
		s.sent = true
		return provider.Event{Type: provider.EventTextDelta, Index: 0, Text: "VISIBLE BEFORE SIGKILL"}, nil
	}
	select {}
}
func (*crashDraftStream) Close() error { return nil }

type crashDraftObserver struct {
	NopObserver
	ready string
}

func (o crashDraftObserver) TextDelta(text string) {
	if err := os.WriteFile(o.ready, []byte(fmt.Sprintf("visible:%s", text)), 0o600); err != nil {
		panic(err)
	}
}
