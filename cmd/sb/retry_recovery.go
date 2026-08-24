package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/session"
)

// recoverInterruptedRetry runs before session discovery or adoption. A valid
// publication marker commits the restored pre-images; every other valid
// staged state rolls the workspace back to the source session's post-images.
func recoverInterruptedRetry(store *session.Store, workspace string) (checkpoint.DurableUndoRecovery, error) {
	var none checkpoint.DurableUndoRecovery
	if store == nil {
		return none, errors.New("retry recovery has no session store")
	}
	dir, err := store.WorkspaceDir(workspace)
	if err != nil {
		return none, fmt.Errorf("opening the workspace session directory: %w", err)
	}
	recovery, err := checkpoint.RecoverDurableUndo(dir, workspace, func(path, identity string) (bool, error) {
		published, err := session.EnsurePublicationDurableExpected(path, identity)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return published, err
	})
	if err != nil {
		return recovery, fmt.Errorf("recovering an interrupted /retry before session adoption: %w", err)
	}
	return recovery, nil
}

func interruptedRetryRecoveryNotice(recovery checkpoint.DurableUndoRecovery) string {
	if !recovery.Found {
		return ""
	}
	var notice string
	if recovery.Published {
		notice = "recovered an interrupted /retry after its child was published; kept the committed pre-turn workspace"
		if recovery.CleanupWarning != nil {
			notice += "; the resolved recovery journal could not be cleared: " + recovery.CleanupWarning.Error()
		}
		return notice
	}
	total := recovery.RolledForward + recovery.AlreadyPost
	if total == 0 {
		notice = "recovered an interrupted /retry before publication; no checkpointed files needed repair"
	} else {
		notice = fmt.Sprintf(
			"recovered an interrupted /retry before publication; returned %d checkpointed file(s) to the source session's post-turn workspace (%d repaired, %d already correct)",
			total, recovery.RolledForward, recovery.AlreadyPost,
		)
	}
	if recovery.CleanupWarning != nil {
		notice += "; the resolved recovery journal could not be cleared: " + recovery.CleanupWarning.Error()
	}
	return notice
}

func constrainUnresolvedRetryStartup(opts *options, id string, status session.RetryIntentStatus, interactive bool) (string, error) {
	if opts == nil {
		return "", errors.New("retry startup has no options")
	}
	if !interactive {
		return "", fmt.Errorf("session %s has an unresolved /retry handoff (%s); reopen this workspace in the interactive TUI to recover it without duplicating provider or tool work", id, status)
	}
	if opts.resume != "" && opts.resume != id {
		return "", fmt.Errorf("session %s has an unresolved /retry handoff (%s) and still governs this workspace; resume that child first and use /retry abandon before choosing session %s", id, status, opts.resume)
	}
	if opts.resume == "" && !opts.cont {
		opts.resume = id
		return fmt.Sprintf("reopening retry child %s because its %s execution handoff still governs this workspace", id, status), nil
	}
	return "", nil
}
