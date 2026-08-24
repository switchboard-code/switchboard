// Package hooks runs user-configured commands at the seams of a tool call.
//
// A pre_tool hook runs after permission resolves and before the tool does,
// and a non-zero exit blocks the call with the hook's output as the reason
// the model reads. A post_tool hook runs after, and whatever it prints is
// appended to the result, so a formatter that rewrote the file can say so to
// the model that wrote it. Hooks are the user's own automation — the same
// standing as a git hook — so they run unconfined and unprompted; which is
// exactly why a repository's hooks file needs the trust grant before it is
// read at all, while ~/.switchboard's always is.
package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

const FileName = "hooks.toml"

const maxHookFileBytes = 1 << 20

const (
	defaultTimeout = 30 * time.Second
	maxHookOutput  = 4 << 10
)

type Event string

const (
	PreTool  Event = "pre_tool"
	PostTool Event = "post_tool"
)

// Hook is one configured command. Run is a shell string, because a hook is
// the user speaking in their own shell's terms, not model output.
type Hook struct {
	Event          Event
	Tools          []string
	Run            string
	TimeoutSeconds int
}

func (h Hook) matches(tool string) bool {
	if len(h.Tools) == 0 {
		return true
	}
	for _, t := range h.Tools {
		if t == tool {
			return true
		}
	}
	return false
}

func (h Hook) timeout() time.Duration {
	if h.TimeoutSeconds > 0 {
		return time.Duration(h.TimeoutSeconds) * time.Second
	}
	return defaultTimeout
}

// Set is every loaded hook, bound to the workspace they run in.
type Set struct {
	workspace string
	hooks     []Hook
}

func (s *Set) Empty() bool { return s == nil || len(s.hooks) == 0 }

// Hooks lists the loaded hooks for display.
func (s *Set) Hooks() []Hook {
	if s == nil {
		return nil
	}
	return append([]Hook(nil), s.hooks...)
}

type hookEntry struct {
	Tools          []string `toml:"tools"`
	Run            string   `toml:"run"`
	TimeoutSeconds int      `toml:"timeout_seconds"`
}

// Load reads one hooks file into a set for the given workspace. A missing
// file is an empty set. An unknown event name is an error rather than a
// silently dead table, because a typo in "pre_tool" would otherwise disable
// the gate its author believes is standing.
func Load(path, workspace string) (*Set, error) {
	return LoadRooted(filepath.Dir(path), filepath.Base(path), workspace)
}

// LoadRooted binds the declaration to an authority root. Repository callers
// pass the workspace; a renamed parent or symlink can therefore never redirect
// a pre-trust inspection outside the checkout.
func LoadRooted(root, name, workspace string) (*Set, error) {
	return loadRootedWithHook(root, name, workspace, nil)
}

