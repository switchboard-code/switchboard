package main

import (
	"fmt"

	"github.com/switchboard-code/switchboard/internal/schedule"
	"github.com/switchboard-code/switchboard/internal/session"
)

// openWorkspaceSchedule discovers historical per-workspace directories from
// validated session identities, then asks the schedule package to re-prove
// each source under every participating ledger lock before moving anything.
func openWorkspaceSchedule(store *session.Store, workspace string) (*schedule.Store, error) {
	dirs, err := store.WorkspaceStateDirs(workspace)
	if err != nil {
		return nil, err
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("session store returned no state directory for workspace %q", workspace)
	}
	return schedule.OpenMigrating(dirs[0], dirs[1:], func(historicalDir string) error {
		return store.ValidateWorkspaceStateDir(workspace, historicalDir)
	})
}
