package tools

import (
	"encoding/json"
	"errors"
)

// Branch clones a registry for a context that branched off this one — a
// /race arm running on a fork of the session (§12). Two properties have to
// hold at once, and each rules out a simpler design.
//
// The definitions must render byte-identical to the source registry's,
// because tool schemas sit in the frozen zone (§6.1): a fork's messages are
// byte-identical to the prefix a provider may still hold warm, and a branch
// that dropped or narrowed a tool would change the request bytes ahead of
// them, going cold to save nothing. So Branch never restricts; a tool the
// branch must not run keeps its schema and refuses at call time instead,
// through the refuse map (tool name to the reason the model reads).
//
// File-read state starts empty. A read tool updates its registry before the
// matching ToolResult is durable, so the registry alone cannot prove those
// bytes reached the exact session prefix being forked. Rereading in a branch
// is conservative and keeps both read-before-write and the §6.7 reinjection
// skip honest even when a fork races the result append.
func (r *Registry) Branch(refuse map[string]string) *Registry {
	nr := &Registry{
		root:        r.root,
		displayRoot: r.displayRoot,
		rootInfo:    r.rootInfo,
		displayPath: map[string]string{},
		capability:  r.capability,
		execution:   r.execution,
		versions:    newFileVersions(),
		// A process started under a branch is still this program's to stop, so
		// the set is shared rather than copied. The branch's own refuse map is
		// what keeps a read-only arm from starting one.
		background: r.background,
		// A branch is read-only, so it produces none of its own; sharing keeps
		// a picture an arm somehow queued from being delivered into the primary.
		images: &toolImages{},
		todos:  &todoState{},
		tools:  map[string]Tool{},
		// No checkpointer: a branch is read-only by policy, and an undo
		// scope for turns that mutate nothing would file empty checkpoints
		// under a session /undo never sees.
	}
	for _, name := range r.order {
		tool := r.tools[name]
		switch name {
		// The core tools hold a pointer to their registry, so each branch
		// needs its own instances or every branch read would arm the
		// source's state. Everything else — astgrep, skill, the LSP pair,
		// bridged MCP tools, delegate — carries no per-context read state,
		// so the instance is shared and the definition bytes with it.
		case "read":
			tool = &readTool{nr}
		case "write":
			tool = &writeTool{nr}
		case "edit":
			tool = &editTool{nr}
		case "exec":
			tool = &execTool{nr}
		case "glob":
			tool = &globTool{nr}
		case "grep":
			tool = &grepTool{nr}
		case "todo":
			tool = &todoTool{nr}
		case "ask":
			// The branch registry never gets a questioner: a branch runs
			// unattended beside its rival, and a question one arm asked
			// would make the user the difference between two runs that
			// exist to be compared. The caller's refuse map names the
			// better reason; the absent questioner is what holds if it
			// does not.
			tool = &askTool{nr}
		}
		if reason, ok := refuse[name]; ok {
			tool = &refusedTool{Tool: tool, reason: reason}
		}
		nr.add(tool)
	}
	return nr
}

// refusedTool keeps a tool's schema while refusing to run it. The schema is
// the frozen-zone obligation; the refusal happens at Plan, so the reason
// goes back to the model as a tool error and the permission engine is never
// asked to record an answer for a call that was going nowhere.
type refusedTool struct {
	Tool
	reason string
}

func (t *refusedTool) Plan(json.RawMessage) (Plan, error) {
	return Plan{}, errors.New(t.reason)
}
