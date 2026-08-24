package session

// Read-only extraction of the file mutations a log records, for surfaces
// that answer "who wrote this line" rather than replay a conversation.
// A write tool call carries the file's complete new bytes and an edit
// carries its exact replacement, so the log already holds everything a
// line-level attribution needs — this reader just pairs each call with
// the records around it: the usage record that names the target which
// emitted it, the tool result that says whether it ran, and the turn's
// route record for the rung it was routed on.

import (
	"bufio"
	"encoding/json"
	"strings"
	"time"

	"github.com/switchboard-code/switchboard/internal/provider"
)

// FileEdit is one successful write or edit call replayed from a log, with
// the attribution the surrounding records carry.
type FileEdit struct {
	SessionID string
	Workspace string
	At        time.Time // when the tool result was recorded
	Turn      int       // 1-based user-turn ordinal within the session
	// Prompt is populated only from an exact durable authored projection.
	// PromptAuthoredKnown distinguishes an intentionally empty authored prompt
	// from legacy Content that may contain @file, shell, or harness expansion.
	Prompt              string
	PromptAuthoredKnown bool
	PromptSynthetic     bool

	// CallID is the tool-use id the provider stamped on the call. A fork
	// copies its source's records byte for byte, timestamps included, so
	// the same call can sit in two logs — and CallID with At is how a
	// reader tells a copy from a second real call, which never shares
	// both.
	CallID string

	// Target is the provider target that emitted the call, from the usage
	// record that follows the assistant message. Tier is the rung the
	// turn's route record names, and only when that record's target is the
	// same one — a call made after a mid-turn move has a target and no
	// rung, because the route record does not say which rung it moved to.
	Target string
	Tier   string

	Path string // as the tool call named it: absolute, or workspace-relative

	Write      bool   // full-content write rather than an edit
	Content    string // write: the file's complete new bytes
	Old, New   string // edit: the exact match and its replacement
	ReplaceAll bool

	// turnDepth is the message count before the turn's opening, the key a
	// route record carries; it exists only for the tier match above.
	turnDepth int
}

// writeInput and editInput mirror the write and edit tool schemas. The
// mirror is deliberate: importing the tools package for two field lists
// would give a log reader a dependency on the permission engine.
type writeInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type editInput struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// pendingCall is a write or edit tool use whose result has not been seen
// yet. Target stays empty until the usage record after its message names
// one; a call whose turn ended before a usage record — an interrupted
// message — never gets a result either, so it is dropped with the rest.
type pendingCall struct {
	id                  string
	name                string
	input               json.RawMessage
	turn                int
	turnDepth           int
	prompt              string
	promptAuthoredKnown bool
	promptSynthetic     bool
	target              string
}

