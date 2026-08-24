package tools

// proc: the three verbs a background command needs after it is started.
//
// exec with background set answers "start this and come back to it", which is
// the shape the synchronous tool could not express at all: a dev server, a
// watch build, a migration that outlasts a turn. Everything after that —
// what has it printed, what is still running, stop it — is a different
// question about a process rather than a request to run one, and folding both
// into exec's schema would make each harder for a model to use correctly.
//
// It carries the execute effect even though it starts nothing. A stop signals
// a process group and a read returns whatever that process wrote, both of
// which are reaching into something running on this host with this account's
// reach; pricing that as a read because no new process appears would be the
// permission engine describing a posture the user does not have.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
)

// maxProcOutput bounds one read. The capture itself is already bounded; this
// bounds what a single call puts in the context, because a server that has
// been logging for ten minutes has more to say than a turn can afford.
const maxProcOutput = 16 << 10

type procTool struct{ r *Registry }

func (t *procTool) Name() string { return "proc" }

func (t *procTool) Description() string {
	return "Inspect and stop the commands you started with exec's background flag. " +
		"list shows every one this session started and whether it is still running; " +
		"read returns what one has printed so far, without consuming it; " +
		"stop ends one and everything it started. A background command is killed " +
		"after an hour and when the session ends, so a server does not outlive the work."
}

// ParallelSafe is false for the reason exec's is: a stop changes what other
// calls in the same batch would observe.
func (t *procTool) ParallelSafe() bool { return false }

func (t *procTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": ["list", "read", "stop"], "description": "What to do."},
    "id": {"type": "string", "description": "Which background command, as exec reported it. Required for read and stop."}
  },
  "required": ["action"],
  "additionalProperties": false
}`)
}

type procInput struct {
	Action string `json:"action"`
	ID     string `json:"id"`
}

func (t *procTool) Plan(input json.RawMessage) (Plan, error) {
	var in procInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Plan{}, fmt.Errorf("proc: %v", err)
	}
	switch in.Action {
	case "list":
	case "read", "stop":
		if strings.TrimSpace(in.ID) == "" {
			return Plan{}, fmt.Errorf("proc: %s needs the id exec reported", in.Action)
		}
	default:
		return Plan{}, fmt.Errorf("proc: action %q is not list, read, or stop", in.Action)
	}

	detail := in.Action
	if in.ID != "" {
		detail += " " + in.ID
	}
	return Plan{
		Request: permission.Request{
			Tool:   t.Name(),
			Effect: permission.EffectExecute,
			Path:   ".",
			Argv:   []string{"proc", in.Action, in.ID},
			Detail: detail,
		},
		Run: func(context.Context) (Result, error) {
			set := t.r.background
			if set == nil {
				return errorf("proc: this surface starts no background commands")
			}
			switch in.Action {
			case "list":
				return Result{Content: renderProcList(set.List())}, nil
			case "read":
				text, truncated, status, err := set.Output(in.ID)
				if err != nil {
					return errorf("proc: %v", err)
				}
				return Result{Content: renderProcRead(status, text, truncated)}, nil
			default:
				status, err := set.Stop(in.ID)
				if err != nil {
					return errorf("proc: %v", err)
				}
				return Result{Content: fmt.Sprintf("%s stopped: %s", status.ID, describeProc(status))}, nil
			}
		},
	}, nil
}

// startBackground is exec's background branch, here so the two tools that
// share a process set also share how one is described.
func (t *execTool) startBackground(argv []string, shell bool, policy execution.CommandPolicy) (Result, error) {
	set := t.r.background
	if set == nil {
		return errorf("exec: this surface cannot start background commands")
	}
	// Starting what you cannot stop is a leak with extra steps. A restricted
	// agent that was granted exec without proc gets the synchronous tool it
	// asked for and a reason, rather than a process it has no verb for.
	if _, ok := t.r.Get("proc"); !ok {
		return errorf("exec: background needs the proc tool to read and stop what it starts, and this agent was not granted it; run the command in the foreground")
	}
	status, err := set.Start(context.Background(), execution.Command{
		Argv:    argv,
		Shell:   shell,
		Dir:     t.r.root,
		Confine: policy.Confinement,
		Policy: execution.Policy{
			Workspace: t.r.root,
			Network:   policy.Network,
		},
	})
	if err != nil {
		return errorf("exec: %v", err)
	}
	return Result{Content: fmt.Sprintf(
		"%s started in the background: %s\n"+
			"Read what it prints with proc {\"action\":\"read\",\"id\":\"%s\"} and end it with "+
			"proc {\"action\":\"stop\",\"id\":\"%s\"}. It is killed after an hour and when the session ends.",
		status.ID, Describe(argv, shell), status.ID, status.ID)}, nil
}

func renderProcList(statuses []execution.BackgroundStatus) string {
	if len(statuses) == 0 {
		return "No background commands have been started in this session."
	}
	var b strings.Builder
	for _, status := range statuses {
		fmt.Fprintf(&b, "%s  %s  %s\n", status.ID, describeProc(status), Describe(status.Argv, status.Shell))
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderProcRead(status execution.BackgroundStatus, text string, truncated bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n", status.ID, describeProc(status))
	if truncated {
		b.WriteString("Output was withheld because it exceeded the bounded capture; an incomplete stream cannot be checked safely.")
		return b.String()
	}
	// Scan the complete captured component before selecting the recent tail.
	// Cutting first can shorten a credential below the scanner's issuer length
	// floor while leaving most of it visible to the next provider.
	text = credential.Redact(text, credential.ScanPrompt(text))
	text = strings.ToValidUTF8(text, "�")
	if len(text) > maxProcOutput {
		// The tail, not the head: what a running process just printed is what
		// the reader wants, and the head of a server log is its banner.
		start := len(text) - maxProcOutput
		for start < len(text) && !utf8.ValidString(text[start:]) {
			start++
		}
		text = "…" + text[start:]
	}
	if strings.TrimSpace(text) == "" {
		b.WriteString("It has printed nothing yet.")
		return b.String()
	}
	b.WriteString(text)
	return b.String()
}

func describeProc(status execution.BackgroundStatus) string {
	if status.Running {
		return fmt.Sprintf("running for %s", time.Since(status.Started).Round(time.Second))
	}
	switch {
	case status.TimedOut:
		return "killed at the one-hour ceiling"
	case status.Killed:
		return "stopped"
	case status.ExitCode == 0:
		return "exited cleanly"
	default:
		return fmt.Sprintf("exited with status %d", status.ExitCode)
	}
}