func loadRootedWithHook(root, name, workspace string, beforeOpen func()) (*Set, error) {
	var file struct {
		Hooks map[string][]hookEntry `toml:"hooks"`
	}
	path := filepath.Join(root, filepath.FromSlash(name))
	data, err := rootedfs.ReadFileWithHook(root, name, maxHookFileBytes, beforeOpen)
	if err != nil {
		if os.IsNotExist(err) {
			return &Set{workspace: workspace}, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if _, err := toml.Decode(string(data), &file); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	events := make([]string, 0, len(file.Hooks))
	for event := range file.Hooks {
		events = append(events, event)
	}
	sort.Strings(events)

	s := &Set{workspace: workspace}
	for _, event := range events {
		switch Event(event) {
		case PreTool, PostTool:
		default:
			return nil, fmt.Errorf("%s: unknown hook event %q: want pre_tool or post_tool", path, event)
		}
		for i, e := range file.Hooks[event] {
			if strings.TrimSpace(e.Run) == "" {
				return nil, fmt.Errorf("%s: %s hook %d has no run command", path, event, i+1)
			}
			s.hooks = append(s.hooks, Hook{
				Event:          Event(event),
				Tools:          e.Tools,
				Run:            e.Run,
				TimeoutSeconds: e.TimeoutSeconds,
			})
		}
	}
	return s, nil
}

// Merge combines sets in order; nil entries are skipped.
func Merge(workspace string, sets ...*Set) *Set {
	merged := &Set{workspace: workspace}
	for _, s := range sets {
		if s != nil {
			merged.hooks = append(merged.hooks, s.hooks...)
		}
	}
	return merged
}

// payload is what a hook reads on stdin. The same facts ride SB_HOOK_*
// variables for one-liners that do not want to parse JSON.
type payload struct {
	Event     Event    `json:"event"`
	Tool      string   `json:"tool"`
	Path      string   `json:"path,omitempty"`
	Argv      []string `json:"argv,omitempty"`
	Shell     bool     `json:"shell,omitempty"`
	Detail    string   `json:"detail,omitempty"`
	Workspace string   `json:"workspace"`
	IsError   bool     `json:"is_error,omitempty"`
	Result    string   `json:"result,omitempty"`
}

// PreTool runs every matching pre_tool hook in file order. The first
// non-zero exit blocks the call, and blocking is also what a timeout does: a
// gate that fails open the moment it hangs is not a gate.
func (s *Set) PreTool(ctx context.Context, req permission.Request) (string, bool) {
	if s.Empty() {
		return "", false
	}
	for _, h := range s.hooks {
		if h.Event != PreTool || !h.matches(req.Tool) {
			continue
		}
		res, err := s.run(ctx, h, payload{
			Event:     PreTool,
			Tool:      req.Tool,
			Path:      req.Path,
			Argv:      req.Argv,
			Shell:     req.Shell,
			Detail:    req.Detail,
			Workspace: s.workspace,
		})
		if err != nil {
			return fmt.Sprintf("blocked by a pre_tool hook that failed to run: %v", err), true
		}
		if res.TimedOut {
			return fmt.Sprintf("blocked by a pre_tool hook that did not answer within %s", h.timeout()), true
		}
		if res.ExitCode != 0 {
			if res.Truncated {
				return "blocked by hook: output withheld because it exceeded the bounded capture", true
			}
			msg := strings.TrimSpace(res.Output)
			if msg == "" {
				msg = fmt.Sprintf("a pre_tool hook exited %d", res.ExitCode)
			}
			return "blocked by hook: " + msg, true
		}
	}
	return "", false
}

// PostTool runs every matching post_tool hook and returns their combined
// output for the tool result. A failure is reported the same way; nothing a
// post hook does can un-run the tool, so there is nothing to block.
func (s *Set) PostTool(ctx context.Context, req permission.Request, resultContent string, isError bool) string {
	if s.Empty() {
		return ""
	}
	excerpt := postToolResultExcerpt(resultContent)
	var notes []string
	for _, h := range s.hooks {
		if h.Event != PostTool || !h.matches(req.Tool) {
			continue
		}
		res, err := s.run(ctx, h, payload{
			Event:     PostTool,
			Tool:      req.Tool,
			Path:      req.Path,
			Argv:      req.Argv,
			Shell:     req.Shell,
			Detail:    req.Detail,
			Workspace: s.workspace,
			IsError:   isError,
			Result:    excerpt,
		})
		switch {
		case err != nil:
			notes = append(notes, fmt.Sprintf("[post_tool hook failed to run: %v]", err))
		case res.TimedOut:
			notes = append(notes, fmt.Sprintf("[post_tool hook did not finish within %s]", h.timeout()))
		case res.Truncated:
			notes = append(notes, "[post_tool hook output withheld because it exceeded the bounded capture]")
		case res.ExitCode != 0:
			notes = append(notes, fmt.Sprintf("[post_tool hook exited %d: %s]", res.ExitCode, strings.TrimSpace(res.Output)))
		case strings.TrimSpace(res.Output) != "":
			notes = append(notes, "[hook] "+strings.TrimSpace(res.Output))
		}
	}
	return strings.Join(notes, "\n")
}

// postToolResultExcerpt keeps the hook payload bounded without turning a
// recognized credential into an unrecognizable prefix. Complete credentials
// inside the bound stay intact for the common provider/session redaction gate;
// one crossing the bound is replaced before a hook can echo the fragment into
// the result that reaches that gate.
func postToolResultExcerpt(resultContent string) string {
	excerpt, _ := credential.SafePrefixForTruncation(resultContent, maxHookOutput)
	return excerpt
}

func (s *Set) run(ctx context.Context, h Hook, p payload) (execution.Result, error) {
	stdin, err := json.Marshal(p)
	if err != nil {
		return execution.Result{}, err
	}
	return execution.Run(ctx, execution.Command{
		Argv:      []string{h.Run},
		Shell:     true,
		Dir:       s.workspace,
		Timeout:   h.timeout(),
		MaxOutput: maxHookOutput,
		Stdin:     stdin,
		ExtraEnv: []string{
			"SB_HOOK_EVENT=" + string(p.Event),
			"SB_HOOK_TOOL=" + p.Tool,
			"SB_HOOK_PATH=" + p.Path,
			"SB_WORKSPACE=" + s.workspace,
		},
	})
}