// ReadFileEdits replays a log and returns its successful file mutations in
// record order. A failed or denied call is dropped — the file never changed —
// and an incomplete final frame ends the read where every other reader stops.
// A complete corrupt frame is an error rather than a silently shortened
// provenance history. Mutations a shell command made are not here and cannot
// be: only write and edit put their bytes in the record.
func ReadFileEdits(path string) ([]FileEdit, error) {
	f, err := openPublishedLog(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	if err := checkHeader(r, path); err != nil {
		return nil, err
	}
	return readFileEditRecords(r)
}

// readFileEditRecords is the replay after the header: separate so the
// fuzz target can feed it arbitrary bytes, because a tampered or
// crash-truncated log must come back as an error or a short read, never
// a panic — the same bar the record decoder holds.
func readFileEditRecords(r *bufio.Reader) ([]FileEdit, error) {
	var (
		out                 []FileEdit
		sessionID           string
		workspace           string
		messages            int
		turn                int
		turnDepth           int
		prompt              string
		promptAuthoredKnown bool
		promptSynthetic     bool
		routes              = map[int]Route{} // keyed by TurnDepth, the one exact key a route record carries
		pending             = map[string]*pendingCall{}
		awaiting            []*pendingCall // calls whose usage record has not arrived
	)

	lastSeq := 0
	budget := newReplayBudget(defaultReplayLimits, 0)
	for {
		rec, _, err := budget.decode(r, &lastSeq)
		if isRecoverableRecordEnd(err) {
			break
		}
		if err != nil {
			return nil, err
		}
		m, isMessage, err := conversationMessage(rec)
		if err != nil {
			return nil, err
		}
		if isMessage {
			if OpensTurn(m) {
				turn++
				turnDepth = messages
				prompt, promptAuthoredKnown, promptSynthetic = authoredTurnPrompt(m)
			}
			messages++
			if m.Role == provider.RoleAssistant {
				// A new assistant message before the previous one's usage
				// record means that one was interrupted; its calls never
				// ran and their targets stay unknown.
				awaiting = awaiting[:0]
				for _, use := range m.ToolUses() {
					if use.Name != "write" && use.Name != "edit" {
						continue
					}
					call := &pendingCall{
						id:                  use.ID,
						name:                use.Name,
						input:               use.Input,
						turn:                turn,
						turnDepth:           turnDepth,
						prompt:              prompt,
						promptAuthoredKnown: promptAuthoredKnown,
						promptSynthetic:     promptSynthetic,
					}
					pending[use.ID] = call
					awaiting = append(awaiting, call)
				}
				continue
			}
			for _, block := range m.Content {
				res, ok := block.(provider.ToolResult)
				if !ok {
					continue
				}
				call, ok := pending[res.ToolUseID]
				if !ok {
					continue
				}
				delete(pending, res.ToolUseID)
				if res.IsError {
					continue
				}
				if edit, ok := call.fileEdit(rec.At, sessionID, workspace); ok {
					out = append(out, edit)
				}
			}
			continue
		}
		switch rec.Type {
		case RecordSessionStart:
			var start SessionStart
			if err := json.Unmarshal(rec.Payload, &start); err != nil {
				return nil, err
			}
			sessionID, workspace = start.ID, start.Workspace
		case RecordUsage:
			var u Usage
			if err := json.Unmarshal(rec.Payload, &u); err != nil {
				return nil, err
			}
			for _, call := range awaiting {
				call.target = u.Target
			}
			awaiting = nil
		case RecordRoute:
			var route Route
			if err := json.Unmarshal(rec.Payload, &route); err != nil {
				return nil, err
			}
			routes[route.TurnDepth] = route
		}
	}

	for i := range out {
		route, ok := routes[out[i].turnDepth]
		if ok && string(route.Target) == out[i].Target {
			out[i].Tier = route.Tier
		}
	}
	return out, nil
}

// fileEdit converts a completed call into the record shape, reporting false
// when the input does not parse — a malformed input that somehow succeeded
// is not something replay can honestly use.
func (c *pendingCall) fileEdit(at time.Time, sessionID, workspace string) (FileEdit, bool) {
	edit := FileEdit{
		SessionID:           sessionID,
		Workspace:           workspace,
		At:                  at,
		Turn:                c.turn,
		Prompt:              c.prompt,
		PromptAuthoredKnown: c.promptAuthoredKnown,
		PromptSynthetic:     c.promptSynthetic,
		CallID:              c.id,
		Target:              c.target,
		turnDepth:           c.turnDepth,
	}
	switch c.name {
	case "write":
		var in writeInput
		if err := json.Unmarshal(c.input, &in); err != nil || in.Path == "" {
			return FileEdit{}, false
		}
		edit.Path, edit.Write, edit.Content = in.Path, true, in.Content
	case "edit":
		var in editInput
		if err := json.Unmarshal(c.input, &in); err != nil || in.Path == "" || in.OldString == "" {
			return FileEdit{}, false
		}
		edit.Path, edit.Old, edit.New, edit.ReplaceAll = in.Path, in.OldString, in.NewString, in.ReplaceAll
	default:
		return FileEdit{}, false
	}
	return edit, true
}

// authoredTurnPrompt extracts only the durable user-visible projection. The
// legacy AuthoredText fallback is intentionally forbidden here: Content may
// include file bytes, shell output, continuity context, or injected evidence.
func authoredTurnPrompt(message provider.Message) (text string, known, synthetic bool) {
	if message.Synthetic {
		return "", false, true
	}
	text, known = message.AuthoredProjection()
	if !known {
		return "", false, false
	}
	return strings.TrimSpace(text), true, false
}

// OpensTurn reports whether a message is a user turn's opening: user-role,
// not injected mid-turn, and not a tool-result carrier. An image-only
// opening still opens a turn; its prompt is just empty.
func OpensTurn(m provider.Message) bool {
	if m.Role != provider.RoleUser || m.Injected {
		return false
	}
	for _, block := range m.Content {
		switch block.(type) {
		case provider.ToolResult, *provider.ToolResult:
			return false
		}
	}
	return true
}

// OpensUserTurn is the authored/user-facing subset of OpensTurn. Synthetic
// openings still delimit a real model turn for replay, continuity delivery,
// cost attribution, and edit provenance, but they must not be presented or
// counted as words the user supplied.
func OpensUserTurn(m provider.Message) bool {
	return OpensTurn(m) && !m.Synthetic
}
