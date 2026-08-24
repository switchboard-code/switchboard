package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

func decodeLines(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimRight(raw, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line is not one JSON object: %q (%v)", line, err)
		}
		out = append(out, ev)
	}
	return out
}

// The whole contract is one complete object per line, so a consumer can read
// it with a line reader and a decoder and nothing else.
func TestEveryStreamedEventIsOneCompleteLine(t *testing.T) {
	var buf bytes.Buffer
	o := newStreamObserver(&buf, agent.NopObserver{})

	o.writeStreamInit("sess-1", "t2", "ollama/local/test", "default")
	o.TextDelta("hello\nworld")
	o.ToolStart(provider.ToolUse{Name: "exec"}, permission.Request{Detail: "go test", Effect: permission.EffectExecute})
	o.ToolEnd(provider.ToolUse{Name: "exec"}, permission.Request{Detail: "go test"},
		tools.Result{IsError: true}, 1500*time.Millisecond)
	o.Notice("warn", "a retry happened")
	o.TurnUsage(session.Usage{Target: "ollama/local/test", Usage: provider.Usage{InputTokens: 10}})

	events := decodeLines(t, buf.String())
	if len(events) != 6 {
		t.Fatalf("got %d events, want one per call:\n%s", len(events), buf.String())
	}

	types := make([]string, len(events))
	for i, ev := range events {
		types[i], _ = ev["type"].(string)
	}
	want := []string{"init", "text", "tool_start", "tool_end", "notice", "usage"}
	for i := range want {
		if types[i] != want[i] {
			t.Errorf("event %d = %q, want %q", i, types[i], want[i])
		}
	}

	// A newline inside the model's text must not become a second line.
	if events[1]["text"] != "hello\nworld" {
		t.Errorf("text event = %v, want the delta intact", events[1]["text"])
	}
	if events[3]["error"] != true {
		t.Errorf("tool_end did not carry the failure: %v", events[3])
	}
	if events[3]["took_ms"].(float64) != 1500 {
		t.Errorf("tool_end took_ms = %v, want the duration", events[3]["took_ms"])
	}
}

// The inner observer is the human-facing transcript, and a scripted run that a
// person is also watching should not have to choose.
func TestStreamingStillFeedsTheTranscript(t *testing.T) {
	inner := &countingObserver{}
	o := newStreamObserver(&bytes.Buffer{}, inner)

	o.TextDelta("a")
	o.ThinkingDelta("b")
	o.Notice("", "c")
	o.ToolBatchEnd(context.Background())

	if inner.text != 1 || inner.thinking != 1 || inner.notice != 1 || inner.batches != 1 {
		t.Errorf("inner observer saw %+v, want every call forwarded", inner)
	}
}

type countingObserver struct {
	text, thinking, notice, batches int
}

func (o *countingObserver) ThinkingDelta(string)                           { o.thinking++ }
func (o *countingObserver) TextDelta(string)                               { o.text++ }
func (o *countingObserver) ToolStart(provider.ToolUse, permission.Request) {}
func (o *countingObserver) ToolEnd(provider.ToolUse, permission.Request, tools.Result, time.Duration) {
}
func (o *countingObserver) ToolBatchEnd(context.Context) { o.batches++ }
func (o *countingObserver) Notice(string, string)        { o.notice++ }
func (o *countingObserver) TurnUsage(session.Usage)      {}

// A consumer that only wants the outcome reads the last line, so the last line
// has to be the report -output json would have printed.
func TestTheLastStreamedLineIsTheResult(t *testing.T) {
	var buf bytes.Buffer
	rep := headlessReport{Result: "done", Outcome: "completed", Session: "sess-1", Tier: "t1"}
	if err := writeStreamResult(&buf, rep); err != nil {
		t.Fatal(err)
	}

	events := decodeLines(t, buf.String())
	if len(events) != 1 {
		t.Fatalf("the result spanned %d lines", len(events))
	}
	if events[0]["type"] != "result" {
		t.Errorf("type = %v, want result", events[0]["type"])
	}
	// The report's own fields have to be present, not nested, or a consumer
	// written against -output json has to be rewritten to read the stream.
	for _, want := range []string{"result", "outcome", "session", "tier"} {
		if _, ok := events[0][want]; !ok {
			t.Errorf("the result line is missing %q: %v", want, events[0])
		}
	}
}

func TestHeadlessStreamWatcherPreservesLiveEventsAndOneFinalResult(t *testing.T) {
	r, _, _ := newOverrideREPL(t, "small")
	var machine bytes.Buffer
	selected := selectPlainObserver("stream-json", &machine, r.out,
		r.loop.Session.ID(), r.tier.ID, string(r.tier.Target.ID()), "plan")
	r.watcher = newWatcher(selected, r.sticky, len(r.config.Tiers)-1, nil)
	r.loop.SetObserver(r.watcher)

	turnErr := r.onceAuthored(context.Background(), "answer once", "answer once")
	if turnErr != nil {
		t.Fatal(turnErr)
	}
	report := buildHeadlessReport(r.loop.Session.State(), r.catalog, r.tier, turnErr)
	if err := writeStreamResult(&machine, report); err != nil {
		t.Fatal(err)
	}

	events := decodeLines(t, machine.String())
	if len(events) < 4 {
		t.Fatalf("headless stream lost live events: %v", events)
	}
	seen := map[string]int{}
	for _, event := range events {
		kind, _ := event["type"].(string)
		seen[kind]++
	}
	if seen["init"] != 1 || seen["text"] == 0 || seen["usage"] != 1 {
		t.Fatalf("typed live events = %v, want init, text, and one usage", seen)
	}
	if seen["result"] != 1 || events[len(events)-1]["type"] != "result" {
		t.Fatalf("final stream contract = %v, want exactly one terminal result", events)
	}
}

func TestPlainObserverLeavesTextAndJSONModesUnchanged(t *testing.T) {
	inner := &countingObserver{}
	for _, output := range []string{"text", "json"} {
		var machine bytes.Buffer
		if got := selectPlainObserver(output, &machine, inner, "s", "t1", "target", "plan"); got != inner {
			t.Fatalf("%s mode replaced its transcript observer", output)
		}
		if machine.Len() != 0 {
			t.Fatalf("%s mode wrote stream output: %q", output, machine.String())
		}
	}
}
