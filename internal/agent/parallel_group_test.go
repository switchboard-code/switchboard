package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/tools"
)

func TestExecutionGateLetsCanceledWaiterLeave(t *testing.T) {
	gate := NewExecutionGate()
	release, err := gate.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := gate.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting gate error = %v", err)
	}
	release()
	next, err := gate.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	next()
}

func TestParallelBatchGroupOverlapsAndJoinsInCallOrder(t *testing.T) {
	group := newBarrierGroupTool(2)
	h := newHarness(t, permission.ModeDefault,
		toolTurn(
			use("first-call", group.Name(), `{"value":"first"}`),
			use("second-call", group.Name(), `{"value":"second"}`),
		),
		textTurn("joined"),
	)
	if err := h.loop.Tools.AddExternal(group); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	turnDone := make(chan error, 1)
	go func() { turnDone <- h.loop.Turn(ctx, "run both") }()
	select {
	case <-group.release:
		// Both calls reached the barrier concurrently. Waiting on this event
		// distinguishes a scheduling delay from a serialized-call deadlock.
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("parallel group did not reach its two-call barrier")
	}
	var err error
	select {
	case err = <-turnDone:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("parallel group did not settle after releasing its barrier")
	}
	if err != nil {
		t.Fatal(err)
	}
	if group.peak.Load() != 2 {
		t.Fatalf("parallel group peak = %d, want 2", group.peak.Load())
	}
	toolMessage := h.messages()[2]
	first := toolMessage.Content[0].(provider.ToolResult)
	second := toolMessage.Content[1].(provider.ToolResult)
	if first.ToolUseID != "first-call" || first.Content != "first" ||
		second.ToolUseID != "second-call" || second.Content != "second" {
		t.Fatalf("barrier results lost provider order: %+v", toolMessage.Content)
	}
}

func TestParallelBatchGroupDoesNotOverlapOrdinaryRead(t *testing.T) {
	var groupedRunning atomic.Bool
	group := &batchGroupTool{
		name: "opaque-task",
		run: func(ctx context.Context, value string) (tools.Result, error) {
			groupedRunning.Store(true)
			defer groupedRunning.Store(false)
			select {
			case <-time.After(50 * time.Millisecond):
				return tools.Result{Content: value}, nil
			case <-ctx.Done():
				return tools.Result{}, ctx.Err()
			}
		},
	}
	read := &observingParallelRead{name: "observed-read", groupedRunning: &groupedRunning}
	h := newHarness(t, permission.ModeDefault,
		toolTurn(
			use("opaque", group.Name(), `{"value":"done"}`),
			use("read", read.Name(), `{}`),
		),
		textTurn("done"),
	)
	if err := h.loop.Tools.AddExternal(group); err != nil {
		t.Fatal(err)
	}
	if err := h.loop.Tools.AddExternal(read); err != nil {
		t.Fatal(err)
	}
	if err := h.loop.Turn(context.Background(), "keep order"); err != nil {
		t.Fatal(err)
	}
	if read.overlapped.Load() {
		t.Fatal("opaque parallel group overlapped an ordinary read")
	}
}

type batchGroupTool struct {
	name string
	run  func(context.Context, string) (tools.Result, error)
}

func (t *batchGroupTool) Name() string             { return t.name }
func (t *batchGroupTool) Description() string      { return "test grouped tool" }
func (t *batchGroupTool) ParallelSafe() bool       { return false }
func (t *batchGroupTool) ParallelBatchKey() string { return "test-group" }
func (t *batchGroupTool) Schema() json.RawMessage  { return json.RawMessage(`{"type":"object"}`) }
func (t *batchGroupTool) Plan(raw json.RawMessage) (tools.Plan, error) {
	var input struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return tools.Plan{}, err
	}
	return tools.Plan{
		Request: permission.Request{Tool: t.Name(), Effect: permission.EffectRead},
		Run:     func(ctx context.Context) (tools.Result, error) { return t.run(ctx, input.Value) },
	}, nil
}

type barrierGroupTool struct {
	*batchGroupTool
	want    int
	mu      sync.Mutex
	started int
	release chan struct{}
	active  atomic.Int32
	peak    atomic.Int32
}

func newBarrierGroupTool(want int) *barrierGroupTool {
	t := &barrierGroupTool{want: want, release: make(chan struct{})}
	t.batchGroupTool = &batchGroupTool{name: "grouped", run: t.runBarrier}
	return t
}

func (t *barrierGroupTool) runBarrier(ctx context.Context, value string) (tools.Result, error) {
	now := t.active.Add(1)
	defer t.active.Add(-1)
	for old := t.peak.Load(); now > old && !t.peak.CompareAndSwap(old, now); old = t.peak.Load() {
	}
	t.mu.Lock()
	t.started++
	if t.started == t.want {
		close(t.release)
	}
	t.mu.Unlock()
	select {
	case <-t.release:
		return tools.Result{Content: value}, nil
	case <-ctx.Done():
		return tools.Result{}, ctx.Err()
	}
}

type observingParallelRead struct {
	name           string
	groupedRunning *atomic.Bool
	overlapped     atomic.Bool
}

func (t *observingParallelRead) Name() string            { return t.name }
func (t *observingParallelRead) Description() string     { return "test read" }
func (t *observingParallelRead) ParallelSafe() bool      { return true }
func (t *observingParallelRead) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *observingParallelRead) Plan(json.RawMessage) (tools.Plan, error) {
	return tools.Plan{
		Request: permission.Request{Tool: t.Name(), Effect: permission.EffectRead},
		Run: func(context.Context) (tools.Result, error) {
			if t.groupedRunning.Load() {
				t.overlapped.Store(true)
			}
			return tools.Result{Content: "read"}, nil
		},
	}, nil
}
