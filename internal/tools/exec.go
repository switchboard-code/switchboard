package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/terminaltext"
)

const maxExecTimeout = 10 * time.Minute

type execTool struct{ r *Registry }

func (t *execTool) Name() string { return "exec" }

func (t *execTool) Description() string {
	return "Run a command with the session's current execution reach. Sandbox is off by default, " +
		"so an approved command can access the host filesystem outside the workspace and the network. " +
		"When a verified sandbox is active, writes are limited to the workspace, temp, and build caches; broad system and outside-home paths remain readable, and network requests are gated. " +
		"Pass either command or script, never both. " +
		"Combined stdout and stderr are returned; output beyond the bounded " +
		"capture is withheld because an incomplete stream cannot be checked safely."
}

func (t *execTool) ParallelSafe() bool { return false }

func (t *execTool) Schema() json.RawMessage {
	scriptDescription := strconv.Quote(scriptSchemaDescription())
	return json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "command": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Program and arguments, run directly with no shell: [\"go\",\"test\",\"./...\"]."
    },
	"script": {"type": "string", "description": %s},
	"network": {"type": "boolean", "description": "Request internet access when a sandbox is active. With the default sandbox-off posture, approved commands already have the host's full network reach regardless of this hint."},
    "timeout_seconds": {"type": "integer", "description": "Wall-clock limit. Defaults to 120. Ignored when background is true."},
    "background": {"type": "boolean", "description": "Start the command and return immediately with a handle instead of waiting. For a server, a watch build, or anything meant to keep running while you work. Read its output and stop it with the proc tool."}
  }
}`, scriptDescription))
}

type execInput struct {
	Command        []string `json:"command"`
	Script         string   `json:"script"`
	Shell          bool     `json:"shell"`
	Network        bool     `json:"network"`
	TimeoutSeconds int      `json:"timeout_seconds"`
	Background     bool     `json:"background"`
}

func (t *execTool) Plan(input json.RawMessage) (Plan, error) {
	var in execInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Plan{}, fmt.Errorf("exec: %w", err)
	}
	// Shell is retired but still decoded. A resumed session replays its own
	// earlier tool_use blocks and a model mimics its own history, so silently
	// ignoring the old field would run a pipeline as argv[0]. Refusing it with
	// the new shape shown is what the model corrected off twice in one
	// recorded session: prose about a shape is not the shape.
	if in.Shell {
		return Plan{}, fmt.Errorf(`exec: shell is retired, script takes the whole script: {"script": %q}`,
			strings.Join(in.Command, " "))
	}
	if (len(in.Command) == 0) == (in.Script == "") {
		return Plan{}, fmt.Errorf(`exec: pass exactly one of command or script, `+
			`for example {"command": ["go","test","./..."]} or {"script": %s}`,
			strconv.Quote(scriptExample()))
	}
	argv, shell := in.Command, false
	if in.Script != "" {
		argv, shell = []string{in.Script}, true
	}

	timeout := time.Duration(in.TimeoutSeconds) * time.Second
	if in.TimeoutSeconds <= 0 {
		timeout = execution.DefaultTimeout
	}
	if timeout > maxExecTimeout {
		return Plan{}, fmt.Errorf("exec: timeout_seconds %d exceeds the %s ceiling",
			in.TimeoutSeconds, maxExecTimeout)
	}

	policy := t.r.execution.CommandPolicy(in.Network)
	requestPolicy := policy
	runPolicy := policy
	// Keep the auditable request and the executable closure on distinct
	// backing arrays. Permission/reviewer code may inspect Request.Argv, but
	// cannot rewrite what Run will execute after approval.
	runArgv := append([]string(nil), argv...)
	requestArgv := append([]string(nil), runArgv...)
	return Plan{
		Request: permission.Request{
			Tool:      t.Name(),
			Effect:    permission.EffectExecute,
			Path:      ".", // command working directory, relative to the workspace
			Argv:      requestArgv,
			Shell:     shell,
			Network:   in.Network,
			Execution: &requestPolicy,
		},
		Run: func(ctx context.Context) (Result, error) {
			release, err := t.r.execution.Hold(runPolicy, in.Network)
			if err != nil {
				return errorf("exec: %v", err)
			}
			defer release()
			if in.Background {
				return t.startBackground(runArgv, shell, runPolicy)
			}
			res, err := execution.Run(ctx, execution.Command{
				Argv:    runArgv,
				Shell:   shell,
				Dir:     t.r.root,
				Timeout: timeout,
				// The confinement and the permission decision come from one
				// capability, so a command approved as contained cannot then run
				// unconfined.
				Confine: runPolicy.Confinement,
				Policy: execution.Policy{
					Workspace: t.r.root,
					Network:   runPolicy.Network,
				},
			})
			if err != nil {
				// A context error is the user cancelling, which the loop handles
				// as a cancellation rather than a tool failure.
				if ctx.Err() != nil {
					return Result{}, err
				}
				return errorf("could not run %s: %v", Describe(argv, shell), err)
			}
			return execResult(res), nil
		},
	}, nil
}

// execResult renders a command's outcome. A timeout is reported as a tool error
// rather than aborting the turn: the model is the one who chose the command and
// is best placed to decide whether to narrow it, retry, or give up (§10.3).
func execResult(res execution.Result) Result {
	var b strings.Builder
	if res.Truncated {
		b.WriteString("[command output withheld because it exceeded the bounded capture]")
		b.WriteByte('\n')
	} else if res.Output != "" {
		b.WriteString(res.Output)
		if !strings.HasSuffix(res.Output, "\n") {
			b.WriteByte('\n')
		}
	}

	switch {
	case res.TimedOut:
		fmt.Fprintf(&b, "[timed out after %s; the process group was terminated]", res.Duration.Round(time.Millisecond))
		return Result{Content: b.String(), IsError: true}
	case res.ExitCode != 0:
		fmt.Fprintf(&b, "[exit status %d]", res.ExitCode)
		return Result{Content: b.String(), IsError: true}
	}

	if b.Len() == 0 {
		return Result{Content: "[no output, exit status 0]"}
	}
	return Result{Content: strings.TrimSuffix(b.String(), "\n")}
}

// Describe renders a command for a permission prompt or a log line. It is not a
// quoting round trip and must never be fed back to a shell.
func Describe(argv []string, shell bool) string {
	if shell {
		return describeScript(strings.Join(argv, " "))
	}
	quoted := make([]string, len(argv))
	for i, a := range argv {
		escaped := terminaltext.Escape(a)
		if a == "" || strings.ContainsAny(a, " \t\n\"'\\$`;|&()<>*?[]{}!~") {
			quoted[i] = strconv.Quote(escaped)
		} else {
			quoted[i] = escaped
		}
	}
	return strings.Join(quoted, " ")
}
