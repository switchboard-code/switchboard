package main

// /export and `sb export`: the session record as markdown — the timeline,
// not just the words, because route decisions and race verdicts are the
// half of the session no transcript of the words can reconstruct. The
// in-session form writes a file beside the work; this CLI form prints any
// recorded session to stdout, because the moment you want a session as a
// document — a PR description, a bug report, a CI artifact — is usually
// after the session closed, from a script, with a shell redirect already
// in hand. Every other record surface has both forms (/recap and sb
// recap, /blame and sb blame); this closes the family.

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/terminaltext"
)

// exportMarkdown renders one session's record. The timeline interleaves
// what was said with what was decided; a log that cannot be read as a
// timeline degrades to the messages alone rather than failing the export.
func exportMarkdown(state session.State, timeline []session.Timeline) string {
	if timeline == nil {
		for i := range state.Messages {
			timeline = append(timeline, session.Timeline{Message: &state.Messages[i]})
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Switchboard session %s\n\n", state.ID)
	fmt.Fprintf(&b, "- workspace: %s\n- target: %s\n- started: %s\n- exported: %s\n\n",
		state.Workspace, provider.DisplayRouteTargetID(provider.RouteTargetID(state.Target)), state.CreatedAt.Format(time.RFC3339), time.Now().Format(time.RFC3339))

	for _, ev := range timeline {
		switch {
		case ev.Message != nil:
			msg := ev.Message
			switch msg.Role {
			case provider.RoleUser:
				b.WriteString("## User\n\n")
			case provider.RoleAssistant:
				if msg.Incomplete {
					b.WriteString("## Assistant (interrupted)\n\n")
				} else {
					b.WriteString("## Assistant\n\n")
				}
			default:
				continue // tool results appear under the call that made them
			}
			for _, block := range msg.Content {
				switch blk := block.(type) {
				case provider.Text:
					b.WriteString(blk.Text + "\n\n")
				case provider.Thinking:
					// Deliberately omitted: an export is for sharing, and
					// thinking is a working draft, not the record.
				case provider.ToolUse:
					fmt.Fprintf(&b, "*[tool: %s]*\n\n", blk.Name)
				}
			}
		case ev.Route != nil:
			r := ev.Route
			line := fmt.Sprintf("> route: %s via %s (%s)", r.Tier, r.Source, r.Rationale)
			if r.Escalations > 0 && (r.EndedOn != "" || r.EndedTier != "") {
				ended := r.EndedTier
				if r.EndedOn != "" {
					target := provider.DisplayRouteTargetID(r.EndedOn)
					if ended == "" {
						ended = target
					} else {
						ended += " (" + target + ")"
					}
				}
				line += fmt.Sprintf("; %d escalation(s), ended on %s", r.Escalations, ended)
			}
			b.WriteString(line + "\n\n")
		case ev.Race != nil:
			r := ev.Race
			line := fmt.Sprintf("> race: %s vs %s — %s", r.A.Tier, r.B.Tier, r.Outcome)
			if r.Kept != "" {
				line += ", continued on " + r.Kept
			}
			b.WriteString(line + "\n\n")
		case ev.Note != nil && ev.Note.Level != "":
			fmt.Fprintf(&b, "> %s: %s\n\n", ev.Note.Level, ev.Note.Text)
		}
	}
	// An export is explicitly for sharing and may also be printed directly to
	// a terminal. Scan the complete artifact so credentials cannot straddle a
	// formatting boundary, then make control and bidi characters visible while
	// preserving the Markdown's intentional line structure.
	return terminaltext.Display(redactCredentialText(b.String()))
}

// runExportCLI prints a recorded session as markdown: the named one, or
// the workspace's most recent. Resolution mirrors sb recap's, because
// "which session" is the same question both commands answer.
func runExportCLI(w io.Writer, store *session.Store, workspace, id string) error {
	infos, err := store.List(workspace)
	if err != nil {
		return err
	}
	var info *session.Info
	for i := range infos {
		if id == "" || infos[i].ID == id {
			info = &infos[i]
			break
		}
	}
	if info == nil {
		if id != "" {
			return fmt.Errorf("no session %s recorded for this workspace; sb find searches what was said", id)
		}
		return fmt.Errorf("no session recorded for this workspace yet")
	}

	state, err := session.ReadState(info.Path)
	if err != nil {
		return err
	}
	timeline, err := session.ReadTimeline(info.Path)
	if err != nil {
		timeline = nil
	}
	_, err = io.WriteString(w, exportMarkdown(state, timeline))
	return err
}
