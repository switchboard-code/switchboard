package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/switchboard-code/switchboard/internal/hooks"
	"github.com/switchboard-code/switchboard/internal/trust"
)

// loadHooks assembles the session's hook set: the user's file always, the
// repository's only behind the trust grant, in that order so a home hook
// runs before the checkout's on the same event.
func loadHooks(workspace string, ts *trust.Store) (*hooks.Set, []mcpNote) {
	var notes []mcpNote
	var sets []*hooks.Set

	if home, err := os.UserHomeDir(); err == nil {
		s, err := hooks.LoadRooted(home, filepath.Join(".switchboard", hooks.FileName), workspace)
		if err != nil {
			notes = append(notes, mcpNote{"error", err.Error()})
		} else {
			sets = append(sets, s)
		}
	}

	repoPath := filepath.Join(workspace, ".switchboard", hooks.FileName)
	if _, err := os.Stat(repoPath); err == nil {
		if ts != nil && ts.Trusted(workspace) {
			s, err := hooks.LoadRooted(workspace, filepath.Join(".switchboard", hooks.FileName), workspace)
			if err != nil {
				notes = append(notes, mcpNote{"error", err.Error()})
			} else {
				sets = append(sets, s)
			}
		} else {
			notes = append(notes, mcpNote{"warn",
				"this repository declares hooks in .switchboard/hooks.toml; they stay off until you run /trust grant"})
		}
	}

	merged := hooks.Merge(workspace, sets...)
	if !merged.Empty() {
		notes = append(notes, mcpNote{"", fmt.Sprintf("hooks: %d loaded", len(merged.Hooks()))})
	}
	return merged, notes
}
