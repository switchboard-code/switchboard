package main

// -output stream-json: the run as it happens, one JSON object per line.
//
// A -p run is opaque until it ends, and then it answers with the model's prose
// in a result string. A script driving Switchboard therefore has two bad
// options: wait with no idea whether anything is happening, and then parse
// English. Neither is a thing you can build on, which is the same argument the
// REPL and -output json were built on and the same one that stops here.
//
// Every line is a complete JSON object with a "type", and the last line of a
// run is always the "result" object -output json already prints, so a consumer
// that only wants the outcome reads the last line and one that wants progress
// reads all of them. Nothing is emitted that the observer did not report: this
// is a rendering of the loop's event stream and invents no events of its own.
//
// It writes to stdout and nothing else does. The human-facing transcript keeps
// going to stderr, so a run can be watched and consumed at the same time.

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/switchboard-code/switchboard/internal/agent"

	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// streamEvent is one line. The fields are a union across event types rather
// than a per-type struct because a consumer switching on "type" wants one
// shape to decode into, and every absent field is omitted rather than zeroed.
type streamEvent struct {
	Type string `json:"type"`

	Text  string `json:"text,omitempty"`
	Level string `json:"level,omitempty"`

	Tool   string `json:"tool,omitempty"`
	Detail string `json:"detail,omitempty"`
	Effect string `json:"effect,omitempty"`
	Error  bool   `json:"error,omitempty"`
	TookMS int64  `json:"took_ms,omitempty"`

	Session string `json:"session,omitempty"`
	Tier    string `json:"tier,omitempty"`
	Target  string `json:"target,omitempty"`
	Mode    string `json:"mode,omitempty"`

	Usage *provider.Usage `json:"usage,omitempty"`
}

// streamObserver renders the loop's observer stream as JSON lines.
//
// It wraps another observer rather than replacing it, because the human-facing
// transcript on stderr is still wanted: a scripted run that a person is also
// watching should not have to choose.
type streamObserver struct {
	inner agent.Observer

	mu sync.Mutex
	w  io.Writer
	// enc writes compact single-line JSON. A pretty-printed event would span
	// lines, and the whole contract here is one object per line.
	enc *json.Encoder
}

func newStreamObserver(w io.Writer, inner agent.Observer) *streamObserver {
	enc := json.NewEncoder(w)
	return &streamObserver{inner: inner, w: w, enc: enc}
}

// selectPlainObserver is the one output-mode switch for a headless/REPL
// surface. The routing watcher must wrap what this returns: wrapping the raw
// renderer instead would overwrite stream-json after its init line and drop
// every typed live event.
func selectPlainObserver(output string, w io.Writer, inner agent.Observer, sessionID, tier, target, mode string) agent.Observer {
	if output != "stream-json" {
		return inner
	}
	stream := newStreamObserver(w, inner)
	stream.writeStreamInit(sessionID, tier, target, mode)
	return stream
}

// emit is the only writer. Tool callbacks may be concurrent when a batch is
// parallel-safe, and two events interleaved mid-line would produce output no
// consumer can parse.
func (o *streamObserver) emit(ev streamEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	_ = o.enc.Encode(ev)
}

// writeStreamInit is the first line: what this run is, before it does
// anything. A consumer that attaches to the stream knows which session and
// which rung it is watching without waiting for the result.
func (o *streamObserver) writeStreamInit(sessionID, tier, target, mode string) {
	o.emit(streamEvent{Type: "init", Session: sessionID, Tier: tier, Target: target, Mode: mode})
}

func (o *streamObserver) ThinkingDelta(text string) {
	o.inner.ThinkingDelta(text)
	o.emit(streamEvent{Type: "thinking", Text: text})
}

func (o *streamObserver) TextDelta(text string) {
	o.inner.TextDelta(text)
	o.emit(streamEvent{Type: "text", Text: text})
}

func (o *streamObserver) ToolStart(call provider.ToolUse, req permission.Request) {
	o.inner.ToolStart(call, req)
	o.emit(streamEvent{
		Type: "tool_start", Tool: call.Name, Detail: req.Detail, Effect: string(req.Effect),
	})
}

func (o *streamObserver) ToolEnd(call provider.ToolUse, req permission.Request, res tools.Result, took time.Duration) {
	o.inner.ToolEnd(call, req, res, took)
	o.emit(streamEvent{
		Type: "tool_end", Tool: call.Name, Detail: req.Detail,
		Error: res.IsError, TookMS: took.Milliseconds(),
	})
}

func (o *streamObserver) ToolBatchEnd(ctx context.Context) { o.inner.ToolBatchEnd(ctx) }

func (o *streamObserver) Notice(level, text string) {
	o.inner.Notice(level, text)
	o.emit(streamEvent{Type: "notice", Level: level, Text: text})
}

func (o *streamObserver) TurnUsage(u session.Usage) {
	o.inner.TurnUsage(u)
	usage := u.Usage
	o.emit(streamEvent{Type: "usage", Target: u.Target, Usage: &usage})
}

// writeStreamResult prints the terminal object. It is the same report -output
// json prints, tagged so a consumer reading a stream can recognize the end
// without counting.
func writeStreamResult(w io.Writer, rep headlessReport) error {
	line := struct {
		Type string `json:"type"`
		headlessReport
	}{Type: "result", headlessReport: rep}
	enc := json.NewEncoder(w)
	return enc.Encode(line)
}
